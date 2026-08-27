//go:build darwin

package tunscope

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"reflect"
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

func TestCleanupTransitionsDirectlyToAutomaticRestartState(t *testing.T) {
	t.Setenv("TUNSCOPE_STATE_DIR", t.TempDir())
	state := cleanupTestState(Route{
		Family: "inet", Kind: "net", Target: "8.0.0.0/5", Gateway: tunGateway4, Purpose: "tun",
	})
	state.OwnerToken = "owner-token"
	state.Device = "utun123"
	if err := saveState(state); err != nil {
		t.Fatal(err)
	}
	waiting := automaticRestartWaitingState(state)
	app := &App{runner: &cleanupRunner{}, out: &bytes.Buffer{}, errOut: &bytes.Buffer{}}

	if err := app.cleanupToState(state, nil, waiting); err != nil {
		t.Fatalf("cleanup to automatic restart state: %v", err)
	}
	persisted, err := loadState()
	if err != nil {
		t.Fatalf("load automatic restart state: %v", err)
	}
	if persisted.Phase != "waiting_network" || persisted.OwnerToken != "owner-token" ||
		persisted.EnginePID != 0 || len(persisted.Routes) != 0 || !persisted.RoutesSuspended {
		t.Fatalf("automatic restart state = %#v", persisted)
	}
}

func TestCleanupFailureCannotReplaceLedgerWithAutomaticRestartState(t *testing.T) {
	t.Setenv("TUNSCOPE_STATE_DIR", t.TempDir())
	state := cleanupTestState(Route{
		Family: "inet", Kind: "net", Target: "8.0.0.0/5", Gateway: tunGateway4, Purpose: "tun",
	})
	if err := saveState(state); err != nil {
		t.Fatal(err)
	}
	waiting := automaticRestartWaitingState(state)
	app := &App{
		runner: &cleanupRunner{errs: []error{errors.New("routing socket unavailable")}},
		out:    &bytes.Buffer{}, errOut: &bytes.Buffer{},
	}

	if err := app.cleanupToState(state, nil, waiting); err == nil {
		t.Fatal("cleanup failure unexpectedly entered automatic recovery")
	}
	persisted, err := loadState()
	if err != nil {
		t.Fatalf("load retained cleanup ledger: %v", err)
	}
	if persisted.Phase != "cleanup_failed" || len(persisted.Routes) != 1 {
		t.Fatalf("retained cleanup state = %#v", persisted)
	}
}

