//go:build darwin

package tunscope

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"testing"
	"time"
)

type cleanupRunner struct {
	errs  []error
	calls []string
}

func (r *cleanupRunner) Run(name string, args ...string) (string, error) {
	r.calls = append(r.calls, strings.Join(append([]string{name}, args...), " "))
	if len(r.errs) == 0 {
		return "", nil
	}
	err := r.errs[0]
	r.errs = r.errs[1:]
	return "", err
}

func cleanupTestState(routes ...Route) *State {
	return &State{
		Version:   stateVersion,
		Phase:     "active",
		OwnerPID:  os.Getpid(),
		EnginePID: 0,
		Routes:    append([]Route(nil), routes...),
	}
}

func TestCleanupRetainsOnlyFailedRoutesForRetry(t *testing.T) {
	t.Setenv("TUNSCOPE_STATE_DIR", t.TempDir())
	routes := []Route{
		{Family: "inet", Kind: "net", Target: "1.0.0.0/8", Gateway: tunGateway4, Purpose: "tun"},
		{Family: "inet", Kind: "net", Target: "2.0.0.0/7", Gateway: tunGateway4, Purpose: "tun"},
		{Family: "inet", Kind: "net", Target: "4.0.0.0/6", Gateway: tunGateway4, Purpose: "tun"},
	}
	state := cleanupTestState(routes...)
	if err := saveState(state); err != nil {
		t.Fatal(err)
	}
	runner := &cleanupRunner{errs: []error{
		nil,
		errors.New("routing socket unavailable"),
		errors.New("route: writing to routing socket: not in table"),
	}}
	app := &App{runner: runner, out: &bytes.Buffer{}, errOut: &bytes.Buffer{}}

	err := app.cleanup(state, nil)
	if err == nil || !strings.Contains(err.Error(), "routing socket unavailable") {
		t.Fatalf("cleanup error = %v, want route deletion failure", err)
	}
	if strings.Contains(err.Error(), "not in table") {
		t.Fatalf("missing route should be idempotent, got %v", err)
	}
	if len(runner.calls) != 3 || !strings.Contains(runner.calls[0], routes[2].Target) || !strings.Contains(runner.calls[2], routes[0].Target) {
		t.Fatalf("route deletion calls = %#v, want reverse route order", runner.calls)
	}

	retryState, err := loadState()
	if err != nil {
		t.Fatalf("load retryable state: %v", err)
	}
	if retryState.Phase != "cleanup_failed" {
		t.Fatalf("phase = %q, want cleanup_failed", retryState.Phase)
	}
	if len(retryState.Routes) != 1 || retryState.Routes[0].Target != routes[1].Target {
		t.Fatalf("retry routes = %#v, want only %s", retryState.Routes, routes[1].Target)
	}

	var status bytes.Buffer
	statusApp := &App{runner: &cleanupRunner{}, out: &status, errOut: &bytes.Buffer{}}
	if err := statusApp.Status(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(status.String(), "status: stale") {
		t.Fatalf("status output = %q, want stale rather than stopped", status.String())
	}

	if err := (&App{runner: &cleanupRunner{}, out: &bytes.Buffer{}, errOut: &bytes.Buffer{}}).cleanup(retryState, nil); err != nil {
		t.Fatalf("retry cleanup: %v", err)
	}
	if _, err := os.Stat(statePath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("state after successful retry: %v, want removed", err)
	}
}

func TestCleanupTreatsMissingRouteAsSuccess(t *testing.T) {
	t.Setenv("TUNSCOPE_STATE_DIR", t.TempDir())
	state := cleanupTestState(Route{
		Family: "inet", Kind: "host", Target: "203.0.113.1", Gateway: tunGateway4, Purpose: "dns",
	})
	if err := saveState(state); err != nil {
		t.Fatal(err)
	}
	app := &App{
		runner: &cleanupRunner{errs: []error{errors.New("delete host 203.0.113.1: No such process")}},
		out:    &bytes.Buffer{}, errOut: &bytes.Buffer{},
	}

	if err := app.cleanup(state, nil); err != nil {
		t.Fatalf("cleanup missing route: %v", err)
	}
	if _, err := os.Stat(statePath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("state after idempotent cleanup: %v, want removed", err)
	}
}

func TestCleanupRemovesTUNCaptureBeforeReconcileJournalRoutes(t *testing.T) {
	t.Setenv("TUNSCOPE_STATE_DIR", t.TempDir())
	tun4 := Route{Family: "inet", Kind: "net", Target: "1.0.0.0/8", Gateway: tunGateway4, Purpose: "tun"}
	tun6 := Route{Family: "inet6", Kind: "net", Target: "::/1", Interface: "utun123", Purpose: "tun"}
	physical := Route{
		Family: "inet", Kind: "net", Target: "2.0.0.0/7",
		Gateway: "192.168.1.1", Scope: "en0", Purpose: "direct-scope",
	}
	dns := Route{Family: "inet", Kind: "host", Target: "1.1.1.1", Gateway: "192.168.1.1", Purpose: "dns-direct"}
	bypassAfter := Route{Family: "inet", Kind: "host", Target: "203.0.113.9", Gateway: "192.168.50.1", Purpose: "bypass"}
	state := cleanupTestState(physical, tun4, dns, tun6)
	state.RouteReconcile = &RouteReconcileJournal{
		Before: []Route{physical, dns},
		After:  []Route{bypassAfter},
	}
	runner := &cleanupRunner{}
	app := &App{runner: runner, out: &bytes.Buffer{}, errOut: &bytes.Buffer{}}

	if err := app.cleanup(state, nil); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 5 {
		t.Fatalf("cleanup calls = %#v", runner.calls)
	}
	for i := 0; i < 2; i++ {
		if !strings.Contains(runner.calls[i], tun4.Target) && !strings.Contains(runner.calls[i], tun6.Target) {
			t.Fatalf("cleanup call %d = %q, want TUN capture removed first", i, runner.calls[i])
		}
	}
	for i := 2; i < len(runner.calls); i++ {
		if strings.Contains(runner.calls[i], tun4.Target) || strings.Contains(runner.calls[i], tun6.Target) {
			t.Fatalf("TUN capture route was delayed until call %d: %#v", i, runner.calls)
		}
	}
}

func TestRecoverStalePropagatesCleanupError(t *testing.T) {
	t.Setenv("TUNSCOPE_STATE_DIR", t.TempDir())
	if err := acquireLock(); err != nil {
		t.Fatal(err)
	}
	defer releaseLock()
	state := cleanupTestState(Route{
		Family: "inet", Kind: "net", Target: "8.0.0.0/5", Gateway: tunGateway4, Purpose: "tun",
	})
	state.OwnerPID = 1 << 30
	if err := saveState(state); err != nil {
		t.Fatal(err)
	}
	app := &App{
		runner: &cleanupRunner{errs: []error{errors.New("permission denied")}},
		out:    &bytes.Buffer{}, errOut: &bytes.Buffer{},
	}

	err := app.recoverStale()
	if err == nil || !strings.Contains(err.Error(), "recover stale TUN state") || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("recoverStale error = %v, want propagated cleanup error", err)
	}
	retryState, loadErr := loadState()
	if loadErr != nil {
		t.Fatalf("load retained state: %v", loadErr)
	}
	if retryState.Phase != "cleanup_failed" || len(retryState.Routes) != 1 {
		t.Fatalf("retained state = %#v", retryState)
	}
}

