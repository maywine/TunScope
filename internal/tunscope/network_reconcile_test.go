//go:build darwin

package tunscope

import (
	"bytes"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"strings"
	"testing"
	"time"
)

type recordingRouteRunner struct {
	calls  []string
	failAt int
}

type scriptedRouteRunner struct {
	errs  []error
	calls []string
}

func (r *scriptedRouteRunner) Run(name string, args ...string) (string, error) {
	r.calls = append(r.calls, strings.Join(append([]string{name}, args...), " "))
	if len(r.errs) == 0 {
		return "", nil
	}
	err := r.errs[0]
	r.errs = r.errs[1:]
	return "", err
}

func (r *recordingRouteRunner) Run(name string, args ...string) (string, error) {
	call := strings.Join(append([]string{name}, args...), " ")
	r.calls = append(r.calls, call)
	if r.failAt > 0 && len(r.calls) == r.failAt {
		return "", errors.New("injected routing socket failure")
	}
	return "", nil
}

func reconcileTestConfig() Config {
	cfg := DefaultConfig()
	cfg.IPv6 = false
	cfg.TrustedDNS = ""
	cfg.Applications = []string{"/Applications/Selected.app"}
	return cfg
}

func reconcileTestState(cfg Config, snapshot physicalRouteSnapshot, bypasses []netip.Prefix, dns []netip.Addr) *State {
	managed := physicalRoutesForSnapshot(cfg, snapshot, bypasses, dns)
	tunRoute := Route{Family: "inet", Kind: "net", Target: "1.0.0.0/8", Gateway: tunGateway4, Purpose: "tun"}
	return &State{
		Version:      stateVersion,
		Phase:        "active",
		OwnerPID:     os.Getpid(),
		Device:       "utun123",
		Interface:    snapshot.Interface,
		Interface6:   snapshot.Interface6,
		PhysicalIPv4: append([]string(nil), snapshot.IPv4...),
		PhysicalIPv6: append([]string(nil), snapshot.IPv6...),
		Gateway4:     snapshot.Gateway4,
		Gateway6:     snapshot.Gateway6,
		Routes:       append(managed, tunRoute),
	}
}

func requireNetworkUnavailableSignal(t *testing.T, err error) {
	t.Helper()
	var signal *physicalNetworkUnavailableSignal
	if !errors.As(err, &signal) {
		t.Fatalf("error = %v, want physicalNetworkUnavailableSignal", err)
	}
}

func requirePhysicalNetworkRestart(t *testing.T, err error) {
	t.Helper()
	var restart *physicalNetworkRestartError
	if !errors.As(err, &restart) {
		t.Fatalf("error = %v, want physicalNetworkRestartError", err)
	}
}

func TestNetworkReconcileHandlerPreservesRestartClassification(t *testing.T) {
	app := &App{runner: &recordingRouteRunner{}, out: &bytes.Buffer{}, errOut: &bytes.Buffer{}}
	reconcileErr := restartAfterPhysicalNetworkStabilizes(errors.New("physical interface changed"))
	err := app.handleNetworkReconcileResult(nil, nil, nil, reconcileErr)
	if err == nil || !strings.Contains(err.Error(), "requires a clean TUN rebuild") {
		t.Fatalf("handled restart error = %v", err)
	}
	requirePhysicalNetworkRestart(t, err)
}

func TestStableObservationDebouncesTransientValue(t *testing.T) {
	tracker := newStableObservation(3, "old")
	if tracker.observe("new") || tracker.observe("new") {
		t.Fatal("candidate became stable before the threshold")
	}
	if tracker.observe("old") {
		t.Fatal("returning to the applied value must not emit an update")
	}
	if tracker.observe("new") || tracker.observe("new") || !tracker.observe("new") {
		t.Fatal("three consecutive candidate samples should emit an update")
	}
	tracker.commit("new")
	if tracker.observe("new") {
		t.Fatal("committed value should not be emitted again")
	}
}

func TestTrustedDNSOmitsPhysicalResolverBypass(t *testing.T) {
	cfg := reconcileTestConfig()
	cfg.TrustedDNS = "8.8.8.8:53"
	snapshot := physicalRouteSnapshot{
		Gateway4: "192.168.1.1", Interface: "en0", Source4: "192.168.1.20", IPv4: []string{"192.168.1.20"},
	}
	routes := physicalRoutesForSnapshot(
		cfg,
		snapshot,
		nil,
		[]netip.Addr{netip.MustParseAddr("223.5.5.5")},
	)
	directScopes := 0
	for _, route := range routes {
		if route.Purpose == "dns-direct" {
			t.Fatalf("trusted DNS retained physical resolver bypass: %#v", route)
		}
		if route.Purpose == "direct-scope" {
			directScopes++
		}
	}
	if directScopes != len(ipv4TunNetworks) {
		t.Fatalf("direct-scope routes = %d, want %d", directScopes, len(ipv4TunNetworks))
	}
}

func TestPhysicalRoutesSkipLoopbackDNS(t *testing.T) {
	cfg := reconcileTestConfig()
	snapshot := physicalRouteSnapshot{
		Gateway4: "192.168.1.1", Interface: "en0", Source4: "192.168.1.20", IPv4: []string{"192.168.1.20"},
	}
	routes := physicalRoutesForSnapshot(
		cfg,
		snapshot,
		nil,
		[]netip.Addr{netip.MustParseAddr("127.0.0.1"), netip.MustParseAddr("1.1.1.1")},
	)
	foundExternal := false
	for _, route := range routes {
		if route.Target == "127.0.0.1" {
			t.Fatalf("loopback DNS received a physical route: %#v", route)
		}
		if route.Purpose == "dns-direct" && route.Target == "1.1.1.1" {
			foundExternal = true
		}
	}
	if !foundExternal {
		t.Fatalf("external DNS route is missing: %#v", routes)
	}
}

func TestGlobalDirectICMPGetsScopedRoutesWithoutDirectDNS(t *testing.T) {
	cfg := DefaultConfig()
	cfg.IPv6 = false
	cfg.Applications = nil
	cfg.ICMPDirect = true
	snapshot := physicalRouteSnapshot{
		Gateway4: "192.168.1.1", Interface: "en0", Source4: "192.168.1.20", IPv4: []string{"192.168.1.20"},
	}
	routes := physicalRoutesForSnapshot(
		cfg,
		snapshot,
		nil,
		[]netip.Addr{netip.MustParseAddr("223.5.5.5")},
	)
	directScopes := 0
	for _, route := range routes {
		if route.Purpose == "dns-direct" {
			t.Fatalf("global ICMP mode changed the system DNS path: %#v", route)
		}
		if route.Purpose == "direct-scope" {
			directScopes++
		}
	}
	if directScopes != len(ipv4TunNetworks) {
		t.Fatalf("direct-scope routes = %d, want %d", directScopes, len(ipv4TunNetworks))
	}
}

func TestReplaceMissingRouteAddsExactRoute(t *testing.T) {
	route := Route{
		Family: "inet", Kind: "net", Target: "1.0.0.0/8",
		Gateway: "192.168.50.1", Scope: "en0", Purpose: "direct-scope",
	}
	runner := &scriptedRouteRunner{errs: []error{errors.New("route: writing to routing socket: not in table"), nil}}
	if err := replaceOwnedRoute(runner, route, route); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 2 || !strings.Contains(runner.calls[0], " delete ") || !strings.Contains(runner.calls[1], " add ") {
		t.Fatalf("replacement calls = %#v, want exact delete then add", runner.calls)
	}
}

func TestReplaceRouteAddConflictNeverFallsBackToChange(t *testing.T) {
	route := Route{
		Family: "inet", Kind: "host", Target: "1.1.1.1",
		Gateway: "192.168.50.1", Purpose: "dns-direct",
	}
	conflict := errors.New("route: writing to routing socket: File exists")
	runner := &scriptedRouteRunner{errs: []error{
		errors.New("route: writing to routing socket: not in table"),
		conflict,
	}}
	if err := replaceOwnedRoute(runner, route, route); !errors.Is(err, conflict) {
		t.Fatalf("replacement error = %v, want add conflict", err)
	}
	if len(runner.calls) != 2 || !strings.Contains(runner.calls[0], " delete ") ||
		!strings.Contains(runner.calls[1], " add ") {
		t.Fatalf("raced replacement calls = %#v, want delete/add without change", runner.calls)
	}
}

func TestReplaceRouteStopsAfterDeleteFailure(t *testing.T) {
	route := Route{
		Family: "inet", Kind: "net", Target: "1.0.0.0/8",
		Gateway: "192.168.50.1", Scope: "en0", Purpose: "direct-scope",
	}
	failure := errors.New("route: permission denied")
	runner := &scriptedRouteRunner{errs: []error{failure}}
	if err := replaceOwnedRoute(runner, route, route); !errors.Is(err, failure) {
		t.Fatalf("replacement error = %v, want delete failure", err)
	}
	if len(runner.calls) != 1 || !strings.Contains(runner.calls[0], " delete ") {
		t.Fatalf("calls after delete failure = %#v", runner.calls)
	}
}

