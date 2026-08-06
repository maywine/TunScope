//go:build windows

package tunscope

import (
	"net/netip"
	"strings"
	"testing"
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
