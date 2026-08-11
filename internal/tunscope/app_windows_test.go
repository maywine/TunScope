//go:build windows

package tunscope

import (
	"encoding/json"
	"net/netip"
	"os"
	"strings"
	"testing"
	"time"
)

type windowsRouteTestRunner struct {
	output string
	name   string
	args   []string
}

func (r *windowsRouteTestRunner) Run(name string, args ...string) (string, error) {
	r.name = name
	r.args = append([]string(nil), args...)
	return r.output, nil
}

type windowsSequenceTestRunner struct {
	outputs       []string
	defaultOutput string
	calls         []string
}

func (r *windowsSequenceTestRunner) Run(name string, args ...string) (string, error) {
	r.calls = append(r.calls, strings.Join(append([]string{name}, args...), " "))
	if len(r.outputs) == 0 {
		return r.defaultOutput, nil
	}
	output := r.outputs[0]
	r.outputs = r.outputs[1:]
	return output, nil
}

func TestWindowsDefaultDevice(t *testing.T) {
	if got := DefaultConfig().Device; got != "TunScope" {
		t.Fatalf("default device = %q, want TunScope", got)
	}
}

func TestWindowsCaptureRoutesUseWintunIndex(t *testing.T) {
	routes := windowsCaptureRoutes(42, true)
	if len(routes) != len(ipv4TunNetworks)-1+2 {
		t.Fatalf("got %d routes", len(routes))
	}
	for _, route := range routes {
		if route.Interface != "42" || route.Purpose != "tun" {
			t.Fatalf("unexpected capture route: %+v", route)
		}
		if route.Family == "inet" && route.Gateway != tunGateway4 {
			t.Fatalf("IPv4 capture route has gateway %q, want %s", route.Gateway, tunGateway4)
		}
	}
}

func TestWindowsPhysicalRoutesKeepSharedDNSDirect(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Applications = []string{`C:\Apps\Browser.exe`}
	cfg.TrustedDNS = ""
	physical := windowsPhysicalNetwork{
		InterfaceIndex: 12,
		Gateway4:       "192.168.1.1",
	}
	routes := windowsPhysicalRoutes(
		cfg,
		physical,
		[]netip.Prefix{netip.MustParsePrefix("203.0.113.7/32")},
		[]netip.Addr{netip.MustParseAddr("223.5.5.5")},
	)
	if len(routes) != 2 {
		t.Fatalf("got %d routes, want 2: %+v", len(routes), routes)
	}
	if routes[0].Purpose != "bypass" || routes[1].Purpose != "dns-direct" || routes[1].Interface != "12" {
		t.Fatalf("unexpected physical routes: %+v", routes)
	}
}

func TestWindowsPhysicalSignatureIgnoresDNSOrderAndIPv6PrivacyAddress(t *testing.T) {
	left := windowsPhysicalNetwork{
		InterfaceIndex: 4, InterfaceAlias: "Wi-Fi", Gateway4: "192.168.1.1", Source4: "192.168.1.2",
		Interface6Index: 4, Interface6Alias: "Wi-Fi", Gateway6: "fe80::1", Source6: "2001:db8::1",
		DNSServers: []string{"8.8.8.8", "1.1.1.1"},
	}
	right := left
	right.Source6 = "2001:db8::2"
	right.DNSServers = []string{"1.1.1.1", "8.8.8.8"}
	if windowsPhysicalSignature(left) != windowsPhysicalSignature(right) {
		t.Fatal("equivalent physical snapshots produced different signatures")
	}
}

func TestWindowsPhysicalPathSignatureIgnoresDNSOnlyChange(t *testing.T) {
	before := windowsPhysicalNetwork{
		InterfaceIndex: 4, InterfaceAlias: "Wi-Fi", Gateway4: "192.168.1.1", Source4: "192.168.1.2",
		DNSServers: []string{"192.168.1.1"},
	}
	after := before
	after.DNSServers = []string{"1.1.1.1"}
	if windowsPhysicalSignature(before) == windowsPhysicalSignature(after) {
		t.Fatal("DNS change did not alter the complete physical signature")
	}
	if windowsPhysicalPathSignature(before) != windowsPhysicalPathSignature(after) {
		t.Fatal("DNS-only change altered the physical path signature")
	}
}