func TestReplaceOwnedRouteDeletesOldSourceThenAddsNewSource(t *testing.T) {
	before := Route{
		Family: "inet", Kind: "net", Target: "1.0.0.0/8",
		Gateway: "192.168.50.1", Scope: "en0", Source: "192.168.50.20", Purpose: "direct-scope",
	}
	after := before
	after.Source = "192.168.50.37"
	runner := &scriptedRouteRunner{}
	if err := replaceOwnedRoute(runner, before, after); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 2 || !strings.Contains(runner.calls[0], " delete ") || !strings.Contains(runner.calls[1], " add ") {
		t.Fatalf("replacement calls = %#v, want delete then add", runner.calls)
	}
	if strings.Contains(runner.calls[0], " -ifa ") {
		t.Fatalf("delete retained removed source: %q", runner.calls[0])
	}
	if !strings.Contains(runner.calls[1], "-ifa 192.168.50.37") {
		t.Fatalf("add lacks new source: %q", runner.calls[1])
	}
}

func TestReconcileGatewayChangesDirectRoutesBeforeBypassAndDNS(t *testing.T) {
	t.Setenv("TUNSCOPE_STATE_DIR", t.TempDir())
	cfg := reconcileTestConfig()
	before := physicalRouteSnapshot{Gateway4: "192.168.1.1", Interface: "en0"}
	after := physicalRouteSnapshot{Gateway4: "192.168.50.1", Interface: "en0"}
	bypasses := []netip.Prefix{netip.MustParsePrefix("203.0.113.9/32")}
	dns := []netip.Addr{netip.MustParseAddr("1.1.1.1")}
	state := reconcileTestState(cfg, before, bypasses, dns)
	if err := saveState(state); err != nil {
		t.Fatal(err)
	}
	runner := &recordingRouteRunner{}
	app := &App{runner: runner, out: &bytes.Buffer{}, errOut: &bytes.Buffer{}}

	if err := app.reconcilePhysicalRoutes(state, cfg, after, bypasses, dns, []string{"203.0.113.9"}); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 2*(len(ipv4TunNetworks)+2) {
		t.Fatalf("routing calls = %d, want %d: %#v", len(runner.calls), 2*(len(ipv4TunNetworks)+2), runner.calls)
	}
	for i := 0; i < 2*len(ipv4TunNetworks); i++ {
		action := " delete "
		if i%2 == 1 {
			action = " add "
		}
		if !strings.Contains(runner.calls[i], action) || !strings.Contains(runner.calls[i], "-ifscope en0") {
			t.Fatalf("call %d = %q, want direct-scope route replacement first", i, runner.calls[i])
		}
	}
	if !strings.Contains(runner.calls[2*len(ipv4TunNetworks)], "203.0.113.9") {
		t.Fatalf("call after direct routes = %q, want bypass", runner.calls[2*len(ipv4TunNetworks)])
	}
	if !strings.Contains(runner.calls[len(runner.calls)-1], "1.1.1.1") {
		t.Fatalf("last call = %q, want DNS route", runner.calls[len(runner.calls)-1])
	}

	persisted, err := loadState()
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Phase != "active" || persisted.RouteReconcile != nil || persisted.Gateway4 != after.Gateway4 {
		t.Fatalf("committed state = %#v", persisted)
	}
	for _, route := range managedPhysicalRoutes(persisted.Routes) {
		if route.Family == "inet" && route.Gateway != after.Gateway4 {
			t.Fatalf("route retained stale gateway: %#v", route)
		}
	}
}

func TestReconcileDNSChangeWithSameGateway(t *testing.T) {
	t.Setenv("TUNSCOPE_STATE_DIR", t.TempDir())
	cfg := reconcileTestConfig()
	snapshot := physicalRouteSnapshot{Gateway4: "192.168.1.1", Interface: "en0"}
	oldDNS := []netip.Addr{netip.MustParseAddr("1.1.1.1")}
	newDNS := []netip.Addr{netip.MustParseAddr("9.9.9.9")}
	state := reconcileTestState(cfg, snapshot, nil, oldDNS)
	if err := saveState(state); err != nil {
		t.Fatal(err)
	}
	runner := &recordingRouteRunner{}
	app := &App{runner: runner, out: &bytes.Buffer{}, errOut: &bytes.Buffer{}}

	if err := app.reconcilePhysicalRoutes(state, cfg, snapshot, nil, newDNS, nil); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 2 {
		t.Fatalf("calls = %#v, want one DNS add and one DNS delete", runner.calls)
	}
	if !strings.Contains(runner.calls[0], " add ") || !strings.Contains(runner.calls[0], "9.9.9.9") {
		t.Fatalf("first call = %q, want new DNS added before deletion", runner.calls[0])
	}
	if !strings.Contains(runner.calls[1], " delete ") || !strings.Contains(runner.calls[1], "1.1.1.1") {
		t.Fatalf("second call = %q, want obsolete DNS deleted", runner.calls[1])
	}
}

func TestReconcileExternalDNSToLoopbackDeletesOnlyExternalRoute(t *testing.T) {
	t.Setenv("TUNSCOPE_STATE_DIR", t.TempDir())
	cfg := reconcileTestConfig()
	snapshot := physicalRouteSnapshot{Gateway4: "192.168.1.1", Interface: "en0"}
	oldDNS := []netip.Addr{netip.MustParseAddr("1.1.1.1")}
	localDNS := []netip.Addr{netip.MustParseAddr("127.0.0.1")}
	state := reconcileTestState(cfg, snapshot, nil, oldDNS)
	if err := saveState(state); err != nil {
		t.Fatal(err)
	}
	runner := &recordingRouteRunner{}
	app := &App{runner: runner, out: &bytes.Buffer{}, errOut: &bytes.Buffer{}}

	if err := app.reconcilePhysicalRoutes(state, cfg, snapshot, nil, localDNS, nil); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 1 || !strings.Contains(runner.calls[0], " delete ") || !strings.Contains(runner.calls[0], "1.1.1.1") {
		t.Fatalf("route calls = %#v, want only deletion of the external DNS route", runner.calls)
	}
	for _, route := range state.Routes {
		if route.Target == "127.0.0.1" || route.Purpose == "dns-direct" {
			t.Fatalf("reconciled state retained an invalid DNS route: %#v", route)
		}
	}
}

func TestReconcileLoopbackDNSToExternalAddsOnlyExternalRoute(t *testing.T) {
	t.Setenv("TUNSCOPE_STATE_DIR", t.TempDir())
	cfg := reconcileTestConfig()
	snapshot := physicalRouteSnapshot{Gateway4: "192.168.1.1", Interface: "en0"}
	localDNS := []netip.Addr{netip.MustParseAddr("127.0.0.1")}
	externalDNS := []netip.Addr{netip.MustParseAddr("9.9.9.9")}
	state := reconcileTestState(cfg, snapshot, nil, localDNS)
	if err := saveState(state); err != nil {
		t.Fatal(err)
	}
	runner := &recordingRouteRunner{}
	app := &App{runner: runner, out: &bytes.Buffer{}, errOut: &bytes.Buffer{}}

	if err := app.reconcilePhysicalRoutes(state, cfg, snapshot, nil, externalDNS, nil); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 1 || !strings.Contains(runner.calls[0], " add ") || !strings.Contains(runner.calls[0], "9.9.9.9") {
		t.Fatalf("route calls = %#v, want only addition of the external DNS route", runner.calls)
	}
}

func TestReconcileAddressChangeForcesRouteRefreshWithSameGateway(t *testing.T) {
	t.Setenv("TUNSCOPE_STATE_DIR", t.TempDir())
	cfg := reconcileTestConfig()
	before := physicalRouteSnapshot{
		Gateway4: "192.168.1.1", Interface: "en0", Source4: "192.168.1.20", IPv4: []string{"192.168.1.20"},
	}
	after := physicalRouteSnapshot{
		Gateway4: "192.168.1.1", Interface: "en0", Source4: "192.168.1.37", IPv4: []string{"192.168.1.37"},
	}
	bypasses := []netip.Prefix{netip.MustParsePrefix("203.0.113.9/32")}
	dns := []netip.Addr{netip.MustParseAddr("1.1.1.1")}
	state := reconcileTestState(cfg, before, bypasses, dns)
	if err := saveState(state); err != nil {
		t.Fatal(err)
	}
	runner := &recordingRouteRunner{}
	app := &App{runner: runner, out: &bytes.Buffer{}, errOut: &bytes.Buffer{}}

	if err := app.reconcilePhysicalRoutes(state, cfg, after, bypasses, dns, nil); err != nil {
		t.Fatal(err)
	}
	wantChanges := 2 * (len(ipv4TunNetworks) + 2) // delete/add direct-scope + bypass + DNS
	if len(runner.calls) != wantChanges {
		t.Fatalf("address refresh calls = %d, want %d: %#v", len(runner.calls), wantChanges, runner.calls)
	}
	for i, call := range runner.calls {
		if i%2 == 0 {
			if !strings.Contains(call, " delete ") || strings.Contains(call, " -ifa ") {
				t.Fatalf("old source route was not deleted cleanly: %q", call)
			}
		} else if !strings.Contains(call, " add ") || !strings.Contains(call, "-ifa 192.168.1.37") {
			t.Fatalf("replacement route did not attach the new physical source: %q", call)
		}
	}
	if got := stringSetSignature(state.PhysicalIPv4); got != "192.168.1.37" {
		t.Fatalf("committed physical IPv4 = %q", got)
	}
}

