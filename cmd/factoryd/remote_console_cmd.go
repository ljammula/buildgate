package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"buildgate/internal/consolelink"
	"buildgate/internal/hostcontrol"
	"buildgate/internal/sessionconfig"
)

// remoteConsoleFileSuffix names the file that records how this profile's
// console is reached from another machine: <config name>.remote-console
// beside the session config, mode 0600, with the checks of the token files.
// `factoryd remote-console` writes it. `serve` reads it on each request whose
// Host is not otherwise accepted and allows the host it names, so turning the
// remote console on or off needs no restart and survives one.
//
// Four lines, each once: "host <name[:port]>" (the Host header the other
// machine sends), "listener <https port>" (the Tailscale listener), "backend
// <addr>" (the serve address the listener forwards to) and "gate-token
// written|kept" (whether this command wrote the profile's gate token, which
// is what -off may then remove).
const remoteConsoleFileSuffix = ".remote-console"

// tailscaleTimeout bounds one call of the tailscale CLI: it can wait on the
// operator (a tailnet with Serve not enabled prints a link and blocks).
const tailscaleTimeout = 30 * time.Second

// remoteConsolePorts are the HTTPS ports tried, in order, for a listener
// when -https-port names none.
var remoteConsolePorts = []int{443, 8443, 8444, 8445, 8446, 8447, 8448, 8449}

// remoteConsoleServeWait is how long a serve this command started is given
// to record its address.
const remoteConsoleServeWait = 20 * time.Second

func remoteConsolePathFor(configPath string) string {
	name := filepath.Base(configPath)
	name = strings.TrimSuffix(name, filepath.Ext(name))
	return filepath.Join(filepath.Dir(configPath), name+remoteConsoleFileSuffix)
}

// remoteConsole is what the file at a path says.
type remoteConsole struct {
	Host     string
	Listener int
	Backend  string
	// WroteToken is whether this command wrote the profile's gate token.
	WroteToken bool
}

func (rc remoteConsole) content() string {
	token := "kept"
	if rc.WroteToken {
		token = "written"
	}
	return fmt.Sprintf("host %s\nlistener %d\nbackend %s\ngate-token %s\n", rc.Host, rc.Listener, rc.Backend, token)
}

// readRemoteConsole reads the file at path. ok is false when it is absent,
// fails a check of inspectServeStartTokenFile, or does not hold exactly its
// four well-formed lines: nothing is allowed on the word of a file `serve`
// cannot trust.
func readRemoteConsole(path string) (rc remoteConsole, ok bool) {
	content, issue, err := inspectServeStartTokenFile(path)
	if err != nil || issue != nil || content == "" {
		return remoteConsole{}, false
	}
	fields := map[string]string{}
	for _, line := range strings.Split(content, "\n") {
		key, value, found := strings.Cut(strings.TrimSpace(line), " ")
		value = strings.TrimSpace(value)
		if _, twice := fields[key]; twice || !found || value == "" || strings.ContainsAny(value, " \t") {
			return remoteConsole{}, false
		}
		fields[key] = value
	}
	token := fields["gate-token"]
	if len(fields) != 4 || (token != "written" && token != "kept") {
		return remoteConsole{}, false
	}
	listener, err := strconv.Atoi(fields["listener"])
	rc = remoteConsole{Host: fields["host"], Listener: listener, Backend: fields["backend"], WroteToken: token == "written"}
	if err != nil || !validRemoteHost(rc.Host) || rc.Listener <= 0 || rc.Listener > 65535 || !validBackend(rc.Backend) {
		return remoteConsole{}, false
	}
	return rc, true
}

// validBackend reports whether addr is a loopback host:port, the only kind
// of address a profile's serve records.
func validBackend(addr string) bool {
	host, port, err := net.SplitHostPort(addr)
	n, convErr := strconv.Atoi(port)
	return err == nil && convErr == nil && n > 0 && n <= 65535 && (host == "127.0.0.1" || host == "localhost" || host == "::1")
}

// validRemoteHost reports whether host is a DNS name with an optional port:
// what a Host header carries, and nothing a URL or a path would add. An IP
// address and "localhost" are refused: the remote address is a name.
func validRemoteHost(host string) bool {
	name, port, err := net.SplitHostPort(host)
	if err != nil {
		name, port = host, ""
	}
	if port != "" {
		if n, err := strconv.Atoi(port); err != nil || n <= 0 || n > 65535 {
			return false
		}
	}
	if name == "" || len(name) > 253 || strings.HasPrefix(name, ".") || strings.HasSuffix(name, ".") {
		return false
	}
	// The machine's own loopback names are never a remote address.
	if name == "localhost" || net.ParseIP(name) != nil || (err == nil && port == "") {
		return false
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '.') {
			return false
		}
	}
	return true
}

