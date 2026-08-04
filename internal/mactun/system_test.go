package mactun

import (
	"reflect"
	"testing"
)

func TestParseRouteGet(t *testing.T) {
	input := `   route to: default
destination: default
       mask: default
    gateway: 192.168.50.1
  interface: en0
`
	gateway, iface, err := parseRouteGet(input)
	if err != nil {
		t.Fatal(err)
	}
	if gateway != "192.168.50.1" || iface != "en0" {
		t.Fatalf("got gateway=%q interface=%q", gateway, iface)
	}
}

func TestRouteArgs(t *testing.T) {
	tests := []struct {
		name  string
		route Route
		want  []string
	}{
		{
			name:  "IPv4 gateway",
			route: Route{Family: "inet", Kind: "net", Target: "1.0.0.0/8", Gateway: tunGateway4},
			want:  []string{"-n", "add", "-net", "1.0.0.0/8", tunGateway4},
		},
		{
			name:  "IPv6 interface",
			route: Route{Family: "inet6", Kind: "net", Target: "::/1", Interface: "utun123"},
			want:  []string{"-n", "add", "-inet6", "-net", "::/1", "-interface", "utun123"},
		},
		{
			name:  "IPv4 interface-scoped gateway",
			route: Route{Family: "inet", Kind: "net", Target: "0.0.0.0/1", Gateway: "192.168.1.1", Scope: "en0", Source: "192.168.1.20"},
			want:  []string{"-n", "add", "-net", "-ifscope", "en0", "-ifa", "192.168.1.20", "0.0.0.0/1", "192.168.1.1"},
		},
		{
			name:  "IPv6 interface-scoped gateway",
			route: Route{Family: "inet6", Kind: "net", Target: "::/1", Gateway: "fe80::1%utun0", Scope: "utun0"},
			want:  []string{"-n", "add", "-inet6", "-net", "-ifscope", "utun0", "::/1", "fe80::1%utun0"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := routeArgs("add", tt.route); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %#v want %#v", got, tt.want)
			}
		})
	}
}

func TestRouteDeleteOmitsRemovedSourceAddress(t *testing.T) {
	route := Route{
		Family: "inet", Kind: "host", Target: "203.0.113.9",
		Gateway: "192.168.1.1", Scope: "en0", Source: "192.168.1.20",
	}
	want := []string{"-n", "delete", "-host", "-ifscope", "en0", "203.0.113.9", "192.168.1.1"}
	if got := routeArgs("delete", route); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v want %#v", got, want)
	}
}
