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

func TestChangeMissingRouteFallsBackToAdd(t *testing.T) {
	route := Route{
		Family: "inet", Kind: "net", Target: "1.0.0.0/8",
		Gateway: "192.168.50.1", Scope: "en0", Purpose: "direct-scope",
	}
	runner := &scriptedRouteRunner{errs: []error{errors.New("route: writing to routing socket: not in table"), nil}}
	if err := changeOrRestoreRoute(runner, route); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 2 || !strings.Contains(runner.calls[0], " change ") || !strings.Contains(runner.calls[1], " add ") {
		t.Fatalf("fallback calls = %#v, want change then add", runner.calls)
	}
}

func TestChangeMissingRouteAddRaceRetriesChange(t *testing.T) {
	route := Route{
		Family: "inet", Kind: "host", Target: "1.1.1.1",
		Gateway: "192.168.50.1", Purpose: "dns-direct",
	}
	runner := &scriptedRouteRunner{errs: []error{
		errors.New("route: writing to routing socket: not in table"),
		errors.New("route: writing to routing socket: File exists"),
		nil,
	}}
	if err := changeOrRestoreRoute(runner, route); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 3 || !strings.Contains(runner.calls[0], " change ") ||
		!strings.Contains(runner.calls[1], " add ") || !strings.Contains(runner.calls[2], " change ") {
		t.Fatalf("raced fallback calls = %#v, want change/add/change", runner.calls)
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
	if len(runner.calls) != len(ipv4TunNetworks)+2 {
		t.Fatalf("routing calls = %d, want %d: %#v", len(runner.calls), len(ipv4TunNetworks)+2, runner.calls)
	}
	for i := 0; i < len(ipv4TunNetworks); i++ {
		if !strings.Contains(runner.calls[i], " change ") || !strings.Contains(runner.calls[i], "-ifscope en0") {
			t.Fatalf("call %d = %q, want direct-scope route change first", i, runner.calls[i])
		}
	}
	if !strings.Contains(runner.calls[len(ipv4TunNetworks)], "203.0.113.9") {
		t.Fatalf("call after direct routes = %q, want bypass", runner.calls[len(ipv4TunNetworks)])
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
	routeGateway string
	routeIface   string
	routeOutput  string
	routeErr     error
	dnsServer    string
	dnsOutput    string
	dnsErr       error
	ipv4Address  string
	calls        []string
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

	for i := 0; i < 3*networkStableSampleCount; i++ {
		updates, err := monitor.poll(app, state, cfg)
		if err != nil {
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
	for i := 0; i < addressStableSampleCount; i++ {
		updates, err := monitor.poll(app, state, cfg)
		recoveryErr = err
		if i == 0 && (len(updates) != 1 || !strings.Contains(updates[0], "available again")) {
			t.Fatalf("first recovery update = %#v", updates)
		}
		if i < addressStableSampleCount-1 && err != nil {
			t.Fatalf("recovery poll %d returned before stability threshold: %v", i, err)
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

	if _, err := monitor.poll(app, state, cfg); err != nil {
		t.Fatalf("first unavailable sample stopped TUN: %v", err)
	}
	now = now.Add(networkUnavailableGrace - time.Millisecond)
	if _, err := monitor.poll(app, state, cfg); err != nil {
		t.Fatalf("sample inside unavailable grace stopped TUN: %v", err)
	}
	now = now.Add(time.Millisecond)
	_, err := monitor.poll(app, state, cfg)
	var change *physicalNetworkChangeError
	if err == nil || errors.As(err, &change) || !strings.Contains(err.Error(), "remained unavailable") {
		t.Fatalf("expired unavailable grace error = %v", err)
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
		if _, err := monitor.poll(app, state, cfg); err != nil {
			t.Fatalf("missing-route poll %d stopped TUN: %v", i, err)
		}
	}
	runner.routeOutput = ""
	var recoveryErr error
	for i := 0; i < addressStableSampleCount; i++ {
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
		if !strings.HasPrefix(call, "/sbin/route -n change ") {
			continue
		}
		mutations++
		if !strings.Contains(call, "-ifscope en0") {
			t.Fatalf("same-network recovery touched non-scoped route: %q", call)
		}
	}
	if mutations != len(ipv4TunNetworks) {
		t.Fatalf("same-network route refreshes = %d, want %d: %#v", mutations, len(ipv4TunNetworks), runner.calls)
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