// remoteConsoleHostSource is the api.WithAllowedHostSource source for the
// file at path: the host it names, or none.
func remoteConsoleHostSource(path string) func() []string {
	return func() []string {
		if rc, ok := readRemoteConsole(path); ok {
			return []string{rc.Host}
		}
		return nil
	}
}

// tailscale runs the operator's tailscale CLI and returns its standard
// output; what it wrote to standard error is in the error. It gives up after
// tailscaleTimeout.
func (impl realHost) tailscale(ctx context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, tailscaleTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "tailscale", args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	switch {
	case ctx.Err() != nil:
		return out, fmt.Errorf("no answer within %s: %s", tailscaleTimeout, hostcontrol.LastLine(string(out)+stderr.String()))
	case err != nil:
		return out, fmt.Errorf("%w: %s", err, hostcontrol.LastLine(stderr.String()))
	}
	return out, nil
}

// tailscaleStatus is the part of `tailscale status --json` this command reads.
type tailscaleStatus struct {
	BackendState string
	Self         struct{ DNSName string }
}

// tailscaleServeConfig is the part of `tailscale serve status --json` this
// command reads: per "host:port", the proxy target of each path.
type tailscaleServeConfig struct {
	// TCP is keyed by port: every port the node listens on, web or not.
	TCP map[string]json.RawMessage
	Web map[string]struct {
		Handlers map[string]struct{ Proxy string }
	}
	// AllowFunnel is keyed by "host:port": the ports open to the internet.
	AllowFunnel map[string]bool
}

// listenerFunnel marks, in tailscaleListeners' result, a port Funnel opens
// to the internet: never one this command uses, whatever it forwards to.
const listenerFunnel = "funnel"

// tailscaleName is this machine's name on its tailnet.
func tailscaleName(ctx context.Context, dp *deps) (string, error) {
	const install = "`factoryd remote-console` puts the console behind `tailscale serve`: install Tailscale and sign in, or follow USAGE.md's \"The console from another machine\" for another reverse proxy"
	out, err := dp.host.tailscale(ctx, "status", "--json")
	if err != nil {
		return "", fmt.Errorf("tailscale status failed (%s) -- %s", sanitizeFirstLine(out, err), install)
	}
	var status tailscaleStatus
	if err := json.Unmarshal(out, &status); err != nil {
		return "", fmt.Errorf("tailscale status printed no JSON this command can read: %w", err)
	}
	name := strings.TrimSuffix(strings.ToLower(status.Self.DNSName), ".")
	if status.BackendState != "Running" || !validRemoteHost(name) {
		return "", fmt.Errorf("Tailscale is not connected (state %q) -- run `tailscale up`, then this command again", status.BackendState)
	}
	return name, nil
}

func sanitizeFirstLine(out []byte, err error) string {
	line := hostcontrol.LastLine(string(out))
	if line == "" {
		line = err.Error()
	}
	return line
}

// tailscaleListeners returns, per port this node listens on, the loopback
// address the "/" of name forwards to there (host:port, as a serve records
// it). "" is a port that serves something else (other paths, another name,
// plain TCP, a target that is not a loopback address) and listenerFunnel one
// open to the internet.
func tailscaleListeners(ctx context.Context, dp *deps, name string) (map[int]string, error) {
	out, err := dp.host.tailscale(ctx, "serve", "status", "--json")
	if err != nil {
		return nil, fmt.Errorf("tailscale serve status failed: %s", sanitizeFirstLine(out, err))
	}
	var config tailscaleServeConfig
	if len(strings.TrimSpace(string(out))) > 0 {
		if err := json.Unmarshal(out, &config); err != nil {
			return nil, fmt.Errorf("tailscale serve status printed no JSON this command can read: %w", err)
		}
	}
	listeners := map[int]string{}
	for portText := range config.TCP {
		if port, err := strconv.Atoi(portText); err == nil {
			listeners[port] = ""
		}
	}
	for hostPort, web := range config.Web {
		host, portText, err := net.SplitHostPort(hostPort)
		port, convErr := strconv.Atoi(portText)
		if err != nil || convErr != nil {
			continue
		}
		listeners[port] = ""
		if strings.EqualFold(strings.TrimSuffix(host, "."), name) && len(web.Handlers) == 1 {
			listeners[port] = proxyBackend(web.Handlers["/"].Proxy)
		}
	}
	for hostPort, open := range config.AllowFunnel {
		if _, portText, err := net.SplitHostPort(hostPort); err == nil && open {
			if port, err := strconv.Atoi(portText); err == nil {
				listeners[port] = listenerFunnel
			}
		}
	}
	return listeners, nil
}

