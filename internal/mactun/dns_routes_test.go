package mactun

import (
	"net/netip"
	"reflect"
	"testing"
)

func TestDirectDNSRoute(t *testing.T) {
	tests := []struct {
		name   string
		server string
		gw4    string
		gw6    string
		iface6 string
		want   Route
	}{
		{
			name:   "IPv4 uses physical gateway",
			server: "223.5.5.5",
			gw4:    "192.168.1.1",
			want: Route{
				Family: "inet", Kind: "host", Target: "223.5.5.5",
				Gateway: "192.168.1.1", Purpose: "dns-direct",
			},
		},
		{
			name:   "IPv6 gateway is interface scoped",
			server: "2606:4700:4700::1111",
			gw6:    "fe80::1%en0",
			iface6: "en0",
			want: Route{
				Family: "inet6", Kind: "host", Target: "2606:4700:4700::1111",
				Gateway: "fe80::1%en0", Scope: "en0", Purpose: "dns-direct",
			},
		},
		{
			name:   "IPv6 can use an interface",
			server: "2001:4860:4860::8888",
			iface6: "en0",
			want: Route{
				Family: "inet6", Kind: "host", Target: "2001:4860:4860::8888",
				Interface: "en0", Purpose: "dns-direct",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := directDNSRoute(netip.MustParseAddr(tt.server), tt.gw4, tt.gw6, tt.iface6)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("route = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestRouteForSystemDNSMode(t *testing.T) {
	server := netip.MustParseAddr("223.5.5.5")
	direct := routeForSystemDNS(server, true, "utun123", "192.168.1.1", "", "")
	if direct.Purpose != "dns-direct" || direct.Gateway != "192.168.1.1" {
		t.Fatalf("per-app DNS route = %#v", direct)
	}
	captured := routeForSystemDNS(server, false, "utun123", "192.168.1.1", "", "")
	if captured.Purpose != "dns" || captured.Gateway != tunGateway4 {
		t.Fatalf("global DNS route = %#v", captured)
	}
}