func TestSuspendAndResumeOwnedRoutesAroundNetworkRecovery(t *testing.T) {
	t.Setenv("TUNSCOPE_STATE_DIR", t.TempDir())
	physical := Route{
		Family: "inet", Kind: "net", Target: "1.0.0.0/8",
		Gateway: "192.168.1.1", Scope: "en0", Purpose: "direct-scope",
	}
	bypass := Route{
		Family: "inet", Kind: "host", Target: "203.0.113.9",
		Gateway: "192.168.1.1", Purpose: "bypass",
	}
	tun := Route{Family: "inet", Kind: "net", Target: "8.0.0.0/5", Gateway: tunGateway4, Purpose: "tun"}
	dns := Route{Family: "inet", Kind: "host", Target: "8.8.8.8", Gateway: tunGateway4, Purpose: "dns"}
	state := cleanupTestState(physical, bypass, tun, dns)
	if err := saveState(state); err != nil {
		t.Fatal(err)
	}
	runner := &cleanupRunner{}
	app := &App{runner: runner, out: &bytes.Buffer{}, errOut: &bytes.Buffer{}}

	removed, err := app.suspendOwnedRoutes(state)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 4 || !state.RoutesSuspended {
		t.Fatalf("suspension removed %d routes, suspended=%v", removed, state.RoutesSuspended)
	}
	if len(runner.calls) != 4 || !strings.Contains(runner.calls[0], " delete ") || !strings.Contains(runner.calls[0], tun.Target) {
		t.Fatalf("suspension calls = %#v, want TUN capture deleted first", runner.calls)
	}
	persisted, err := loadState()
	if err != nil {
		t.Fatal(err)
	}
	if !persisted.RoutesSuspended || len(persisted.Routes) != 4 {
		t.Fatalf("persisted suspended state = %#v", persisted)
	}

	// Physical managed routes are restored by reconciliation before this call;
	// resume must add only capture routes after the engine is rebound.
	runner.calls = nil
	added, err := app.resumeCaptureRoutes(state)
	if err != nil {
		t.Fatal(err)
	}
	if added != 2 || state.RoutesSuspended {
		t.Fatalf("resume added %d routes, suspended=%v", added, state.RoutesSuspended)
	}
	if len(runner.calls) != 2 || !strings.Contains(runner.calls[0], " add ") || !strings.Contains(runner.calls[0], dns.Target) ||
		!strings.Contains(runner.calls[1], tun.Target) {
		t.Fatalf("resume calls = %#v, want DNS then broad TUN capture", runner.calls)
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

func TestStatusRecognizesOwnedAutomaticRestartStateWithoutEngine(t *testing.T) {
	t.Setenv("TUNSCOPE_STATE_DIR", t.TempDir())
	if err := acquireLock(); err != nil {
		t.Fatal(err)
	}
	defer releaseLock()
	identity, err := readProcessIdentity(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	state := automaticRestartWaitingState(cleanupTestState())
	state.OwnerToken = currentLockToken()
	state.OwnerStartedAt = identity.StartedAt
	state.OwnerCommand = identity.Command
	state.EnginePID = 0
	if err := saveState(state); err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	app := &App{runner: &cleanupRunner{}, out: &output, errOut: &bytes.Buffer{}}
	if err := app.Status(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "status: waiting-network") ||
		!strings.Contains(output.String(), "system routes are restored") {
		t.Fatalf("waiting status output = %q", output.String())
	}

	state.OwnerStartedAt = state.OwnerStartedAt.Add(time.Microsecond)
	if err := saveState(state); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	if err := app.Status(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "status: stale") ||
		!strings.Contains(output.String(), "owner PID birth identity changed") {
		t.Fatalf("invalid waiting owner status output = %q", output.String())
	}
}

func TestAutomaticRestartWaitRequiresConsecutiveStableSamples(t *testing.T) {
	signatures := []struct {
		value string
		err   error
	}{
		{value: "network-a"},
		{value: "network-a"},
		{err: errors.New("route disappeared")},
		{value: "network-a"},
		{value: "network-b"},
		{value: "network-b"},
		{value: "network-b"},
	}
	calls := 0
	sig := waitForStablePhysicalNetwork(
		make(chan os.Signal),
		0,
		time.Millisecond,
		func() (string, error) {
			result := signatures[calls]
			calls++
			return result.value, result.err
		},
		func() bool { return true },
	)
	if sig != nil || calls != len(signatures) {
		t.Fatalf("stable wait = signal %v after %d calls, want %d", sig, calls, len(signatures))
	}
}

func TestAutomaticRestartWaitIsImmediatelyCancellable(t *testing.T) {
	sigCh := make(chan os.Signal, 1)
	sigCh <- syscall.SIGTERM
	samples := 0
	sig := waitForStablePhysicalNetwork(
		sigCh,
		time.Hour,
		time.Millisecond,
		func() (string, error) {
			samples++
			return "network", nil
		},
		func() bool { return true },
	)
	if sig != syscall.SIGTERM || samples != 0 {
		t.Fatalf("cancelled wait = signal %v, samples %d", sig, samples)
	}
}

func TestAutomaticRestartDelayIsCapped(t *testing.T) {
	if got := automaticRestartDelay(0); got != 0 {
		t.Fatalf("initial restart delay = %s", got)
	}
	if got := automaticRestartDelay(1); got != time.Second {
		t.Fatalf("first retry delay = %s", got)
	}
	if got := automaticRestartDelay(100); got != 30*time.Second {
		t.Fatalf("capped retry delay = %s", got)
	}
}

func ownedAutomaticRestartTestState(t *testing.T) *State {
	t.Helper()
	identity, err := readProcessIdentity(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	state := automaticRestartWaitingState(cleanupTestState())
	state.OwnerToken = currentLockToken()
	state.OwnerStartedAt = identity.StartedAt
	state.OwnerCommand = identity.Command
	if err := saveState(state); err != nil {
		t.Fatal(err)
	}
	return state
}

func ownedStartingTestState(t *testing.T) *State {
	t.Helper()
	cfg := DefaultConfig()
	cfg.Proxy = "socks5://user:secret@127.0.0.1:1080"
	state, err := newOwnerStartingState(cfg, []string{"/Applications/Test.app"})
	if err != nil {
		t.Fatal(err)
	}
	if err := saveState(state); err != nil {
		t.Fatal(err)
	}
	return state
}

func TestStatusRecognizesOwnedStartingState(t *testing.T) {
	t.Setenv("TUNSCOPE_STATE_DIR", t.TempDir())
	if err := acquireLock(); err != nil {
		t.Fatal(err)
	}
	defer releaseLock()
	ownedStartingTestState(t)
	var output bytes.Buffer
	app := &App{runner: &cleanupRunner{}, out: &output, errOut: &bytes.Buffer{}}
	if err := app.Status(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "status: starting") ||
		!strings.Contains(output.String(), "TUN data plane is being initialized") {
		t.Fatalf("starting status output = %q", output.String())
	}
}

func TestOwnedStartingStateRejectsDataPlaneLedger(t *testing.T) {
	t.Setenv("TUNSCOPE_STATE_DIR", t.TempDir())
	if err := acquireLock(); err != nil {
		t.Fatal(err)
	}
	defer releaseLock()
	state := ownedStartingTestState(t)
	state.Routes = []Route{{Family: "inet", Kind: "host", Target: "203.0.113.10", Purpose: "bypass"}}
	if err := saveState(state); err != nil {
		t.Fatal(err)
	}
	if _, err := loadOwnedStartingState(); err == nil || !strings.Contains(err.Error(), "cleanup ownership") {
		t.Fatalf("unsafe starting ledger result = %v", err)
	}
}

func TestInitialNetworkReadinessFailureWaitsAndRetries(t *testing.T) {
	t.Setenv("TUNSCOPE_STATE_DIR", t.TempDir())
	if err := acquireLock(); err != nil {
		t.Fatal(err)
	}
	defer releaseLock()
	starting := ownedStartingTestState(t)
	app := &App{runner: &cleanupRunner{}, out: &bytes.Buffer{}, errOut: &bytes.Buffer{}}
	runs := 0
	var delays []time.Duration
	err := app.superviseAutomaticRestart(make(chan os.Signal), starting, automaticRestartSupervisorDeps{
		allowInitialRetry: true,
		runSession: func(restartState *State) error {
			runs++
			if runs == 1 {
				if restartState != nil {
					t.Fatalf("initial restart state = %#v, want nil", restartState)
				}
				return networkPreflightFailure(nil, &net.DNSError{Err: "i/o timeout", IsTimeout: true})
			}
			if restartState == nil || restartState.Phase != "recovering" {
				t.Fatalf("retry state = %#v", restartState)
			}
			return nil
		},
		waitNetwork: func(delay time.Duration) os.Signal {
			delays = append(delays, delay)
			persisted, loadErr := loadState()
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			if persisted.Phase != "waiting_network" || persisted.EnginePID != 0 ||
				len(persisted.Routes) != 0 || !persisted.RoutesSuspended {
				t.Fatalf("initial waiting marker = %#v", persisted)
			}
			if persisted.Proxy != "socks5://127.0.0.1:1080" {
				t.Fatalf("persisted proxy = %q", persisted.Proxy)
			}
			return nil
		},
		waitInterfaceGone: func() (os.Signal, error) { return nil, nil },
	})
	if err != nil {
		t.Fatalf("initial automatic recovery: %v", err)
	}
	if runs != 2 || len(delays) != 1 || delays[0] != time.Second {
		t.Fatalf("runs=%d delays=%v", runs, delays)
	}
	if _, err := os.Stat(statePath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("state after successful initial retry = %v", err)
	}
}

func TestInitialRecoveryStopsRetryingAfterPermanentFailure(t *testing.T) {
	t.Setenv("TUNSCOPE_STATE_DIR", t.TempDir())
	if err := acquireLock(); err != nil {
		t.Fatal(err)
	}
	defer releaseLock()
	starting := ownedStartingTestState(t)
	app := &App{runner: &cleanupRunner{}, out: &bytes.Buffer{}, errOut: &bytes.Buffer{}}
	runs := 0
	waits := 0
	err := app.superviseAutomaticRestart(make(chan os.Signal), starting, automaticRestartSupervisorDeps{
		allowInitialRetry: true,
		runSession: func(restartState *State) error {
			runs++
			if runs == 1 {
				return networkPreflightFailure(restartState, &net.DNSError{Err: "timeout", IsTimeout: true})
			}
			return networkPreflightFailure(restartState, errors.New("rejected username/password"))
		},
		waitNetwork: func(time.Duration) os.Signal {
			waits++
			return nil
		},
		waitInterfaceGone: func() (os.Signal, error) { return nil, nil },
	})
	if err == nil || !strings.Contains(err.Error(), "rejected username/password") {
		t.Fatalf("permanent recovery result = %v", err)
	}
	if runs != 2 || waits != 1 {
		t.Fatalf("runs=%d waits=%d", runs, waits)
	}
	if _, err := os.Stat(statePath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("marker after permanent recovery failure = %v", err)
	}
}

func TestInitialRecoveryBackoffSequenceIsCapped(t *testing.T) {
	t.Setenv("TUNSCOPE_STATE_DIR", t.TempDir())
	if err := acquireLock(); err != nil {
		t.Fatal(err)
	}
	defer releaseLock()
	starting := ownedStartingTestState(t)
	app := &App{runner: &cleanupRunner{}, out: &bytes.Buffer{}, errOut: &bytes.Buffer{}}
	runs := 0
	var delays []time.Duration
	err := app.superviseAutomaticRestart(make(chan os.Signal), starting, automaticRestartSupervisorDeps{
		allowInitialRetry: true,
		runSession: func(restartState *State) error {
			runs++
			if runs <= 6 {
				return networkPreflightFailure(restartState, &net.DNSError{Err: "timeout", IsTimeout: true})
			}
			return nil
		},
		waitNetwork: func(delay time.Duration) os.Signal {
			delays = append(delays, delay)
			return nil
		},
		waitInterfaceGone: func() (os.Signal, error) { return nil, nil },
	})
	if err != nil {
		t.Fatalf("capped initial recovery: %v", err)
	}
	wantDelays := []time.Duration{time.Second, 2 * time.Second, 5 * time.Second, 10 * time.Second, 30 * time.Second, 30 * time.Second}
	if runs != 7 || !reflect.DeepEqual(delays, wantDelays) {
		t.Fatalf("runs=%d delays=%v, want %v", runs, delays, wantDelays)
	}
}

func TestInitialRetryIsDisabledForExplicitPhysicalConfiguration(t *testing.T) {
	t.Setenv("TUNSCOPE_STATE_DIR", t.TempDir())
	if err := acquireLock(); err != nil {
		t.Fatal(err)
	}
	defer releaseLock()
	starting := ownedStartingTestState(t)
	app := &App{runner: &cleanupRunner{}, out: &bytes.Buffer{}, errOut: &bytes.Buffer{}}
	waits := 0
	err := app.superviseAutomaticRestart(make(chan os.Signal), starting, automaticRestartSupervisorDeps{
		allowInitialRetry: false,
		runSession: func(*State) error {
			return networkPreflightFailure(nil, &net.DNSError{Err: "timeout", IsTimeout: true})
		},
		waitNetwork: func(time.Duration) os.Signal {
			waits++
			return nil
		},
		waitInterfaceGone: func() (os.Signal, error) { return nil, nil },
	})
	if err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("explicit physical configuration result = %v", err)
	}
	if waits != 0 {
		t.Fatalf("explicit physical configuration waits = %d", waits)
	}
	if _, err := os.Stat(statePath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("starting marker after explicit-mode failure = %v", err)
	}
}

func TestInitialFatalPreflightFailureDoesNotRetry(t *testing.T) {
	t.Setenv("TUNSCOPE_STATE_DIR", t.TempDir())
	if err := acquireLock(); err != nil {
		t.Fatal(err)
	}
	defer releaseLock()
	starting := ownedStartingTestState(t)
	app := &App{runner: &cleanupRunner{}, out: &bytes.Buffer{}, errOut: &bytes.Buffer{}}
	waits := 0
	err := app.superviseAutomaticRestart(make(chan os.Signal), starting, automaticRestartSupervisorDeps{
		allowInitialRetry: true,
		runSession: func(*State) error {
			return networkPreflightFailure(nil, errors.New("rejected username/password"))
		},
		waitNetwork: func(time.Duration) os.Signal {
			waits++
			return nil
		},
		waitInterfaceGone: func() (os.Signal, error) { return nil, nil },
	})
	if err == nil || !strings.Contains(err.Error(), "rejected username/password") {
		t.Fatalf("fatal initial result = %v", err)
	}
	if waits != 0 {
		t.Fatalf("network waits after fatal initial error = %d", waits)
	}
	if _, err := os.Stat(statePath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("starting marker after fatal error = %v", err)
	}
}

func TestInitialRetryDoesNotOverwriteCleanupFailure(t *testing.T) {
	t.Setenv("TUNSCOPE_STATE_DIR", t.TempDir())
	if err := acquireLock(); err != nil {
		t.Fatal(err)
	}
	defer releaseLock()
	starting := ownedStartingTestState(t)
	failedRoute := Route{Family: "inet", Kind: "host", Target: "203.0.113.9", Gateway: "192.0.2.1", Purpose: "bypass"}
	app := &App{runner: &cleanupRunner{}, out: &bytes.Buffer{}, errOut: &bytes.Buffer{}}
	waits := 0
	err := app.superviseAutomaticRestart(make(chan os.Signal), starting, automaticRestartSupervisorDeps{
		allowInitialRetry: true,
		runSession: func(*State) error {
			failed := *starting
			failed.Phase = "cleanup_failed"
			failed.Routes = []Route{failedRoute}
			if saveErr := saveState(&failed); saveErr != nil {
				t.Fatal(saveErr)
			}
			return automaticRestartAttemptFailure(errors.New("network timeout during cleanup"))
		},
		waitNetwork: func(time.Duration) os.Signal {
			waits++
			return nil
		},
		waitInterfaceGone: func() (os.Signal, error) { return nil, nil },
	})
	if err == nil || !strings.Contains(err.Error(), "unsafe phase \"cleanup_failed\"") {
		t.Fatalf("cleanup failure retry result = %v", err)
	}
	if waits != 0 {
		t.Fatalf("network waits after cleanup failure = %d", waits)
	}
	persisted, loadErr := loadState()
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if persisted.Phase != "cleanup_failed" || len(persisted.Routes) != 1 || persisted.Routes[0] != failedRoute {
		t.Fatalf("cleanup failure state was overwritten: %#v", persisted)
	}
}

func TestInitialAutomaticRecoveryCanBeCancelled(t *testing.T) {
	t.Setenv("TUNSCOPE_STATE_DIR", t.TempDir())
	if err := acquireLock(); err != nil {
		t.Fatal(err)
	}
	defer releaseLock()
	starting := ownedStartingTestState(t)
	app := &App{runner: &cleanupRunner{}, out: &bytes.Buffer{}, errOut: &bytes.Buffer{}}
	runs := 0
	err := app.superviseAutomaticRestart(make(chan os.Signal), starting, automaticRestartSupervisorDeps{
		allowInitialRetry: true,
		runSession: func(*State) error {
			runs++
			return automaticRestartAttemptFailure(errors.New("trusted DNS timeout"))
		},
		waitNetwork:       func(time.Duration) os.Signal { return syscall.SIGTERM },
		waitInterfaceGone: func() (os.Signal, error) { return nil, nil },
	})
	if err != nil {
		t.Fatalf("cancel initial recovery: %v", err)
	}
	if runs != 1 {
		t.Fatalf("initial session runs after cancellation = %d", runs)
	}
	if _, err := os.Stat(statePath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("state after initial cancellation = %v", err)
	}
}

func TestNetworkPreflightFailureClassification(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		retryable bool
	}{
		{name: "timeout", err: &net.DNSError{Err: "timeout", IsTimeout: true}, retryable: true},
		{name: "SOCKS network unreachable", err: errors.New("CONNECT: network unreachable"), retryable: true},
		{name: "upstream EOF", err: fmt.Errorf("read trusted DNS response: %w", io.EOF), retryable: true},
		{name: "authentication", err: errors.New("rejected username/password")},
		{name: "invalid DNS response", err: errors.New("trusted DNS returned an invalid response")},
		{name: "unsupported method", err: errors.New("unsupported method")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := networkPreflightFailure(nil, test.err)
			var retry *automaticRestartAttemptError
			if got := errors.As(err, &retry); got != test.retryable {
				t.Fatalf("retryable = %v, want %v; error: %v", got, test.retryable, err)
			}
		})
	}
	var retry *automaticRestartAttemptError
	if err := networkPreflightFailure(&State{WasActive: true}, errors.New("rejected username/password")); !errors.As(err, &retry) {
		t.Fatalf("previously active session did not retain restart behavior: %v", err)
	}
}

func TestValidateStartupInputsRejectsStaticErrors(t *testing.T) {
	base := DefaultConfig()
	base.Proxy = "socks5://127.0.0.1:1080"
	tests := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{name: "proxy", mutate: func(cfg *Config) { cfg.Proxy = "http://127.0.0.1:8080" }, want: "scheme must be socks5"},
		{name: "device", mutate: func(cfg *Config) { cfg.Device = "tun0" }, want: "--device must look like"},
		{name: "MTU", mutate: func(cfg *Config) { cfg.MTU = 1000 }, want: "--mtu must be"},
		{name: "log level", mutate: func(cfg *Config) { cfg.LogLevel = "verbose" }, want: "invalid --log-level"},
		{name: "trusted DNS", mutate: func(cfg *Config) { cfg.TrustedDNS = "localhost" }, want: "trusted DNS must be"},
		{name: "gateway", mutate: func(cfg *Config) { cfg.Gateway4 = "not-an-ip" }, want: "--gateway must be"},
		{name: "package family", mutate: func(cfg *Config) { cfg.PackageFamilies = []string{"OpenAI.Codex_2p2nqsd0c76g0"} }, want: "not supported on macOS"},
		{name: "TCP only", mutate: func(cfg *Config) { cfg.TCPOnly = true }, want: "requires at least one --app"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := base
			test.mutate(&cfg)
			_, _, err := validateStartupInputs(cfg)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("validation error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestAutomaticRestartSupervisorRebuildsAfterSafeMarker(t *testing.T) {
	t.Setenv("TUNSCOPE_STATE_DIR", t.TempDir())
	if err := acquireLock(); err != nil {
		t.Fatal(err)
	}
	defer releaseLock()
	app := &App{runner: &cleanupRunner{}, out: &bytes.Buffer{}, errOut: &bytes.Buffer{}}
	runs := 0
	var delays []time.Duration
	err := app.superviseAutomaticRestart(make(chan os.Signal), nil, automaticRestartSupervisorDeps{
		runSession: func(restartState *State) error {
			runs++
			if runs == 1 {
				ownedAutomaticRestartTestState(t)
				return &automaticRestartRequest{cause: errors.New("physical network changed")}
			}
			if restartState == nil || restartState.Phase != "recovering" {
				t.Fatalf("restart state = %#v", restartState)
			}
			return nil
		},
		waitNetwork: func(delay time.Duration) os.Signal {
			delays = append(delays, delay)
			return nil
		},
		waitInterfaceGone: func() (os.Signal, error) { return nil, nil },
	})
	if err != nil {
		t.Fatalf("automatic restart supervisor: %v", err)
	}
	if runs != 2 || len(delays) != 1 || delays[0] != 0 {
		t.Fatalf("restart runs = %d, delays = %v", runs, delays)
	}
	if _, err := os.Stat(statePath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("state after successful restart = %v", err)
	}
}

func TestAutomaticRestartSupervisorBacksOffAfterReadinessFailure(t *testing.T) {
	t.Setenv("TUNSCOPE_STATE_DIR", t.TempDir())
	if err := acquireLock(); err != nil {
		t.Fatal(err)
	}
	defer releaseLock()
	app := &App{runner: &cleanupRunner{}, out: &bytes.Buffer{}, errOut: &bytes.Buffer{}}
	runs := 0
	var delays []time.Duration
	err := app.superviseAutomaticRestart(make(chan os.Signal), nil, automaticRestartSupervisorDeps{
		runSession: func(*State) error {
			runs++
			switch runs {
			case 1:
				ownedAutomaticRestartTestState(t)
				return &automaticRestartRequest{cause: errors.New("network timeout")}
			case 2:
				return &automaticRestartAttemptError{cause: errors.New("SOCKS5 is not ready")}
			default:
				return nil
			}
		},
		waitNetwork: func(delay time.Duration) os.Signal {
			delays = append(delays, delay)
			return nil
		},
		waitInterfaceGone: func() (os.Signal, error) { return nil, nil },
	})
	if err != nil {
		t.Fatalf("automatic restart retry: %v", err)
	}
	if runs != 3 || len(delays) != 2 || delays[0] != 0 || delays[1] != time.Second {
		t.Fatalf("restart runs = %d, delays = %v", runs, delays)
	}
}

func TestAutomaticRestartSupervisorRetriesWhileOldInterfaceRemains(t *testing.T) {
	t.Setenv("TUNSCOPE_STATE_DIR", t.TempDir())
	if err := acquireLock(); err != nil {
		t.Fatal(err)
	}
	defer releaseLock()
	app := &App{runner: &cleanupRunner{}, out: &bytes.Buffer{}, errOut: &bytes.Buffer{}}
	runs := 0
	interfaceChecks := 0
	var delays []time.Duration
	err := app.superviseAutomaticRestart(make(chan os.Signal), nil, automaticRestartSupervisorDeps{
		runSession: func(*State) error {
			runs++
			if runs == 1 {
				ownedAutomaticRestartTestState(t)
				return &automaticRestartRequest{cause: errors.New("network timeout")}
			}
			return nil
		},
		waitNetwork: func(delay time.Duration) os.Signal {
			delays = append(delays, delay)
			return nil
		},
		waitInterfaceGone: func() (os.Signal, error) {
			interfaceChecks++
			if interfaceChecks == 1 {
				return nil, errors.New("old utun still exists")
			}
			return nil, nil
		},
	})
	if err != nil {
		t.Fatalf("automatic interface retry: %v", err)
	}
	if runs != 2 || interfaceChecks != 2 || len(delays) != 2 || delays[0] != 0 || delays[1] != time.Second {
		t.Fatalf("runs=%d interface checks=%d delays=%v", runs, interfaceChecks, delays)
	}
}

func TestAutomaticRestartSupervisorStopsWhileWaiting(t *testing.T) {
	t.Setenv("TUNSCOPE_STATE_DIR", t.TempDir())
	if err := acquireLock(); err != nil {
		t.Fatal(err)
	}
	defer releaseLock()
	app := &App{runner: &cleanupRunner{}, out: &bytes.Buffer{}, errOut: &bytes.Buffer{}}
	runs := 0
	err := app.superviseAutomaticRestart(make(chan os.Signal), nil, automaticRestartSupervisorDeps{
		runSession: func(*State) error {
			runs++
			ownedAutomaticRestartTestState(t)
			return &automaticRestartRequest{cause: errors.New("network timeout")}
		},
		waitNetwork:       func(time.Duration) os.Signal { return syscall.SIGTERM },
		waitInterfaceGone: func() (os.Signal, error) { return nil, nil },
	})
	if err != nil {
		t.Fatalf("cancel automatic restart: %v", err)
	}
	if runs != 1 {
		t.Fatalf("session runs after cancellation = %d", runs)
	}
	if _, err := os.Stat(statePath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("state after cancellation = %v", err)
	}
}

func TestAutomaticRestartSupervisorDoesNotRetryFatalSessionError(t *testing.T) {
	t.Setenv("TUNSCOPE_STATE_DIR", t.TempDir())
	if err := acquireLock(); err != nil {
		t.Fatal(err)
	}
	defer releaseLock()
	app := &App{runner: &cleanupRunner{}, out: &bytes.Buffer{}, errOut: &bytes.Buffer{}}
	runs := 0
	err := app.superviseAutomaticRestart(make(chan os.Signal), nil, automaticRestartSupervisorDeps{
		runSession: func(*State) error {
			runs++
			if runs == 1 {
				ownedAutomaticRestartTestState(t)
				return &automaticRestartRequest{cause: errors.New("network timeout")}
			}
			return errors.New("persist route journal: disk unavailable")
		},
		waitNetwork:       func(time.Duration) os.Signal { return nil },
		waitInterfaceGone: func() (os.Signal, error) { return nil, nil },
	})
	if err == nil || !strings.Contains(err.Error(), "disk unavailable") {
		t.Fatalf("fatal restart result = %v", err)
	}
	if runs != 2 {
		t.Fatalf("session runs after fatal error = %d", runs)
	}
	if _, err := os.Stat(statePath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("automatic restart marker after fatal error = %v", err)
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

func TestAcquireLockMakesStateMetadataReachableForStatus(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TUNSCOPE_STATE_DIR", dir)
	if err := acquireLock(); err != nil {
		t.Fatal(err)
	}
	defer releaseLock()

	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := dirInfo.Mode().Perm(); got != 0755 {
		t.Fatalf("state directory permissions = %04o, want 0755 for unprivileged status reads", got)
	}

	if err := saveState(cleanupTestState()); err != nil {
		t.Fatal(err)
	}
	stateInfo, err := os.Stat(statePath())
	if err != nil {
		t.Fatal(err)
	}
	if got := stateInfo.Mode().Perm(); got != 0644 {
		t.Fatalf("state permissions = %04o, want 0644 for unprivileged status reads", got)
	}
}