func TestMonitorReconcilesAddressAndRequestsFlowRebindWithoutDroppingTUN(t *testing.T) {
	t.Setenv("TUNSCOPE_STATE_DIR", t.TempDir())
	cfg := reconcileTestConfig()
	before := physicalRouteSnapshot{
		Gateway4: "192.168.50.1", Interface: "en0", Source4: "192.168.50.20", IPv4: []string{"192.168.50.20"},
	}
	state := reconcileTestState(cfg, before, nil, nil)
	if err := saveState(state); err != nil {
		t.Fatal(err)
	}
	runner := &liveMonitorRunner{
		routeGateway: "192.168.50.1",
		routeIface:   "en0",
		dnsServer:    "1.1.1.1",
		ipv4Address:  "192.168.50.37",
	}
	app := &App{runner: runner, out: &bytes.Buffer{}, errOut: &bytes.Buffer{}}
	monitor := newLiveNetworkMonitor(before, nil, nil, nil, false, true, false, 7890)

	var err error
	for i := 0; i < addressStableSampleCount; i++ {
		_, err = monitor.poll(app, state, cfg)
		if i < addressStableSampleCount-1 && err != nil {
			t.Fatalf("poll %d returned before address became stable: %v", i, err)
		}
	}
	var change *physicalNetworkChangeError
	if !errors.As(err, &change) {
		t.Fatalf("poll error = %v, want physicalNetworkChangeError", err)
	}
	if change.Source4 != "192.168.50.37" {
		t.Fatalf("change = %#v", change)
	}
	if state.Phase != "active" || state.Gateway4 != before.Gateway4 || len(state.PhysicalIPv4) != 1 || state.PhysicalIPv4[0] != "192.168.50.37" {
		t.Fatalf("reconciled state = %#v", state)
	}
	mutations := 0
	for _, call := range runner.calls {
		if !strings.HasPrefix(call, "/sbin/route -n delete ") && !strings.HasPrefix(call, "/sbin/route -n add ") {
			continue
		}
		mutations++
		if !strings.Contains(call, "-ifscope en0") {
			t.Fatalf("address handoff touched a non-scoped route (possibly TUN capture): %q", call)
		}
		if strings.HasPrefix(call, "/sbin/route -n add ") && !strings.Contains(call, "-ifa 192.168.50.37") {
			t.Fatalf("replacement route lacks the new source: %q", call)
		}
	}
	if mutations != 2*len(ipv4TunNetworks) {
		t.Fatalf("physical route mutations = %d, want %d", mutations, 2*len(ipv4TunNetworks))
	}
	if _, err := monitor.poll(app, state, cfg); err != nil {
		t.Fatalf("committed address change repeated on next poll: %v", err)
	}
}