func TestCleanupWithoutProcessHandleTerminatesEngine(t *testing.T) {
	if os.Getenv("TUNSCOPE_CLEANUP_HELPER") == "1" {
		signal.Ignore(syscall.SIGTERM)
		fmt.Println("ready")
		for {
			time.Sleep(time.Second)
		}
	}

	t.Setenv("TUNSCOPE_STATE_DIR", t.TempDir())
	cmd := exec.Command(os.Args[0], "-test.run=^TestCleanupWithoutProcessHandleTerminatesEngine$")
	cmd.Env = append(os.Environ(), "TUNSCOPE_CLEANUP_HELPER=1")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || strings.TrimSpace(line) != "ready" {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("helper readiness = %q, %v", line, err)
	}
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()

	originalTerminateGrace, originalKillGrace, originalPoll := engineTerminateGrace, engineKillGrace, engineExitPoll
	engineTerminateGrace, engineKillGrace, engineExitPoll = 100*time.Millisecond, time.Second, 5*time.Millisecond
	defer func() {
		engineTerminateGrace, engineKillGrace, engineExitPoll = originalTerminateGrace, originalKillGrace, originalPoll
	}()

	state := cleanupTestState()
	state.EnginePID = cmd.Process.Pid
	identity, err := readProcessIdentity(state.EnginePID)
	if err != nil {
		_ = cmd.Process.Kill()
		<-waitCh
		t.Fatalf("read helper identity: %v", err)
	}
	state.EngineStartedAt = identity.StartedAt
	state.EngineCommand = identity.Command
	if err := saveState(state); err != nil {
		_ = cmd.Process.Kill()
		<-waitCh
		t.Fatal(err)
	}
	app := &App{runner: &cleanupRunner{}, out: &bytes.Buffer{}, errOut: &bytes.Buffer{}}
	if err := app.cleanup(state, nil); err != nil {
		_ = cmd.Process.Kill()
		<-waitCh
		t.Fatalf("cleanup stale engine: %v", err)
	}
	select {
	case <-waitCh:
	case <-time.After(time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("engine helper did not exit after cleanup")
	}
	if state.EnginePID != 0 {
		t.Fatalf("engine PID = %d after cleanup, want 0", state.EnginePID)
	}
	if _, err := os.Stat(statePath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("state after engine cleanup: %v, want removed", err)
	}
}

func TestCleanupWithTrustedHandleStopsUnidentifiedEngine(t *testing.T) {
	t.Setenv("TUNSCOPE_STATE_DIR", t.TempDir())
	cmd := exec.Command("/bin/sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	state := cleanupTestState()
	state.EnginePID = cmd.Process.Pid
	if err := saveState(state); err != nil {
		_ = cmd.Process.Kill()
		<-waitCh
		t.Fatal(err)
	}
	app := &App{runner: &cleanupRunner{}, out: &bytes.Buffer{}, errOut: &bytes.Buffer{}}
	if err := app.cleanup(state, cmd.Process); err != nil {
		_ = cmd.Process.Kill()
		<-waitCh
		t.Fatalf("cleanup trusted engine handle without persisted identity: %v", err)
	}
	select {
	case <-waitCh:
	case <-time.After(time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("trusted engine process was not stopped")
	}
}

func TestCleanupDoesNotSignalReusedEnginePID(t *testing.T) {
	t.Setenv("TUNSCOPE_STATE_DIR", t.TempDir())
	cmd := exec.Command("/bin/sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()
	identity, err := readProcessIdentity(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	state := cleanupTestState()
	state.EnginePID = cmd.Process.Pid
	state.EngineStartedAt = identity.StartedAt.Add(time.Microsecond)
	state.EngineCommand = identity.Command
	if err := saveState(state); err != nil {
		t.Fatal(err)
	}
	app := &App{runner: &cleanupRunner{}, out: &bytes.Buffer{}, errOut: &bytes.Buffer{}}

	if err := app.cleanup(state, nil); err != nil {
		t.Fatalf("cleanup reused engine PID: %v", err)
	}
	if !processAlive(cmd.Process.Pid) {
		t.Fatal("cleanup signaled a live process whose birth identity did not match")
	}
}

func TestStatusRequiresLockTokenAndBirthIdentity(t *testing.T) {
	t.Setenv("TUNSCOPE_STATE_DIR", t.TempDir())
	if err := acquireLock(); err != nil {
		t.Fatal(err)
	}
	defer releaseLock()
	identity, err := readProcessIdentity(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	state := cleanupTestState()
	state.OwnerToken = currentLockToken()
	state.OwnerStartedAt = identity.StartedAt
	state.OwnerCommand = identity.Command
	state.EnginePID = os.Getpid()
	state.EngineStartedAt = identity.StartedAt
	state.EngineCommand = identity.Command
	if err := saveState(state); err != nil {
		t.Fatal(err)
	}

	var activeOutput bytes.Buffer
	app := &App{runner: &cleanupRunner{}, out: &activeOutput, errOut: &bytes.Buffer{}}
	if err := app.Status(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(activeOutput.String(), "status: active") {
		t.Fatalf("valid status output = %q, want active", activeOutput.String())
	}

	state.EngineStartedAt = state.EngineStartedAt.Add(time.Microsecond)
	if err := saveState(state); err != nil {
		t.Fatal(err)
	}
	var staleOutput bytes.Buffer
	app.out = &staleOutput
	if err := app.Status(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(staleOutput.String(), "status: stale") || !strings.Contains(staleOutput.String(), "birth identity changed") {
		t.Fatalf("reused PID status output = %q, want stale identity diagnostic", staleOutput.String())
	}
}

func TestKernelLockHelper(t *testing.T) {
	if os.Getenv("TUNSCOPE_LOCK_HELPER") != "1" {
		return
	}
	if err := acquireLock(); err != nil {
		t.Fatal(err)
	}
	fmt.Println("ready")
	_, _ = io.Copy(io.Discard, os.Stdin)
}

func TestKernelLockCannotBeReleasedByContender(t *testing.T) {
	t.Setenv("TUNSCOPE_STATE_DIR", t.TempDir())
	cmd := exec.Command(os.Args[0], "-test.run=^TestKernelLockHelper$")
	cmd.Env = append(os.Environ(), "TUNSCOPE_LOCK_HELPER=1")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(stdout)
	if line, err := reader.ReadString('\n'); err != nil || strings.TrimSpace(line) != "ready" {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("lock helper readiness = %q, %v", line, err)
	}

	if err := acquireLock(); !errors.Is(err, errLockHeld) {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("contended acquire = %v, want errLockHeld", err)
	}
	releaseLock()
	if err := acquireLock(); !errors.Is(err, errLockHeld) {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("acquire after unrelated release = %v, want child lock retained", err)
	}
	if err := stdin.Close(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	if err := acquireLock(); err != nil {
		t.Fatalf("acquire after holder exit: %v", err)
	}
	defer releaseLock()
	pid, token := lockRecord()
	if pid != os.Getpid() || token == "" || token != currentLockToken() {
		t.Fatalf("lock record = (%d, %q), want current PID and token", pid, token)
	}
	info, err := os.Stat(lockPath())
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0644 {
		t.Fatalf("lock permissions = %04o, want 0644 for unprivileged status reads", got)
	}
}