// proxyBackend is the loopback host:port a proxy target names, with
// localhost written as 127.0.0.1, or "" for any other target.
func proxyBackend(target string) string {
	addr, isHTTP := strings.CutPrefix(strings.TrimSuffix(target, "/"), "http://")
	host, port, err := net.SplitHostPort(addr)
	if !isHTTP || err != nil || !validBackend(addr) {
		return ""
	}
	if host == "localhost" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

// chooseRemotePort picks the HTTPS port of the listener for backend. A port
// is this console's when it forwards "/" to backend, or is the port previous
// recorded and still forwards to the backend previous recorded (the serve
// has moved since): such a port is kept, so the address does not change.
// Otherwise wanted when given, else the first of remoteConsolePorts nothing
// listens on. It never takes a port that serves something else.
func chooseRemotePort(listeners map[int]string, backend string, wanted int, previous remoteConsole) (int, error) {
	ours := func(port int) bool {
		target, listening := listeners[port]
		return listening && target != "" && (target == backend || (port == previous.Listener && target == previous.Backend))
	}
	if wanted != 0 {
		if target, taken := listeners[wanted]; taken && !ours(wanted) {
			return 0, fmt.Errorf("port %d already serves %s on this machine's tailnet name; choose another -https-port, or run without it for a free one", wanted, describeProxy(target))
		}
		return wanted, nil
	}
	if ours(previous.Listener) {
		return previous.Listener, nil
	}
	ports := make([]int, 0, len(listeners))
	for port := range listeners {
		ports = append(ports, port)
	}
	sort.Ints(ports)
	for _, port := range ports {
		if ours(port) {
			return port, nil
		}
	}
	for _, port := range remoteConsolePorts {
		if _, taken := listeners[port]; !taken {
			return port, nil
		}
	}
	return 0, fmt.Errorf("every port this command tries (%v) already serves something on this machine's tailnet name; pass a free one with -https-port", remoteConsolePorts)
}

func describeProxy(target string) string {
	switch target {
	case "":
		return "something else"
	case listenerFunnel:
		return "a Funnel listener, open to the internet,"
	}
	return "http://" + target
}

// remoteHostFor is the Host header a browser sends for name on port.
func remoteHostFor(name string, port int) string {
	if port == 443 {
		return name
	}
	return net.JoinHostPort(name, strconv.Itoa(port))
}

type remoteConsoleFlags struct {
	configPath *string
	dataDir    *string
	httpsPort  *int
	ttl        *time.Duration
	off        *bool
}

// newRemoteConsoleFlags builds `factoryd remote-console`'s FlagSet in
// isolation from parsing, so the doc-vs-flag drift test can enumerate it.
func newRemoteConsoleFlags() (flags *flag.FlagSet, f remoteConsoleFlags) {
	flags = flag.NewFlagSet("remote-console", flag.ContinueOnError)
	f.configPath = flags.String("config", "", "session config path, which locates the files beside it; default: the first of "+strings.Join(sessionconfig.DefaultPaths(), ", ")+" that exists")
	f.dataDir = flags.String("data-dir", "", "data directory of the console to open; default: the session config's data dir")
	f.httpsPort = flags.Int("https-port", 0, "HTTPS port of the Tailscale listener; default: the one already forwarding to this console, else the first free of 443, 8443, 8444 and up")
	f.ttl = flags.Duration("ttl", gateDefaultTTL, fmt.Sprintf("how long a gate token this command writes works, from %s to %s; a usable token is kept as it is", gateMinTTL, gateMaxTTL))
	f.off = flags.Bool("off", false, "close the console to other machines: remove the allowed host, the listener this command added, and the gate token if this command wrote it")
	plainFlagUsage(flags)
	return
}

// remoteConsoleMain implements `factoryd remote-console`: one command that
// makes this profile's console usable from another machine on the tailnet
// (a listener, the host `serve` accepts, a gate token), or with -off undoes
// all three.
func remoteConsoleMain(dp *deps, stdout io.Writer, args []string) error {
	flags, f := newRemoteConsoleFlags()
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected positional argument %q", flags.Arg(0))
	}
	if *f.ttl < gateMinTTL || *f.ttl > gateMaxTTL {
		return fmt.Errorf("-ttl %s is outside %s to %s", *f.ttl, gateMinTTL, gateMaxTTL)
	}
	if *f.httpsPort < 0 || *f.httpsPort > 65535 {
		return fmt.Errorf("-https-port %d is not a port", *f.httpsPort)
	}
	configPath, found := resolveEffectiveConfigPath(*f.configPath)
	if !found {
		return errors.New("no session config found: run `factoryd setup` first; the remote console's files live beside it")
	}
	if _, err := os.Stat(configPath); err != nil {
		return fmt.Errorf("session config %s: %w", configPath, err)
	}
	ctx := context.Background()
	if *f.off {
		return remoteConsoleOff(ctx, dp, stdout, configPath)
	}
	dataDir := resolveConfiguredDataDir(*f.dataDir != "", *f.dataDir, *f.configPath)
	if dataDir == "" {
		dataDir = "data"
	}
	if abs, err := filepath.Abs(dataDir); err == nil {
		dataDir = abs
	}
	return remoteConsoleOn(ctx, dp, stdout, configPath, dataDir, *f.httpsPort, *f.ttl)
}

