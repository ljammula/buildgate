package main

import (
	"buildgate/internal/hostcontrol"
	"context"
	"io"
	"time"
)

// The methods that make *deps a hostcontrol dependency set.

func (dp *deps) ColimaBinary() string { return dp.docker.colimaBinary() }
func (dp *deps) DockerBinary() string { return dp.docker.dockerBinary() }
func (dp *deps) EnsureTemporal(ctx context.Context, w io.Writer) string {
	return dp.temporal.ensure(ctx, w)
}
func (dp *deps) GatewayHealthy(ctx context.Context) error { return dp.sandbox.gatewayHealthy(ctx) }
func (dp *deps) MeterHealthy(ctx context.Context) error   { return dp.sandbox.meterHealthy(ctx) }
func (dp *deps) SandboxNames(ctx context.Context) ([]string, error) {
	return dp.sandbox.sandboxNames(ctx)
}
func (dp *deps) Launchctl(args ...string) ([]byte, error) { return dp.host.launchctl(args...) }
func (dp *deps) LaunchdServicePID(domain string) (int, bool) {
	return dp.host.launchdServicePID(domain)
}
func (dp *deps) Lsof(port string) ([]byte, error) { return dp.host.lsof(port) }
func (dp *deps) PidLooksLikeWorker(pid int) bool  { return dp.host.pidLooksLikeWorker(pid) }
func (dp *deps) ProcessUID(pid int) (int, bool)   { return dp.host.processUID(pid) }
func (dp *deps) ServeHealthzOK(addr string) bool  { return dp.host.serveHealthzOK(addr) }
func (dp *deps) ServeVerifiedOurs(dataDir string, addr string) (int, bool) {
	return dp.host.serveVerifiedOurs(dataDir, addr)
}
func (dp *deps) Sleep(d time.Duration) { dp.host.sleep(d) }
func (dp *deps) SpawnServe(w io.Writer, binaryPath string, configPath string, dataDir string, addr string) error {
	return dp.host.spawnServe(w, binaryPath, configPath, dataDir, addr)
}
func (dp *deps) SpawnWorker(w io.Writer, binaryPath string, configPath string, dataDir string, credentialEnv []string, pidPath string, temporalAddress string) error {
	return dp.host.spawnWorker(w, binaryPath, configPath, dataDir, credentialEnv, pidPath, temporalAddress)
}
func (dp *deps) TemporalHealthy(ctx context.Context, addr string) error {
	return dp.temporal.healthy(ctx, addr)
}

func (dp *deps) WorkerServiceState(ctx context.Context, plistPath string) string {
	return doctorCheckWorkerService(ctx, dp.host.launchctlBinary(), plistPath).Name
}

func (dp *deps) ServeStartToken(configPath string) (token, path string, err error) {
	path = serveStableStartTokenPathFor(configPath)
	token, _, err = ensureServeStartTokenFile(dp, path)
	return token, path, err
}

// The real boundary methods whose bodies live in hostcontrol.

func (impl realDocker) dockerBinary() string { return hostcontrol.RealDockerBinary(impl.dp) }
func (impl realTemporal) healthy(ctx context.Context, addr string) error {
	return hostcontrol.RealHealthy(impl.dp, ctx, addr)
}
func (impl realTemporal) ensure(ctx context.Context, w io.Writer) string {
	return hostcontrol.RealEnsure(impl.dp, ctx, w)
}
func (impl realDocker) colimaBinary() string  { return hostcontrol.RealColimaBinary(impl.dp) }
func (impl realHost) launchctlBinary() string { return hostcontrol.RealLaunchctlBinary(impl.dp) }
func (impl realHost) serveHealthzOK(addr string) bool {
	return hostcontrol.RealServeHealthzOK(impl.dp, addr)
}
func (impl realHost) lsof(port string) ([]byte, error) { return hostcontrol.RealLsof(impl.dp, port) }
func (impl realHost) processUID(pid int) (int, bool)   { return hostcontrol.RealProcessUID(impl.dp, pid) }
func (impl realHost) serveVerifiedOurs(dataDir, addr string) (int, bool) {
	return hostcontrol.RealServeVerifiedOurs(impl.dp, dataDir, addr)
}
func (impl realHost) spawnServe(w io.Writer, binaryPath, configPath, dataDir, addr string) error {
	return hostcontrol.RealSpawnServe(impl.dp, w, binaryPath, configPath, dataDir, addr)
}
func (impl realHost) pidLooksLikeWorker(pid int) bool {
	return hostcontrol.RealPidLooksLikeWorker(impl.dp, pid)
}
func (impl realHost) spawnWorker(w io.Writer, binaryPath, configPath, dataDir string, credentialEnv []string, pidPath string, temporalAddress string) error {
	return hostcontrol.RealSpawnWorker(impl.dp, w, binaryPath, configPath, dataDir, credentialEnv, pidPath, temporalAddress)
}
func (impl realHost) sleep(d time.Duration) { hostcontrol.RealSleep(impl.dp, d) }
func (impl realHost) launchdServicePID(domain string) (int, bool) {
	return hostcontrol.RealLaunchdServicePID(impl.dp, domain)
}
