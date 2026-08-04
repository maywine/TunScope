package mactun

import (
	"errors"
	"net/netip"
	"reflect"
	"testing"
)

type fakeRunner struct {
	output string
	err    error
}

func (f fakeRunner) Run(string, ...string) (string, error) { return f.output, f.err }

func TestSystemDNSServers(t *testing.T) {
	input := `resolver #1
  nameserver[0] : 127.0.0.1
	 nameserver[1] : ::1
	 nameserver[2] : ::ffff:127.0.0.1
	 nameserver[3] : 0.0.0.0
	 nameserver[4] : ::
	 nameserver[5] : 192.168.1.1
	 nameserver[6] : 2606:4700:4700::1111
resolver #2
  nameserver[0] : 192.168.1.1
  nameserver[1] : 224.0.0.251
`
	got := systemDNSServers(fakeRunner{output: input})
	want := []string{"127.0.0.1", "192.168.1.1", "2606:4700:4700::1111", "::1"}
	var stringsGot []string
	for _, value := range got {
		stringsGot = append(stringsGot, value.String())
	}
	if !reflect.DeepEqual(stringsGot, want) {
		t.Fatalf("got %v want %v", stringsGot, want)
	}
	if got := systemDNSServers(fakeRunner{err: errors.New("failed")}); got != nil {
		t.Fatalf("expected nil, got %v", got)
	}
}

func TestRoutedSystemDNSServersSkipsLoopback(t *testing.T) {
	servers := []netip.Addr{
		netip.MustParseAddr("127.0.0.1"),
		netip.MustParseAddr("::1"),
		netip.MustParseAddr("223.5.5.5"),
		netip.MustParseAddr("2606:4700:4700::1111"),
	}

	if got := routedSystemDNSServers(servers, false); !reflect.DeepEqual(got, []netip.Addr{netip.MustParseAddr("223.5.5.5")}) {
		t.Fatalf("IPv4 routed DNS = %v", got)
	}
	want := []netip.Addr{netip.MustParseAddr("223.5.5.5"), netip.MustParseAddr("2606:4700:4700::1111")}
	if got := routedSystemDNSServers(servers, true); !reflect.DeepEqual(got, want) {
		t.Fatalf("dual-stack routed DNS = %v, want %v", got, want)
	}
}