// remoteConsoleOn does the steps in the order that leaves nothing half open:
// the gate token exists before anything forwards, and the listener is in
// place before the host is recorded (until then `serve` refuses the name).
// A step that fails undoes the one before it.
func remoteConsoleOn(ctx context.Context, dp *deps, stdout io.Writer, configPath, dataDir string, wantedPort int, ttl time.Duration) error {
	name, err := tailscaleName(ctx, dp)
	if err != nil {
		return err
	}
	backend, err := remoteConsoleBackend(dp, stdout, configPath, dataDir)
	if err != nil {
		return err
	}
	listeners, err := tailscaleListeners(ctx, dp, name)
	if err != nil {
		return err
	}
	path := remoteConsolePathFor(configPath)
	previous, _ := readRemoteConsole(path)
	port, err := chooseRemotePort(listeners, backend, wantedPort, previous)
	if err != nil {
		return err
	}

	gatePath := gateTokenPathFor(configPath)
	hadToken := readGateFile(gatePath, time.Now()).token != ""
	token, err := ensureGateToken(dp, gatePath, ttl, time.Now())
	if err != nil {
		return err
	}
	next := remoteConsole{Host: remoteHostFor(name, port), Listener: port, Backend: backend, WroteToken: !hadToken || previous.WroteToken}
	added := listeners[port] != backend
	if added {
		if out, err := dp.host.tailscale(ctx, "serve", "--bg", "--https="+strconv.Itoa(port), "http://"+backend); err != nil {
			return fmt.Errorf("tailscale serve could not add the listener on port %d (%s); nothing else was changed", port, sanitizeFirstLine(out, err))
		}
	}
	if err := writeGateFile(dp, path, next.content()); err != nil {
		if added && listeners[port] == "" {
			_, _ = dp.host.tailscale(ctx, "serve", "--https="+strconv.Itoa(port), "off")
		}
		return err
	}
	// The listener this command recorded earlier, on another port, is
	// removed only while it still forwards where this command pointed it.
	if previous.Listener != 0 && previous.Listener != port && listeners[previous.Listener] == previous.Backend {
		if _, err := dp.host.tailscale(ctx, "serve", "--https="+strconv.Itoa(previous.Listener), "off"); err != nil {
			fmt.Fprintf(stdout, "The earlier listener on port %d could not be removed: run `tailscale serve --https=%d off`.\n", previous.Listener, previous.Listener)
		}
	}

	fmt.Fprintf(stdout, "Console:    https://%s/\n", next.Host)
	fmt.Fprintf(stdout, "Gate token: %s\n", token.token)
	fmt.Fprintf(stdout, "Expires:    %s (in %s)\n", token.expires.Local().Format(time.RFC3339), time.Until(token.expires).Round(time.Minute))
	fmt.Fprintln(stdout, "\nOn the other machine, open the console address and paste the gate token into the field it shows.")
	fmt.Fprintln(stdout, "The token lets that console read, and approve, reject, retry, resume, cancel, edit and submit requests. It cannot override a quarantined run, start a run or use /mcp.")
	fmt.Fprintln(stdout, "Any device your tailnet lets reach this machine can open the address; without the token it reads and writes nothing.")
	fmt.Fprintln(stdout, "`factoryd gate-token -rotate` replaces the token at once. `factoryd remote-console -off` closes the console to other machines.")
	return nil
}

