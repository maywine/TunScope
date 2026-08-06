package tunscope

import (
	"net/netip"
	"reflect"
	"testing"
)

func TestValidateProxy(t *testing.T) {
	info, err := validateProxy("socks5://user:secret@127.0.0.1:7890")
	if err != nil {
		t.Fatal(err)
	}
	if !info.Loopback || info.Port != 7890 {
		t.Fatalf("unexpected info: %#v", info)
	}
	if got := redactProxy(info.URL.String()); got != "socks5://127.0.0.1:7890" {
		t.Fatalf("redacted URL = %q", got)
	}
}

func TestRedactProxyRemovesAllUserInfo(t *testing.T) {
	for _, raw := range []string{
		"socks5://username@127.0.0.1:7890",
		"socks5://user%40example.com:secret@127.0.0.1:7890",
	} {
		if got := redactProxy(raw); got != "socks5://127.0.0.1:7890" {
			t.Errorf("redactProxy(%q) = %q", raw, got)
		}
	}
}

func TestRejectHTTPProxy(t *testing.T) {
	if _, err := validateProxy("http://127.0.0.1:7890"); err == nil {
		t.Fatal("expected HTTP proxy rejection")
	}
}

func TestProxyURLWithLiteralHost(t *testing.T) {
	info, err := validateProxy("socks5://user:secret@203.0.113.7:1080")
	if err != nil {
		t.Fatal(err)
	}
	got, err := proxyURLWithResolvedHost(info)
	if err != nil {
		t.Fatal(err)
	}
	if got != info.URL.String() {
		t.Fatalf("got %q want %q", got, info.URL.String())
	}
}

func TestResolveBypasses(t *testing.T) {
	got, err := resolveBypasses([]string{"1.2.3.4", "10.0.0.0/24", "1.2.3.4/32"})
	if err != nil {
		t.Fatal(err)
	}
	want := []netip.Prefix{netip.MustParsePrefix("1.2.3.4/32"), netip.MustParsePrefix("10.0.0.0/24")}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestDiscoverPeerPattern(t *testing.T) {
	input := "TCP 192.168.1.5:53122->1.2.3.4:443 (ESTABLISHED) TCP [fd00::1]:50000->[2606:4700::1111]:443 (ESTABLISHED)"
	matches := peerPattern.FindAllStringSubmatch(input, -1)
	if len(matches) != 2 || matches[0][2] != "1.2.3.4" || matches[1][1] != "2606:4700::1111" {
		t.Fatalf("unexpected matches: %#v", matches)
	}
}