func TestValidateWindowsConfigRejectsUnsafeAdapterName(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Proxy = "socks5://127.0.0.1:7890"
	cfg.Device = `TunScope:Unsafe`
	if _, err := validateWindowsConfig(cfg); err == nil {
		t.Fatal("expected unsafe adapter name to fail")
	}
}

func TestDeleteWindowsRoutesRetainsOnlyReportedFailures(t *testing.T) {
	routes := []Route{
		{Family: "inet", Target: "1.0.0.0/8", Gateway: tunGateway4, Interface: "42", Purpose: "tun"},
		{Family: "inet", Target: "8.8.8.8", Gateway: tunGateway4, Interface: "42", Purpose: "dns"},
	}
	runner := &windowsRouteTestRunner{output: `{"Failed":[1]}`}
	failed, err := deleteWindowsRoutes(runner, routes)
	if err == nil {
		t.Fatal("expected the reported route failure to return an error")
	}
	if len(failed) != 1 || windowsRouteKey(failed[0]) != windowsRouteKey(routes[1]) {
		t.Fatalf("unexpected retained routes: %+v", failed)
	}
	if runner.name != "powershell.exe" || !strings.Contains(strings.Join(runner.args, " "), "Remove-NetRoute") {
		t.Fatalf("unexpected cleanup command: %s %v", runner.name, runner.args)
	}
}

func TestSuspendAndResumeWindowsOwnedRoutesAroundRecovery(t *testing.T) {
	t.Setenv("TUNSCOPE_STATE_DIR", t.TempDir())
	state := &State{
		Version:     stateVersion,
		Phase:       "active",
		Device:      "TunScope",
		DeviceIndex: 42,
		Routes: []Route{
			{Family: "inet", Kind: "host", Target: "203.0.113.9", Gateway: "192.168.1.1", Interface: "12", Purpose: "bypass"},
			{Family: "inet", Kind: "host", Target: "8.8.8.8", Gateway: tunGateway4, Interface: "42", Purpose: "dns"},
			{Family: "inet", Kind: "net", Target: "1.0.0.0/8", Gateway: tunGateway4, Interface: "42", Purpose: "tun"},
		},
	}
	if err := saveState(state); err != nil {
		t.Fatal(err)
	}
	runner := &windowsSequenceTestRunner{
		outputs:       []string{`{"Failed":[]}`},
		defaultOutput: "added",
	}
	app := &App{runner: runner}

	removed, err := app.suspendWindowsOwnedRoutes(state)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 3 || !state.RoutesSuspended || len(state.Routes) != 0 {
		t.Fatalf("suspension removed=%d suspended=%v routes=%+v", removed, state.RoutesSuspended, state.Routes)
	}
	persisted, err := loadState()
	if err != nil {
		t.Fatal(err)
	}
	if !persisted.RoutesSuspended || len(persisted.Routes) != 0 {
		t.Fatalf("persisted suspended state = %+v", persisted)
	}

	cfg := DefaultConfig()
	cfg.IPv6 = false
	added, err := app.resumeWindowsCaptureRoutes(state, cfg, []netip.Addr{netip.MustParseAddr("8.8.8.8")})
	if err != nil {
		t.Fatal(err)
	}
	wantCapture := len(windowsTUNDNSRoutes(cfg, state.DeviceIndex, []netip.Addr{netip.MustParseAddr("8.8.8.8")})) + len(windowsCaptureRoutes(state.DeviceIndex, false))
	if added != wantCapture || state.RoutesSuspended || len(state.Routes) != wantCapture {
		t.Fatalf("resume added=%d suspended=%v routes=%d, want %d", added, state.RoutesSuspended, len(state.Routes), wantCapture)
	}
	if state.Routes[0].Purpose != "dns" {
		t.Fatalf("first restored route = %+v, want exact DNS capture before broad TUN routes", state.Routes[0])
	}
	for _, route := range state.Routes[1:] {
		if route.Purpose != "tun" {
			t.Fatalf("unexpected non-capture route after resume: %+v", route)
		}
	}
}