// remoteConsoleBackend is the address of dataDir's own serve, started here
// when none runs and FACTORYD_AUTOSTART allows it.
func remoteConsoleBackend(dp *deps, stdout io.Writer, configPath, dataDir string) (string, error) {
	if addr := consolelink.ServeAddress(dataDir); addr != "" {
		return proxyBackend("http://" + addr), nil
	}
	notRunning := fmt.Errorf("no `factoryd serve` answers for data dir %s: start it with `factoryd console`, then run this command again", dataDir)
	if !hostcontrol.AutostartEnabled() {
		return "", notRunning
	}
	binary, err := dp.host.executable()
	if err != nil {
		return "", fmt.Errorf("find this binary: %w", err)
	}
	hostcontrol.QuickstartEnsureServe(dp, false, stdout, binary, configPath, dataDir)
	for waited := time.Duration(0); waited < remoteConsoleServeWait; waited += 500 * time.Millisecond {
		if addr := consolelink.ServeAddress(dataDir); addr != "" {
			return proxyBackend("http://" + addr), nil
		}
		dp.host.sleep(500 * time.Millisecond)
	}
	return "", notRunning
}

// remoteConsoleOff removes what this command put in place: the allowed host
// always, the gate token when this command wrote it, and the listener while
// it still forwards where this command pointed it.
func remoteConsoleOff(ctx context.Context, dp *deps, stdout io.Writer, configPath string) error {
	path := remoteConsolePathFor(configPath)
	rc, ok := readRemoteConsole(path)
	// The host first: from the next request `serve` refuses the name, whether
	// or not the listener can be removed.
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", path, err)
	}
	if !ok {
		fmt.Fprintln(stdout, "Remote console off: this profile had none recorded. A gate token, if any, is left: `factoryd gate-token -remove` removes it.")
		return nil
	}
	did := []string{"the allowed host " + rc.Host}
	note := ""
	if rc.WroteToken {
		gatePath := gateTokenPathFor(configPath)
		if err := os.Remove(gatePath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("the allowed host is removed, but not the gate token: remove %s: %w", gatePath, err)
		}
		did = append(did, "the gate token")
	} else {
		note = " The gate token was there before this command and is left: `factoryd gate-token -remove` removes it."
	}
	if err := remoteConsoleRemoveListener(ctx, dp, rc); err != nil {
		return fmt.Errorf("removed %s, but not the listener on port %d: %w", strings.Join(did, " and "), rc.Listener, err)
	}
	fmt.Fprintf(stdout, "Remote console off: removed the listener on port %d, %s.%s\n", rc.Listener, strings.Join(did, " and "), note)
	return nil
}

// remoteConsoleRemoveListener turns off the listener rc recorded, unless
// that port has since been pointed somewhere else by hand.
func remoteConsoleRemoveListener(ctx context.Context, dp *deps, rc remoteConsole) error {
	manual := fmt.Sprintf("run `tailscale serve --https=%d off` if it still forwards to %s", rc.Listener, rc.Backend)
	name, err := tailscaleName(ctx, dp)
	if err != nil {
		return fmt.Errorf("%w; %s", err, manual)
	}
	listeners, err := tailscaleListeners(ctx, dp, name)
	if err != nil {
		return fmt.Errorf("%w; %s", err, manual)
	}
	target, listening := listeners[rc.Listener]
	if !listening {
		return nil
	}
	if target != rc.Backend {
		return fmt.Errorf("it now serves %s, which this command did not set up, and is left alone", describeProxy(target))
	}
	if out, err := dp.host.tailscale(ctx, "serve", "--https="+strconv.Itoa(rc.Listener), "off"); err != nil {
		return fmt.Errorf("%s; %s", sanitizeFirstLine(out, err), manual)
	}
	return nil
}

// doctorRemoteConsoleChecks is the row for a remote console whose listener
// forwards to an address this data dir's serve no longer has: the other
// machine reaches nothing, or another process. No row when the remote
// console is off or its listener matches.
func doctorRemoteConsoleChecks(configPath, dataDir string) []doctorCheck {
	configPath, found := resolveEffectiveConfigPath(configPath)
	if !found || dataDir == "" {
		return nil
	}
	rc, ok := readRemoteConsole(remoteConsolePathFor(configPath))
	if !ok {
		return nil
	}
	current := consolelink.ServeAddress(dataDir)
	if current != "" && proxyBackend("http://"+current) == rc.Backend {
		return nil
	}
	state := "no serve is running for this data dir"
	if current != "" {
		state = "this data dir's serve now listens on " + current
	}
	return []doctorCheck{{
		Name:     "remote console",
		Advisory: true,
		Err:      fmt.Errorf("the listener for %s forwards to %s, and %s", rc.Host, rc.Backend, state),
		Fix:      "`factoryd remote-console` points the listener at the running serve (it starts one when none runs); `factoryd remote-console -off` closes the console to other machines",
	}}
}
