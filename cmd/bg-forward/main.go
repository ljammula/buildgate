// bg-forward makes a sandboxed worker's localhost reach the run's Compose
// sidecars at the ports the target's compose file publishes, so a test that
// defaults to what `docker compose up` gives a developer (localhost:5433 for
// "5433:5432") works unchanged.
//
// Usage: bg-forward -- <command> [args...]
//
// It reads BG_COMPOSE_FORWARDS ("<port>=<service>:<port>,...", written by
// internal/sandbox's ComposeServicesLifecycle.ApplyToWorkerLaunch), binds
// 127.0.0.1 and ::1 at each port before starting the command, runs the
// command as its child, and exits with the command's status. It only dials
// the named services, which the worker can already reach by name on the
// run's internal Compose network: it adds no route the worker lacks.
package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Forward is one localhost port and the service address it forwards to.
type Forward struct {
	Port   int
	Target string // "<service>:<port>"
}

func main() {
	os.Exit(run(os.Args[1:], os.Getenv("BG_COMPOSE_FORWARDS")))
}

func run(args []string, spec string) int {
	if len(args) < 2 || args[0] != "--" {
		fmt.Fprintln(os.Stderr, "usage: bg-forward -- <command> [args...]")
		return 2
	}
	forwards, err := ParseForwards(spec)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bg-forward: BG_COMPOSE_FORWARDS: %v\n", err)
		return 2
	}
	listeners, err := Listen(forwards)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bg-forward: %v\n", err)
		return 2
	}
	if len(forwards) > 0 {
		described := make([]string, len(forwards))
		for i, f := range forwards {
			described[i] = fmt.Sprintf("localhost:%d -> %s", f.Port, f.Target)
		}
		fmt.Fprintf(os.Stderr, "bg-forward: %s\n", strings.Join(described, ", "))
	}
	for _, l := range listeners {
		go l.serve()
	}
	return runChild(args[1:])
}

// ParseForwards parses BG_COMPOSE_FORWARDS. Empty means no forwards.
func ParseForwards(spec string) ([]Forward, error) {
	if spec == "" {
		return nil, nil
	}
	var out []Forward
	for _, entry := range strings.Split(spec, ",") {
		port, target, ok := strings.Cut(entry, "=")
		n, err := strconv.Atoi(port)
		if !ok || err != nil || n < 1 || n > 65535 {
			return nil, fmt.Errorf("entry %q is not <port>=<service>:<port>", entry)
		}
		host, targetPort, err := net.SplitHostPort(target)
		if err != nil || host == "" || targetPort == "" {
			return nil, fmt.Errorf("entry %q is not <port>=<service>:<port>", entry)
		}
		out = append(out, Forward{Port: n, Target: target})
	}
	return out, nil
}

type listener struct {
	net.Listener
	target string
}

// Listen binds every forward on 127.0.0.1 and ::1 before returning, so the
// command never starts before localhost answers. ::1 is skipped only when
// the container has no IPv6 loopback; Go dials [::1] first for localhost,
// so a bound ::1 is what makes "localhost" (not just 127.0.0.1) work.
func Listen(forwards []Forward) ([]listener, error) {
	var out []listener
	for _, f := range forwards {
		for _, host := range []string{"127.0.0.1", "::1"} {
			l, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(f.Port)))
			if err != nil {
				if host == "::1" && errors.Is(err, syscall.EADDRNOTAVAIL) {
					continue
				}
				for _, bound := range out {
					bound.Close()
				}
				return nil, fmt.Errorf("listen on localhost:%d for %s: %w", f.Port, f.Target, err)
			}
			out = append(out, listener{Listener: l, target: f.Target})
		}
	}
	return out, nil
}

func (l listener) serve() {
	for {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		go proxy(conn, l.target)
	}
}

func proxy(client net.Conn, target string) {
	defer client.Close()
	upstream, err := net.DialTimeout("tcp", target, 10*time.Second)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bg-forward: dial %s: %v\n", target, err)
		return
	}
	defer upstream.Close()
	done := make(chan struct{})
	go func() {
		pipe(upstream, client)
		close(done)
	}()
	pipe(client, upstream)
	<-done
}

// pipe copies src to dst, then half-closes dst so the peer sees EOF while
// the other direction keeps flowing.
func pipe(dst, src net.Conn) {
	_, _ = io.Copy(dst, src)
	if tcp, ok := dst.(*net.TCPConn); ok {
		_ = tcp.CloseWrite()
	}
}

// runChild runs command with this process's stdio and environment,
// relays termination signals to it, and returns its exit status (128+N
// when a signal ended it, the shell convention).
func runChild(command []string) int {
	cmd := exec.Command(command[0], command[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	signals := make(chan os.Signal, 4)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP, syscall.SIGQUIT)
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "bg-forward: start %s: %v\n", command[0], err)
		return 127
	}
	go func() {
		for sig := range signals {
			_ = cmd.Process.Signal(sig)
		}
	}()
	err := cmd.Wait()
	signal.Stop(signals)
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if status, ok := exitErr.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			return 128 + int(status.Signal())
		}
		return exitErr.ExitCode()
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "bg-forward: %s: %v\n", command[0], err)
		return 1
	}
	return 0
}