func TestPrepareWindowsRecoveryRoutesRestoresOnlyPhysicalRoutes(t *testing.T) {
	t.Setenv("TUNSCOPE_STATE_DIR", t.TempDir())
	state := &State{
		Version: stateVersion, Phase: "active", Device: "TunScope", DeviceIndex: 42,
		RoutesSuspended: true,
	}
	if err := saveState(state); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.IPv6 = false
	cfg.Applications = []string{`C:\Apps\Browser.exe`}
	physical := windowsPhysicalNetwork{
		InterfaceIndex: 12, InterfaceAlias: "Wi-Fi",
		Gateway4: "192.168.50.1", Source4: "192.168.50.20",
	}
	bypasses := []netip.Prefix{netip.MustParsePrefix("203.0.113.9/32")}
	dns := []netip.Addr{netip.MustParseAddr("223.5.5.5")}
	runner := &windowsSequenceTestRunner{defaultOutput: "added"}
	app := &App{runner: runner}

	if err := app.prepareWindowsRecoveryRoutes(state, cfg, physical, bypasses, dns); err != nil {
		t.Fatal(err)
	}
	if !state.RoutesSuspended || len(state.Routes) != 2 {
		t.Fatalf("prepared state = %+v, want only bypass and direct DNS routes", state)
	}
	for _, route := range state.Routes {
		if !windowsRecoveryPhysicalRoute(route) || route.Purpose == "tun" || route.Purpose == "dns" {
			t.Fatalf("capture route restored before engine rebind: %+v", route)
		}
	}
	if state.Interface != "Wi-Fi" || state.Gateway4 != "192.168.50.1" || len(state.PhysicalIPv4) != 1 || state.PhysicalIPv4[0] != "192.168.50.20" {
		t.Fatalf("replacement physical state was not committed: %+v", state)
	}

	// A retry force-checks the same routes but must not duplicate their cleanup
	// ownership records.
	runner.defaultOutput = "exists"
	if err := app.prepareWindowsRecoveryRoutes(state, cfg, physical, bypasses, dns); err != nil {
		t.Fatal(err)
	}
	if len(state.Routes) != 2 {
		t.Fatalf("retry duplicated route ownership: %+v", state.Routes)
	}
}

func TestVerifyWindowsPhysicalNetworkChecksDefaultSourceAndOwnedRoutes(t *testing.T) {
	physical := windowsPhysicalNetwork{
		InterfaceIndex: 12, InterfaceAlias: "Wi-Fi",
		Gateway4: "192.168.50.1", Source4: "192.168.50.20",
	}
	route := Route{
		Family: "inet", Kind: "host", Target: "203.0.113.9",
		Gateway: physical.Gateway4, Interface: "12", Purpose: "bypass",
	}
	runner := &windowsRouteTestRunner{output: "ok"}
	if err := verifyWindowsPhysicalNetwork(runner, physical, []Route{route}); err != nil {
		t.Fatal(err)
	}
	script := strings.Join(runner.args, " ")
	for _, want := range []string{"0.0.0.0/0", "192.168.50.20", "203.0.113.9/32", "Get-NetIPAddress", "Get-NetRoute"} {
		if !strings.Contains(script, want) {
			t.Fatalf("verification script does not contain %q: %s", want, script)
		}
	}
}

func TestWindowsEngineControllerSendsNetworkInvalidation(t *testing.T) {
	commandRead, commandWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	responseRead, responseWrite, err := os.Pipe()
	if err != nil {
		commandRead.Close()
		commandWrite.Close()
		t.Fatal(err)
	}
	defer commandRead.Close()
	defer responseWrite.Close()
	controller := newWindowsEngineController(commandWrite, responseRead)
	defer controller.Close()
	commands := make(chan EngineControlCommand, 1)

	go func() {
		var command EngineControlCommand
		if err := json.NewDecoder(commandRead).Decode(&command); err != nil {
			return
		}
		commands <- command
		_ = json.NewEncoder(responseWrite).Encode(EngineControlResponse{
			Action: command.Action, Generation: command.Generation, Closed: 4,
		})
	}()

	closed, err := controller.InvalidateNetwork(time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if closed != 4 {
		t.Fatalf("acknowledged closed flows = %d, want 4", closed)
	}
	command := <-commands
	if !command.IsNetworkInvalidation() || command.Source4 != "" {
		t.Fatalf("invalidation command = %#v", command)
	}
}