func TestPrimaryInterfaceIPv4UsesIPConfigAddress(t *testing.T) {
	runner := &liveMonitorRunner{ipv4Address: "192.168.50.37"}
	got, err := primaryInterfaceIPv4(runner, "en0", []string{"169.254.10.2", "192.168.50.37"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "192.168.50.37" {
		t.Fatalf("primary address = %q", got)
	}
	if _, err := primaryInterfaceIPv4(runner, "en0", []string{"192.168.50.99"}); err == nil {
		t.Fatal("expected an error when ipconfig returns an address not assigned to the interface")
	}
}

func TestVerifyDirectScopedRoutesRejectsDefaultRouteFallback(t *testing.T) {
	runner := &liveMonitorRunner{
		routeGateway:      "192.168.1.1",
		routeIface:        "en0",
		scopedRouteTarget: "default",
		scopedRouteMask:   "default",
	}
	routes := []Route{{
		Family: "inet", Kind: "net", Target: "1.0.0.0/8",
		Gateway: "192.168.1.1", Scope: "en0", Purpose: "direct-scope",
	}}
	err := verifyDirectScopedRoutes(runner, routes)
	if err == nil || !strings.Contains(err.Error(), "resolved as 0.0.0.0/0") {
		t.Fatalf("default-route fallback verification error = %v", err)
	}
}

func TestFailedReconcileJournalCleansBothGatewayCandidates(t *testing.T) {
	t.Setenv("TUNSCOPE_STATE_DIR", t.TempDir())
	cfg := reconcileTestConfig()
	before := physicalRouteSnapshot{Gateway4: "192.168.1.1", Interface: "en0"}
	after := physicalRouteSnapshot{Gateway4: "192.168.50.1", Interface: "en0"}
	bypasses := []netip.Prefix{netip.MustParsePrefix("203.0.113.9/32")}
	dns := []netip.Addr{netip.MustParseAddr("1.1.1.1")}
	state := reconcileTestState(cfg, before, bypasses, dns)
	if err := saveState(state); err != nil {
		t.Fatal(err)
	}
	failingRunner := &recordingRouteRunner{failAt: 2}
	app := &App{runner: failingRunner, out: &bytes.Buffer{}, errOut: &bytes.Buffer{}}

	err := app.reconcilePhysicalRoutes(state, cfg, after, bypasses, dns, nil)
	if err == nil || !strings.Contains(err.Error(), "injected routing socket failure") {
		t.Fatalf("reconcile error = %v", err)
	}
	persisted, loadErr := loadState()
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if persisted.Phase != "reconciling" || persisted.RouteReconcile == nil {
		t.Fatalf("persisted recovery journal = %#v", persisted)
	}
	if len(persisted.RouteReconcile.Before) == 0 || len(persisted.RouteReconcile.After) == 0 {
		t.Fatalf("journal did not retain both route sets: %#v", persisted.RouteReconcile)
	}

	cleanupRunner := &recordingRouteRunner{}
	cleanupApp := &App{runner: cleanupRunner, out: &bytes.Buffer{}, errOut: &bytes.Buffer{}}
	if err := cleanupApp.cleanup(persisted, nil); err != nil {
		t.Fatalf("cleanup journal: %v", err)
	}
	if _, err := os.Stat(statePath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("state after journal cleanup: %v, want removed", err)
	}
	joined := strings.Join(cleanupRunner.calls, "\n")
	if !strings.Contains(joined, before.Gateway4) || !strings.Contains(joined, after.Gateway4) {
		t.Fatalf("cleanup calls did not cover old and new gateways:\n%s", joined)
	}
}

type liveMonitorRunner struct {
	routeGateway       string
	routeIface         string
	routeOutput        string
	routeErr           error
	scopedRouteGateway string
	scopedRouteIface   string
	scopedRouteTarget  string
	scopedRouteMask    string
	scopedRouteErr     error
	dnsServer          string
	dnsOutput          string
	dnsErr             error
	ipv4Address        string
	calls              []string
}

func (r *liveMonitorRunner) Run(name string, args ...string) (string, error) {
	call := strings.Join(append([]string{name}, args...), " ")
	r.calls = append(r.calls, call)
	if name == "/sbin/route" && len(args) >= 3 && args[1] == "get" && args[2] == "default" {
		if r.routeErr != nil {
			return "", r.routeErr
		}
		if r.routeOutput != "" {
			return r.routeOutput, nil
		}
		return fmt.Sprintf("gateway: %s\ninterface: %s\n", r.routeGateway, r.routeIface), nil
	}
	if name == "/sbin/route" && len(args) >= 6 && args[1] == "get" && args[2] == "-net" && args[3] == "-ifscope" {
		if r.scopedRouteErr != nil {
			return "", r.scopedRouteErr
		}
		gateway := r.scopedRouteGateway
		if gateway == "" {
			gateway = r.routeGateway
		}
		iface := r.scopedRouteIface
		if iface == "" {
			iface = r.routeIface
		}
		destination := r.scopedRouteTarget
		mask := r.scopedRouteMask
		if destination == "" || mask == "" {
			prefix, parseErr := netip.ParsePrefix(args[5])
			if parseErr != nil || !prefix.Addr().Is4() {
				return "", fmt.Errorf("invalid scoped network route %q", args[5])
			}
			destination = prefix.Masked().Addr().String()
			bits := prefix.Bits()
			maskValue := uint32(0)
			if bits > 0 {
				maskValue = ^uint32(0) << (32 - bits)
			}
			mask = fmt.Sprintf("%d.%d.%d.%d", byte(maskValue>>24), byte(maskValue>>16), byte(maskValue>>8), byte(maskValue))
		}
		return fmt.Sprintf("destination: %s\nmask: %s\ngateway: %s\ninterface: %s\n", destination, mask, gateway, iface), nil
	}
	if name == "/usr/sbin/scutil" {
		if r.dnsErr != nil {
			return "", r.dnsErr
		}
		if r.dnsOutput != "" {
			return r.dnsOutput, nil
		}
		return fmt.Sprintf("nameserver[0] : %s\n", r.dnsServer), nil
	}
	if name == "/sbin/ifconfig" {
		address := r.ipv4Address
		if address == "" {
			address = "192.168.50.20"
		}
		return fmt.Sprintf("\tinet %s netmask 0xffffff00 broadcast 192.168.50.255\n", address), nil
	}
	if name == "/usr/sbin/ipconfig" {
		address := r.ipv4Address
		if address == "" {
			address = "192.168.50.20"
		}
		return address + "\n", nil
	}
	if name == "/usr/sbin/lsof" {
		return "COMMAND PID USER FD TYPE DEVICE SIZE/OFF NODE NAME\n", nil
	}
	return "", nil
}

func TestMonitorKeepsTUNDuringTransientMissingDefaultRouteAndRecovers(t *testing.T) {
	t.Setenv("TUNSCOPE_STATE_DIR", t.TempDir())
	cfg := reconcileTestConfig()
	before := physicalRouteSnapshot{
		Gateway4: "192.168.1.1", Interface: "en0", Source4: "192.168.1.20", IPv4: []string{"192.168.1.20"},
	}
	state := reconcileTestState(cfg, before, nil, nil)
	if err := saveState(state); err != nil {
		t.Fatal(err)
	}
	runner := &liveMonitorRunner{
		routeGateway: "192.168.50.1",
		routeIface:   "en0",
		routeOutput:  "route to: default\n",
		dnsServer:    "1.1.1.1",
		ipv4Address:  "192.168.50.37",
	}
	app := &App{runner: runner, out: &bytes.Buffer{}, errOut: &bytes.Buffer{}}
	monitor := newLiveNetworkMonitor(before, nil, nil, nil, false, true, false, 0)
	monitor.fullWake = func() bool { return true }

	for i := 0; i < 3*networkStableSampleCount; i++ {
		updates, err := monitor.poll(app, state, cfg)
		if i == 0 {
			requireNetworkUnavailableSignal(t, err)
		} else if err != nil {
			t.Fatalf("missing-route poll %d stopped TUN: %v", i, err)
		}
		if i == 0 {
			if len(updates) != 1 || !strings.Contains(updates[0], "temporarily unavailable") {
				t.Fatalf("first missing-route update = %#v", updates)
			}
		} else if len(updates) != 0 {
			t.Fatalf("repeated missing-route update %d = %#v", i, updates)
		}
	}
	if state.Phase != "active" || state.Gateway4 != before.Gateway4 {
		t.Fatalf("state changed while route was unavailable: %#v", state)
	}

	runner.routeOutput = ""
	var recoveryErr error
	for i := 0; i < networkRecoverySampleCount+1; i++ {
		updates, err := monitor.poll(app, state, cfg)
		recoveryErr = err
		switch {
		case i == 0 && (len(updates) != 1 || !strings.Contains(updates[0], "available again")):
			t.Fatalf("first recovery update = %#v", updates)
		case i < networkRecoverySampleCount-1 && err != nil:
			t.Fatalf("recovery poll %d returned before stability threshold: %v", i, err)
		case i == networkRecoverySampleCount-1 && (err != nil || len(updates) != 1 || !strings.Contains(updates[0], "waiting one poll")):
			t.Fatalf("route preparation poll = updates %#v, error %v", updates, err)
		}
	}
	var change *physicalNetworkChangeError
	if !errors.As(recoveryErr, &change) {
		t.Fatalf("recovery error = %v, want physicalNetworkChangeError", recoveryErr)
	}
	if change.Source4 != "192.168.50.37" || state.Gateway4 != "192.168.50.1" {
		t.Fatalf("recovered change/state = %#v / %#v", change, state)
	}
}

func TestMonitorRepairsScopedRoutesRemovedDuringRecoveryVerification(t *testing.T) {
	t.Setenv("TUNSCOPE_STATE_DIR", t.TempDir())
	cfg := reconcileTestConfig()
	before := physicalRouteSnapshot{
		Gateway4: "192.168.1.1", Interface: "en0", Source4: "192.168.1.20", IPv4: []string{"192.168.1.20"},
	}
	state := reconcileTestState(cfg, before, nil, nil)
	if err := saveState(state); err != nil {
		t.Fatal(err)
	}
	runner := &liveMonitorRunner{
		routeGateway: "192.168.50.1",
		routeIface:   "en0",
		routeOutput:  "route to: default\n",
		dnsServer:    "1.1.1.1",
		ipv4Address:  "192.168.50.37",
	}
	app := &App{runner: runner, out: &bytes.Buffer{}, errOut: &bytes.Buffer{}}
	monitor := newLiveNetworkMonitor(before, nil, nil, nil, false, true, false, 0)

	_, unavailableErr := monitor.poll(app, state, cfg)
	requireNetworkUnavailableSignal(t, unavailableErr)
	runner.routeOutput = ""
	for i := 0; i < networkRecoverySampleCount; i++ {
		if _, err := monitor.poll(app, state, cfg); err != nil {
			t.Fatalf("route preparation poll %d: %v", i, err)
		}
	}
	if monitor.recoveryPrepared == "" {
		t.Fatal("recovery routes were not marked prepared")
	}

	runner.scopedRouteErr = errors.New("route: writing to routing socket: not in table")
	updates, err := monitor.poll(app, state, cfg)
	if err != nil {
		t.Fatalf("a flushed scoped route stopped TUN immediately: %v", err)
	}
	if len(updates) != 1 || !strings.Contains(updates[0], "removed a replacement scoped route") {
		t.Fatalf("scoped-route flush update = %#v", updates)
	}
	if monitor.recoveryPrepared != "" {
		t.Fatal("flushed scoped routes remained marked prepared")
	}

	runner.scopedRouteErr = nil
	updates, err = monitor.poll(app, state, cfg)
	if err != nil || len(updates) != 1 || !strings.Contains(updates[0], "waiting one poll") {
		t.Fatalf("route repair poll = updates %#v, error %v", updates, err)
	}
	_, recoveryErr := monitor.poll(app, state, cfg)
	var change *physicalNetworkChangeError
	if !errors.As(recoveryErr, &change) {
		t.Fatalf("repaired recovery error = %v, want physicalNetworkChangeError", recoveryErr)
	}
}

func TestMonitorRepairsScopedRoutesLostDuringSleepWithoutRouteChange(t *testing.T) {
	t.Setenv("TUNSCOPE_STATE_DIR", t.TempDir())
	cfg := reconcileTestConfig()
	snapshot := physicalRouteSnapshot{
		Gateway4: "192.168.1.1", Interface: "en0", Source4: "192.168.1.20", IPv4: []string{"192.168.1.20"},
	}
	state := reconcileTestState(cfg, snapshot, nil, nil)
	if err := saveState(state); err != nil {
		t.Fatal(err)
	}
	runner := &liveMonitorRunner{
		routeGateway: snapshot.Gateway4,
		routeIface:   snapshot.Interface,
		dnsServer:    "1.1.1.1",
		ipv4Address:  snapshot.Source4,
	}
	app := &App{runner: runner, out: &bytes.Buffer{}, errOut: &bytes.Buffer{}}
	monitor := newLiveNetworkMonitor(snapshot, nil, nil, nil, false, true, false, 0)
	now := time.Unix(1_700_000_000, 0)
	monitor.now = func() time.Time { return now }

	if _, err := monitor.poll(app, state, cfg); err != nil {
		t.Fatalf("initial route audit: %v", err)
	}
	now = now.Add(networkWakePollGap + time.Millisecond)
	runner.scopedRouteErr = errors.New("route: writing to routing socket: not in table")
	updates, err := monitor.poll(app, state, cfg)
	requireNetworkUnavailableSignal(t, err)
	if len(updates) != 1 || !strings.Contains(updates[0], "after system wake") {
		t.Fatalf("wake route-loss updates = %#v", updates)
	}
	if !monitor.recoveryPending || monitor.routeUnavailable {
		t.Fatalf("wake route loss did not enter scoped-route recovery: %#v", monitor)
	}
	if !monitor.recoveryStartedAt.Equal(now) {
		t.Fatalf("recovery started at %s, want %s", monitor.recoveryStartedAt, now)
	}

	if _, suspendErr := app.suspendOwnedRoutes(state); suspendErr != nil {
		t.Fatalf("suspend routes after wake route loss: %v", suspendErr)
	}
	runner.scopedRouteErr = nil
	var recoveryErr error
	for range networkRecoverySampleCount + 1 {
		now = now.Add(networkPollInterval)
		_, recoveryErr = monitor.poll(app, state, cfg)
	}
	var change *physicalNetworkChangeError
	if !errors.As(recoveryErr, &change) {
		t.Fatalf("same-signature wake recovery error = %v, want physicalNetworkChangeError", recoveryErr)
	}
	if change.Source4 != snapshot.Source4 || monitor.recoveryPending {
		t.Fatalf("same-signature wake recovery = %#v / monitor %#v", change, monitor)
	}
}

func TestMonitorRefreshesSameSignatureAfterPhysicalNetworkEvent(t *testing.T) {
	t.Setenv("TUNSCOPE_STATE_DIR", t.TempDir())
	cfg := reconcileTestConfig()
	snapshot := physicalRouteSnapshot{
		Gateway4: "192.168.1.1", Interface: "en0", Source4: "192.168.1.20", IPv4: []string{"192.168.1.20"},
	}
	state := reconcileTestState(cfg, snapshot, nil, nil)
	if err := saveState(state); err != nil {
		t.Fatal(err)
	}
	runner := &liveMonitorRunner{
		routeGateway: snapshot.Gateway4,
		routeIface:   snapshot.Interface,
		dnsServer:    "1.1.1.1",
		ipv4Address:  snapshot.Source4,
	}
	app := &App{runner: runner, out: &bytes.Buffer{}, errOut: &bytes.Buffer{}}
	monitor := newLiveNetworkMonitor(snapshot, nil, nil, nil, false, true, false, 0)
	now := time.Unix(1_700_000_000, 0)
	monitor.now = func() time.Time { return now }

	updates, err := monitor.handlePhysicalNetworkEvent("macOS reported a Wi-Fi link epoch change on en0")
	requireNetworkUnavailableSignal(t, err)
	if len(updates) != 1 || !strings.Contains(updates[0], "pausing TUN capture") {
		t.Fatalf("first physical-network event updates = %#v", updates)
	}
	if !monitor.recoveryPending || !monitor.recoveryStartedAt.Equal(now) {
		t.Fatalf("physical-network event did not enter recovery: %#v", monitor)
	}
	if _, suspendErr := app.suspendOwnedRoutes(state); suspendErr != nil {
		t.Fatalf("suspend routes after physical-network event: %v", suspendErr)
	}

	// Two apparently stable samples are not enough to recover. A later DHCP
	// publication from the same roam must restart the stability window even
	// though the address and gateway remain identical.
	for range networkRecoverySampleCount - 1 {
		now = now.Add(networkPollInterval)
		if _, pollErr := monitor.poll(app, state, cfg); pollErr != nil {
			t.Fatalf("pre-DHCP stability poll: %v", pollErr)
		}
	}
	now = now.Add(100 * time.Millisecond)
	updates, err = monitor.handlePhysicalNetworkEvent("macOS reported a DHCP epoch change on en0")
	if err != nil || len(updates) != 1 || !strings.Contains(updates[0], "restarting the physical-route stability window") {
		t.Fatalf("repeated physical-network event = updates %#v, error %v", updates, err)
	}
	if !monitor.recoveryStartedAt.Equal(now) || monitor.recoveryPrepared != "" {
		t.Fatalf("repeated event did not reset recovery: %#v", monitor)
	}

	var recoveryErr error
	for range networkRecoverySampleCount + 1 {
		now = now.Add(networkPollInterval)
		_, recoveryErr = monitor.poll(app, state, cfg)
	}
	var change *physicalNetworkChangeError
	if !errors.As(recoveryErr, &change) {
		t.Fatalf("same-signature event recovery error = %v, want physicalNetworkChangeError", recoveryErr)
	}
	if change.Source4 != snapshot.Source4 || monitor.recoveryPending {
		t.Fatalf("same-signature event recovery = %#v / monitor %#v", change, monitor)
	}
	if !state.RoutesSuspended {
		t.Fatal("monitor restored capture routes before the engine rebind acknowledgement")
	}
}

func TestMonitorRecoversWhenBoundRouteProbeFailsButRoutesRemainListed(t *testing.T) {
	cfg := reconcileTestConfig()
	snapshot := physicalRouteSnapshot{
		Gateway4: "192.168.1.1", Interface: "en0", Source4: "192.168.1.20", IPv4: []string{"192.168.1.20"},
	}
	state := reconcileTestState(cfg, snapshot, nil, nil)
	runner := &liveMonitorRunner{
		routeGateway: snapshot.Gateway4,
		routeIface:   snapshot.Interface,
		dnsServer:    "1.1.1.1",
		ipv4Address:  snapshot.Source4,
	}
	probeCalls := 0
	app := &App{
		runner: runner, out: &bytes.Buffer{}, errOut: &bytes.Buffer{},
		directRouteProbe: func(interfaceName, source4 string) error {
			probeCalls++
			if interfaceName != snapshot.Interface || source4 != snapshot.Source4 {
				t.Fatalf("probe route = %s/%s, want %s/%s", interfaceName, source4, snapshot.Interface, snapshot.Source4)
			}
			return errors.New("connect: network is unreachable")
		},
	}
	monitor := newLiveNetworkMonitor(snapshot, nil, nil, nil, false, true, false, 0)

	updates, err := monitor.poll(app, state, cfg)
	requireNetworkUnavailableSignal(t, err)
	if probeCalls != 1 {
		t.Fatalf("bound route probe calls = %d, want 1", probeCalls)
	}
	if len(updates) != 1 || !strings.Contains(updates[0], "bound direct route probe failed") {
		t.Fatalf("bound route failure updates = %#v", updates)
	}
	if !monitor.recoveryPending {
		t.Fatal("bound route failure did not enter recovery")
	}
}

func TestMonitorRechecksScopedRoutesAfterWakeAuditSucceeds(t *testing.T) {
	cfg := reconcileTestConfig()
	snapshot := physicalRouteSnapshot{
		Gateway4: "192.168.1.1", Interface: "en0", Source4: "192.168.1.20", IPv4: []string{"192.168.1.20"},
	}
	state := reconcileTestState(cfg, snapshot, nil, nil)
	runner := &liveMonitorRunner{
		routeGateway: snapshot.Gateway4,
		routeIface:   snapshot.Interface,
		dnsServer:    "1.1.1.1",
		ipv4Address:  snapshot.Source4,
	}
	app := &App{runner: runner, out: &bytes.Buffer{}, errOut: &bytes.Buffer{}}
	monitor := newLiveNetworkMonitor(snapshot, nil, nil, nil, false, true, false, 0)
	now := time.Unix(1_700_000_000, 0)
	monitor.now = func() time.Time { return now }

	if _, err := monitor.poll(app, state, cfg); err != nil {
		t.Fatal(err)
	}
	now = now.Add(networkWakePollGap + time.Millisecond)
	updates, err := monitor.poll(app, state, cfg)
	if err != nil || len(updates) != 1 || !strings.Contains(updates[0], "will be rechecked") {
		t.Fatalf("successful first wake audit = updates %#v, error %v", updates, err)
	}
	if monitor.postWakeRouteAudits != postWakeRouteAuditSamples {
		t.Fatalf("remaining post-wake audits = %d", monitor.postWakeRouteAudits)
	}

	// macOS may flush manually-added routes shortly after publishing the same
	// default route. The next normal poll must audit again instead of waiting for
	// the five-second steady-state interval.
	now = now.Add(networkPollInterval)
	runner.scopedRouteErr = errors.New("route disappeared after wake")
	updates, err = monitor.poll(app, state, cfg)
	requireNetworkUnavailableSignal(t, err)
	if len(updates) != 1 || !strings.Contains(updates[0], "after system wake") {
		t.Fatalf("delayed wake flush updates = %#v", updates)
	}
}

func TestMonitorAuditsScopedRoutesDuringSteadyState(t *testing.T) {
	cfg := reconcileTestConfig()
	snapshot := physicalRouteSnapshot{
		Gateway4: "192.168.1.1", Interface: "en0", Source4: "192.168.1.20", IPv4: []string{"192.168.1.20"},
	}
	dns := []netip.Addr{netip.MustParseAddr("1.1.1.1")}
	state := reconcileTestState(cfg, snapshot, nil, dns)
	runner := &liveMonitorRunner{
		routeGateway: snapshot.Gateway4,
		routeIface:   snapshot.Interface,
		dnsServer:    "1.1.1.1",
		ipv4Address:  snapshot.Source4,
	}
	app := &App{runner: runner, out: &bytes.Buffer{}, errOut: &bytes.Buffer{}}
	monitor := newLiveNetworkMonitor(snapshot, dns, nil, nil, false, true, false, 0)
	now := time.Unix(1_700_000_000, 0)
	monitor.now = func() time.Time { return now }

	if _, err := monitor.poll(app, state, cfg); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Duration(0)
	for elapsed+networkPollInterval < directRouteAuditInterval {
		now = now.Add(networkPollInterval)
		elapsed += networkPollInterval
		if _, err := monitor.poll(app, state, cfg); err != nil {
			t.Fatalf("steady poll at %s: %v", elapsed, err)
		}
	}
	runner.scopedRouteErr = errors.New("route disappeared outside a handoff")
	now = now.Add(networkPollInterval)
	updates, err := monitor.poll(app, state, cfg)
	requireNetworkUnavailableSignal(t, err)
	if len(updates) != 1 || !strings.Contains(updates[0], "steady-state route audit") {
		t.Fatalf("steady route-loss updates = %#v", updates)
	}
}

func TestMonitorStopsWhenReplacementScopedRoutesRemainUnusable(t *testing.T) {
	t.Setenv("TUNSCOPE_STATE_DIR", t.TempDir())
	cfg := reconcileTestConfig()
	before := physicalRouteSnapshot{
		Gateway4: "192.168.1.1", Interface: "en0", Source4: "192.168.1.20", IPv4: []string{"192.168.1.20"},
	}
	state := reconcileTestState(cfg, before, nil, nil)
	if err := saveState(state); err != nil {
		t.Fatal(err)
	}
	runner := &liveMonitorRunner{
		routeGateway:   "192.168.50.1",
		routeIface:     "en0",
		routeOutput:    "route to: default\n",
		dnsServer:      "1.1.1.1",
		ipv4Address:    "192.168.50.37",
		scopedRouteErr: errors.New("route: writing to routing socket: not in table"),
	}
	app := &App{runner: runner, out: &bytes.Buffer{}, errOut: &bytes.Buffer{}}
	monitor := newLiveNetworkMonitor(before, nil, nil, nil, false, true, false, 0)
	now := time.Unix(1_700_000_000, 0)
	monitor.now = func() time.Time { return now }

	_, unavailableErr := monitor.poll(app, state, cfg)
	requireNetworkUnavailableSignal(t, unavailableErr)
	runner.routeOutput = ""
	for i := 0; i < networkRecoverySampleCount; i++ {
		if _, err := monitor.poll(app, state, cfg); err != nil {
			t.Fatalf("recovery preparation poll %d: %v", i, err)
		}
	}
	now = now.Add(networkRecoveryGrace)
	_, err := monitor.poll(app, state, cfg)
	if err == nil || !strings.Contains(err.Error(), "replacement physical routes remained unusable") {
		t.Fatalf("expired scoped-route recovery error = %v", err)
	}
	requirePhysicalNetworkRestart(t, err)
}

func TestMonitorPausesImmediatelyEvenIfPreviousSourceRemainsAssigned(t *testing.T) {
	cfg := reconcileTestConfig()
	before := physicalRouteSnapshot{
		Gateway4: "192.168.1.1", Interface: "en0", Source4: "192.168.1.20", IPv4: []string{"192.168.1.20"},
	}
	state := reconcileTestState(cfg, before, nil, nil)
	runner := &liveMonitorRunner{
		routeOutput: "route to: default\n",
		ipv4Address: before.Source4,
	}
	app := &App{runner: runner, out: &bytes.Buffer{}, errOut: &bytes.Buffer{}}
	monitor := newLiveNetworkMonitor(before, nil, nil, nil, false, true, false, 0)

	updates, err := monitor.poll(app, state, cfg)
	requireNetworkUnavailableSignal(t, err)
	if len(updates) != 1 || !strings.Contains(updates[0], "pausing TUN capture") {
		t.Fatalf("assigned-source updates = %#v", updates)
	}
	if !monitor.routeUnavailable || !monitor.recoveryPending {
		t.Fatalf("route gap was not recorded: %#v", monitor)
	}
}

func TestMonitorStopsAfterPhysicalNetworkUnavailableGrace(t *testing.T) {
	cfg := reconcileTestConfig()
	before := physicalRouteSnapshot{
		Gateway4: "192.168.1.1", Interface: "en0", Source4: "192.168.1.20", IPv4: []string{"192.168.1.20"},
	}
	state := reconcileTestState(cfg, before, nil, nil)
	runner := &liveMonitorRunner{routeOutput: "route to: default\n"}
	app := &App{runner: runner, out: &bytes.Buffer{}, errOut: &bytes.Buffer{}}
	monitor := newLiveNetworkMonitor(before, nil, nil, nil, false, true, false, 0)
	now := time.Unix(1_700_000_000, 0)
	monitor.now = func() time.Time { return now }
	monitor.fullWake = func() bool { return true }

	_, err := monitor.poll(app, state, cfg)
	requireNetworkUnavailableSignal(t, err)
	for monitor.routeUnavailableFor+networkPollInterval < networkUnavailableGrace {
		now = now.Add(networkPollInterval)
		if _, err := monitor.poll(app, state, cfg); err != nil {
			t.Fatalf("sample inside unavailable grace stopped TUN at %s: %v", monitor.routeUnavailableFor, err)
		}
	}
	now = now.Add(networkPollInterval)
	_, err = monitor.poll(app, state, cfg)
	var change *physicalNetworkChangeError
	if err == nil || errors.As(err, &change) || !strings.Contains(err.Error(), "remained unavailable") {
		t.Fatalf("expired unavailable grace error = %v", err)
	}
	requirePhysicalNetworkRestart(t, err)
}

func TestMonitorRequestsFullRestartWhenPhysicalInterfaceChanges(t *testing.T) {
	cfg := reconcileTestConfig()
	before := physicalRouteSnapshot{
		Gateway4: "192.168.1.1", Interface: "en0", Source4: "192.168.1.20", IPv4: []string{"192.168.1.20"},
	}
	state := reconcileTestState(cfg, before, nil, nil)
	runner := &liveMonitorRunner{
		routeGateway: "192.168.1.1",
		routeIface:   "en7",
		ipv4Address:  "192.168.1.20",
	}
	app := &App{runner: runner, out: &bytes.Buffer{}, errOut: &bytes.Buffer{}}
	monitor := newLiveNetworkMonitor(before, nil, nil, nil, false, true, false, 0)

	for i := 0; i < networkStableSampleCount-1; i++ {
		if _, err := monitor.poll(app, state, cfg); err != nil {
			t.Fatalf("interface candidate poll %d: %v", i, err)
		}
	}
	_, err := monitor.poll(app, state, cfg)
	if err == nil || !strings.Contains(err.Error(), "interface changed from en0 to en7") {
		t.Fatalf("interface-change error = %v", err)
	}
	requirePhysicalNetworkRestart(t, err)
}

func TestMonitorPausesUnavailableGraceDuringSleepAndDarkWake(t *testing.T) {
	cfg := reconcileTestConfig()
	before := physicalRouteSnapshot{
		Gateway4: "192.168.1.1", Interface: "en0", Source4: "192.168.1.20", IPv4: []string{"192.168.1.20"},
	}
	state := reconcileTestState(cfg, before, nil, nil)
	runner := &liveMonitorRunner{routeOutput: "route to: default\n"}
	app := &App{runner: runner, out: &bytes.Buffer{}, errOut: &bytes.Buffer{}}
	monitor := newLiveNetworkMonitor(before, nil, nil, nil, false, true, false, 0)
	now := time.Unix(1_700_000_000, 0)
	fullWake := false
	monitor.now = func() time.Time { return now }
	monitor.fullWake = func() bool { return fullWake }

	updates, err := monitor.poll(app, state, cfg)
	requireNetworkUnavailableSignal(t, err)
	if len(updates) != 2 || !strings.Contains(updates[1], "pausing") {
		t.Fatalf("dark-wake updates = %#v", updates)
	}
	for i := 0; i < 10; i++ {
		now = now.Add(networkUnavailableGrace)
		if _, err := monitor.poll(app, state, cfg); err != nil {
			t.Fatalf("dark-wake sample %d charged the unavailable timeout: %v", i, err)
		}
	}
	if monitor.routeUnavailableFor != 0 {
		t.Fatalf("dark wake accumulated unavailable time: %s", monitor.routeUnavailableFor)
	}

	fullWake = true
	now = now.Add(networkPollInterval)
	updates, err = monitor.poll(app, state, cfg)
	if err != nil {
		t.Fatalf("first full-wake sample stopped TUN: %v", err)
	}
	if len(updates) != 1 || !strings.Contains(updates[0], "resuming") {
		t.Fatalf("full-wake updates = %#v", updates)
	}
	if monitor.routeUnavailableFor != 0 {
		t.Fatalf("sleep-to-wake interval was charged: %s", monitor.routeUnavailableFor)
	}

	for monitor.routeUnavailableFor+networkPollInterval < networkUnavailableGrace {
		now = now.Add(networkPollInterval)
		if _, err := monitor.poll(app, state, cfg); err != nil {
			t.Fatalf("full-wake grace expired early at %s: %v", monitor.routeUnavailableFor, err)
		}
	}
	now = now.Add(networkPollInterval)
	if _, err := monitor.poll(app, state, cfg); err == nil || !strings.Contains(err.Error(), "full-wake time") {
		t.Fatalf("full-wake grace did not expire: %v", err)
	} else {
		requirePhysicalNetworkRestart(t, err)
	}
}

func TestMonitorDoesNotChargeAWholeSuspendedPollGap(t *testing.T) {
	cfg := reconcileTestConfig()
	before := physicalRouteSnapshot{
		Gateway4: "192.168.1.1", Interface: "en0", Source4: "192.168.1.20", IPv4: []string{"192.168.1.20"},
	}
	state := reconcileTestState(cfg, before, nil, nil)
	runner := &liveMonitorRunner{routeOutput: "route to: default\n"}
	app := &App{runner: runner, out: &bytes.Buffer{}, errOut: &bytes.Buffer{}}
	monitor := newLiveNetworkMonitor(before, nil, nil, nil, false, true, false, 0)
	now := time.Unix(1_700_000_000, 0)
	monitor.now = func() time.Time { return now }
	monitor.fullWake = func() bool { return true }

	_, err := monitor.poll(app, state, cfg)
	requireNetworkUnavailableSignal(t, err)
	now = now.Add(8 * time.Hour)
	if _, err := monitor.poll(app, state, cfg); err != nil {
		t.Fatalf("a suspended poll gap stopped TUN immediately: %v", err)
	}
	if monitor.routeUnavailableFor != networkPollInterval {
		t.Fatalf("suspended gap charged %s, want one poll interval %s", monitor.routeUnavailableFor, networkPollInterval)
	}
}

func TestMonitorRetainsDNSRoutesAcrossProbeFailure(t *testing.T) {
	t.Setenv("TUNSCOPE_STATE_DIR", t.TempDir())
	cfg := reconcileTestConfig()
	snapshot := physicalRouteSnapshot{
		Gateway4: "192.168.1.1", Interface: "en0", Source4: "192.168.1.20", IPv4: []string{"192.168.1.20"},
	}
	oldDNS := []netip.Addr{netip.MustParseAddr("1.1.1.1")}
	state := reconcileTestState(cfg, snapshot, nil, oldDNS)
	if err := saveState(state); err != nil {
		t.Fatal(err)
	}
	runner := &liveMonitorRunner{
		routeGateway: snapshot.Gateway4,
		routeIface:   snapshot.Interface,
		dnsErr:       errors.New("dynamic store temporarily unavailable"),
		ipv4Address:  snapshot.Source4,
	}
	app := &App{runner: runner, out: &bytes.Buffer{}, errOut: &bytes.Buffer{}}
	monitor := newLiveNetworkMonitor(snapshot, oldDNS, nil, nil, false, true, false, 0)

	for i := 0; i < 3*networkStableSampleCount; i++ {
		if _, err := monitor.poll(app, state, cfg); err != nil {
			t.Fatalf("DNS probe failure %d stopped TUN: %v", i, err)
		}
	}
	for _, route := range state.Routes {
		if route.Purpose == "dns-direct" && route.Target != "1.1.1.1" {
			t.Fatalf("old DNS route changed during probe failure: %#v", route)
		}
	}

	runner.dnsErr = nil
	runner.dnsServer = "8.8.8.8"
	for i := 0; i < networkStableSampleCount; i++ {
		if _, err := monitor.poll(app, state, cfg); err != nil {
			t.Fatalf("DNS recovery poll %d: %v", i, err)
		}
	}
	foundNew := false
	for _, route := range state.Routes {
		if route.Purpose == "dns-direct" && route.Target == "8.8.8.8" {
			foundNew = true
		}
	}
	if !foundNew {
		t.Fatalf("recovered DNS route was not committed: %#v", state.Routes)
	}
}

func TestMonitorReconcilesExternalAndLoopbackDNS(t *testing.T) {
	t.Setenv("TUNSCOPE_STATE_DIR", t.TempDir())
	cfg := reconcileTestConfig()
	snapshot := physicalRouteSnapshot{
		Gateway4: "192.168.1.1", Interface: "en0", Source4: "192.168.1.20", IPv4: []string{"192.168.1.20"},
	}
	externalDNS := []netip.Addr{netip.MustParseAddr("1.1.1.1")}
	state := reconcileTestState(cfg, snapshot, nil, externalDNS)
	if err := saveState(state); err != nil {
		t.Fatal(err)
	}
	runner := &liveMonitorRunner{
		routeGateway: snapshot.Gateway4,
		routeIface:   snapshot.Interface,
		dnsServer:    "127.0.0.1",
		ipv4Address:  snapshot.Source4,
	}
	app := &App{runner: runner, out: &bytes.Buffer{}, errOut: &bytes.Buffer{}}
	monitor := newLiveNetworkMonitor(snapshot, externalDNS, nil, nil, false, true, false, 0)

	for i := 0; i < networkStableSampleCount; i++ {
		if _, err := monitor.poll(app, state, cfg); err != nil {
			t.Fatalf("external-to-loopback DNS poll %d: %v", i, err)
		}
	}
	if monitor.dnsUnavailable {
		t.Fatal("loopback DNS was incorrectly marked unavailable")
	}
	if got := addrSetSignature(monitor.dnsServers); got != "127.0.0.1" {
		t.Fatalf("committed DNS = %q, want loopback", got)
	}
	mutations := routeMutationCalls(runner.calls)
	if len(mutations) != 1 || !strings.Contains(mutations[0], " delete ") || !strings.Contains(mutations[0], "1.1.1.1") {
		t.Fatalf("external-to-loopback mutations = %#v, want only external route deletion", mutations)
	}
	if strings.Contains(strings.Join(mutations, "\n"), "127.0.0.1") {
		t.Fatalf("loopback DNS received a managed route: %#v", mutations)
	}

	runner.calls = nil
	runner.dnsServer = "9.9.9.9"
	for i := 0; i < networkStableSampleCount; i++ {
		if _, err := monitor.poll(app, state, cfg); err != nil {
			t.Fatalf("loopback-to-external DNS poll %d: %v", i, err)
		}
	}
	mutations = routeMutationCalls(runner.calls)
	if len(mutations) != 1 || !strings.Contains(mutations[0], " add ") || !strings.Contains(mutations[0], "9.9.9.9") {
		t.Fatalf("loopback-to-external mutations = %#v, want only external route addition", mutations)
	}
}

func routeMutationCalls(calls []string) []string {
	mutations := make([]string, 0)
	for _, call := range calls {
		if strings.HasPrefix(call, "/sbin/route -n add ") || strings.HasPrefix(call, "/sbin/route -n delete ") || strings.HasPrefix(call, "/sbin/route -n change ") {
			mutations = append(mutations, call)
		}
	}
	return mutations
}

func TestMonitorRefreshesRoutesAndRebindsAfterSameNetworkReturns(t *testing.T) {
	t.Setenv("TUNSCOPE_STATE_DIR", t.TempDir())
	cfg := reconcileTestConfig()
	snapshot := physicalRouteSnapshot{
		Gateway4: "192.168.1.1", Interface: "en0", Source4: "192.168.1.20", IPv4: []string{"192.168.1.20"},
	}
	state := reconcileTestState(cfg, snapshot, nil, nil)
	if err := saveState(state); err != nil {
		t.Fatal(err)
	}
	runner := &liveMonitorRunner{
		routeGateway: snapshot.Gateway4,
		routeIface:   snapshot.Interface,
		routeOutput:  "route to: default\n",
		dnsServer:    "1.1.1.1",
		ipv4Address:  snapshot.Source4,
	}
	app := &App{runner: runner, out: &bytes.Buffer{}, errOut: &bytes.Buffer{}}
	monitor := newLiveNetworkMonitor(snapshot, nil, nil, nil, false, true, false, 0)

	for i := 0; i < networkStableSampleCount+1; i++ {
		_, err := monitor.poll(app, state, cfg)
		if i == 0 {
			requireNetworkUnavailableSignal(t, err)
		} else if err != nil {
			t.Fatalf("missing-route poll %d stopped TUN: %v", i, err)
		}
	}
	runner.routeOutput = ""
	var recoveryErr error
	for i := 0; i < networkRecoverySampleCount+1; i++ {
		_, recoveryErr = monitor.poll(app, state, cfg)
	}
	var change *physicalNetworkChangeError
	if !errors.As(recoveryErr, &change) {
		t.Fatalf("same-network recovery error = %v, want physicalNetworkChangeError", recoveryErr)
	}
	if change.Source4 != snapshot.Source4 {
		t.Fatalf("same-network recovery source = %q", change.Source4)
	}
	mutations := 0
	for _, call := range runner.calls {
		if !strings.HasPrefix(call, "/sbin/route -n delete ") && !strings.HasPrefix(call, "/sbin/route -n add ") {
			continue
		}
		mutations++
		if !strings.Contains(call, "-ifscope en0") {
			t.Fatalf("same-network recovery touched non-scoped route: %q", call)
		}
	}
	if mutations != 2*len(ipv4TunNetworks) {
		t.Fatalf("same-network route refreshes = %d, want %d: %#v", mutations, 2*len(ipv4TunNetworks), runner.calls)
	}
	if _, err := monitor.poll(app, state, cfg); err != nil {
		t.Fatalf("same-network recovery repeated on next poll: %v", err)
	}
}

func TestMonitorRoundTripAddressChangeRebindsBothWays(t *testing.T) {
	t.Setenv("TUNSCOPE_STATE_DIR", t.TempDir())
	cfg := reconcileTestConfig()
	a := physicalRouteSnapshot{
		Gateway4: "192.168.1.1", Interface: "en0", Source4: "192.168.1.20", IPv4: []string{"192.168.1.20"},
	}
	b := physicalRouteSnapshot{
		Gateway4: "192.168.50.1", Interface: "en0", Source4: "192.168.50.37", IPv4: []string{"192.168.50.37"},
	}
	state := reconcileTestState(cfg, a, nil, nil)
	if err := saveState(state); err != nil {
		t.Fatal(err)
	}
	runner := &liveMonitorRunner{
		routeGateway: b.Gateway4, routeIface: b.Interface, dnsServer: "1.1.1.1", ipv4Address: b.Source4,
	}
	app := &App{runner: runner, out: &bytes.Buffer{}, errOut: &bytes.Buffer{}}
	monitor := newLiveNetworkMonitor(a, nil, nil, nil, false, true, false, 0)

	for _, want := range []physicalRouteSnapshot{b, a} {
		runner.routeGateway = want.Gateway4
		runner.routeIface = want.Interface
		runner.ipv4Address = want.Source4
		var changeErr error
		for i := 0; i < addressStableSampleCount; i++ {
			_, changeErr = monitor.poll(app, state, cfg)
			if i < addressStableSampleCount-1 && changeErr != nil {
				t.Fatalf("%s poll %d returned early: %v", want.Source4, i, changeErr)
			}
		}
		var change *physicalNetworkChangeError
		if !errors.As(changeErr, &change) || change.Source4 != want.Source4 {
			t.Fatalf("change to %s = %#v / %v", want.Source4, change, changeErr)
		}
		if state.Phase != "active" || state.RouteReconcile != nil || len(state.PhysicalIPv4) != 1 || state.PhysicalIPv4[0] != want.Source4 {
			t.Fatalf("state after change to %s = %#v", want.Source4, state)
		}
	}
	for _, route := range state.Routes {
		if route.Purpose == "tun" && route.Gateway != tunGateway4 {
			t.Fatalf("round trip changed TUN capture route: %#v", route)
		}
	}
}

func TestAddressDebounceUsesAtomicRouteSnapshot(t *testing.T) {
	t.Setenv("TUNSCOPE_STATE_DIR", t.TempDir())
	cfg := reconcileTestConfig()
	before := physicalRouteSnapshot{
		Gateway4: "192.168.1.1", Interface: "en0", Source4: "192.168.1.20", IPv4: []string{"192.168.1.20"},
	}
	state := reconcileTestState(cfg, before, nil, nil)
	if err := saveState(state); err != nil {
		t.Fatal(err)
	}
	runner := &liveMonitorRunner{
		routeGateway: "192.168.50.1", routeIface: "en0", dnsServer: "1.1.1.1", ipv4Address: "192.168.50.37",
	}
	app := &App{runner: runner, out: &bytes.Buffer{}, errOut: &bytes.Buffer{}}
	monitor := newLiveNetworkMonitor(before, nil, nil, nil, false, true, false, 0)

	if _, err := monitor.poll(app, state, cfg); err != nil {
		t.Fatal(err)
	}
	runner.routeGateway = "192.168.60.1"
	if _, err := monitor.poll(app, state, cfg); err != nil {
		t.Fatalf("torn gateway/source samples were accepted: %v", err)
	}
	if state.Gateway4 != before.Gateway4 {
		t.Fatalf("torn snapshot changed state gateway to %q", state.Gateway4)
	}
	runner.routeGateway = "192.168.50.1"
	if _, err := monitor.poll(app, state, cfg); err != nil {
		t.Fatalf("first complete replacement sample returned early: %v", err)
	}
	_, err := monitor.poll(app, state, cfg)
	var change *physicalNetworkChangeError
	if !errors.As(err, &change) || state.Gateway4 != "192.168.50.1" {
		t.Fatalf("stable complete snapshot = %#v / %v / state %#v", change, err, state)
	}
}

func TestMonitorRepairsGatewayDespiteEmptyPeerObservation(t *testing.T) {
	t.Setenv("TUNSCOPE_STATE_DIR", t.TempDir())
	cfg := reconcileTestConfig()
	before := physicalRouteSnapshot{Gateway4: "192.168.1.1", Interface: "en0"}
	bypasses := []netip.Prefix{netip.MustParsePrefix("203.0.113.9/32")}
	dns := []netip.Addr{netip.MustParseAddr("1.1.1.1")}
	state := reconcileTestState(cfg, before, bypasses, dns)
	state.AutoBypasses = []string{"203.0.113.9"}
	if err := saveState(state); err != nil {
		t.Fatal(err)
	}
	runner := &liveMonitorRunner{routeGateway: "192.168.50.1", routeIface: "en0", dnsServer: "1.1.1.1"}
	app := &App{runner: runner, out: &bytes.Buffer{}, errOut: &bytes.Buffer{}}
	monitor := newLiveNetworkMonitor(before, dns, nil, state.AutoBypasses, false, true, true, 7890)

	for i := 0; i < networkStableSampleCount; i++ {
		_, err := monitor.poll(app, state, cfg)
		if i < networkStableSampleCount-1 && err != nil {
			t.Fatalf("poll %d: %v", i, err)
		}
		if i == networkStableSampleCount-1 {
			var change *physicalNetworkChangeError
			if !errors.As(err, &change) {
				t.Fatalf("stable gateway poll error = %v, want flow-rebind request", err)
			}
		}
	}
	if state.Gateway4 != runner.routeGateway {
		t.Fatalf("gateway = %q, want independently reconciled %q", state.Gateway4, runner.routeGateway)
	}
	// The next poll observes an empty lsof interval. It must not remove the
	// last known peer route or roll back the already committed gateway.
	if _, err := monitor.poll(app, state, cfg); err != nil {
		t.Fatal(err)
	}
	if len(state.AutoBypasses) != 1 || state.AutoBypasses[0] != "203.0.113.9" {
		t.Fatalf("auto bypasses after empty peer sample = %#v", state.AutoBypasses)
	}
	if state.Gateway4 != runner.routeGateway {
		t.Fatalf("gateway changed after peer observation: %q", state.Gateway4)
	}
}

func TestMonitorRejectsStableUTunDefault(t *testing.T) {
	cfg := reconcileTestConfig()
	before := physicalRouteSnapshot{Gateway4: "192.168.1.1", Interface: "en0"}
	dns := []netip.Addr{netip.MustParseAddr("1.1.1.1")}
	state := reconcileTestState(cfg, before, nil, dns)
	runner := &liveMonitorRunner{routeGateway: "198.18.0.1", routeIface: "utun99", dnsServer: "1.1.1.1"}
	app := &App{runner: runner, out: &bytes.Buffer{}, errOut: &bytes.Buffer{}}
	monitor := newLiveNetworkMonitor(before, dns, nil, nil, false, true, false, 0)

	for i := 0; i < networkStableSampleCount-1; i++ {
		if _, err := monitor.poll(app, state, cfg); err != nil {
			t.Fatalf("transient utun sample %d stopped early: %v", i, err)
		}
	}
	_, err := monitor.poll(app, state, cfg)
	if err == nil || !strings.Contains(err.Error(), "uses TUN interface utun99") {
		t.Fatalf("stable utun error = %v", err)
	}
}
