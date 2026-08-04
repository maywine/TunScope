package mactun

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"net/netip"
	"testing"
	"time"

	M "github.com/xjasonlyu/tun2socks/v2/metadata"
)

type dnsTestSOCKS struct{}

func (dnsTestSOCKS) DialContext(_ context.Context, metadata *M.Metadata) (net.Conn, error) {
	client, server := net.Pipe()
	go func() {
		defer server.Close()
		var size [2]byte
		if _, err := io.ReadFull(server, size[:]); err != nil {
			return
		}
		query := make([]byte, int(binary.BigEndian.Uint16(size[:])))
		if _, err := io.ReadFull(server, query); err != nil {
			return
		}
		response := append([]byte(nil), query...)
		response[2] |= 0x80
		binary.BigEndian.PutUint16(size[:], uint16(len(response)))
		_, _ = server.Write(append(size[:], response...))
	}()
	return client, nil
}

func (dnsTestSOCKS) DialUDP(*M.Metadata) (net.PacketConn, error) {
	return nil, nil
}

type recordingDNSSOCKS struct {
	targets chan netip.AddrPort
}

func (d recordingDNSSOCKS) DialContext(ctx context.Context, metadata *M.Metadata) (net.Conn, error) {
	d.targets <- metadata.DestinationAddrPort()
	return (dnsTestSOCKS{}).DialContext(ctx, metadata)
}

func (recordingDNSSOCKS) DialUDP(*M.Metadata) (net.PacketConn, error) {
	return nil, nil
}

func TestDNSOverTCPPacketConn(t *testing.T) {
	dialer := newDNSOverTCPDialer(dnsTestSOCKS{}, netip.AddrPort{})
	packetConn, err := dialer.DialUDP(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer packetConn.Close()
	if err := packetConn.SetDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}

	destination := &M.Metadata{
		Network: M.UDP,
		DstIP:   netip.MustParseAddr("1.1.1.1"),
		DstPort: 53,
	}
	query := dnsProbeQuery()
	if n, err := packetConn.WriteTo(query, destination.Addr()); err != nil || n != len(query) {
		t.Fatalf("WriteTo() = %d, %v", n, err)
	}

	buffer := make([]byte, 4096)
	n, addr, err := packetConn.ReadFrom(buffer)
	if err != nil {
		t.Fatal(err)
	}
	if n != len(query) || buffer[2]&0x80 == 0 {
		t.Fatalf("invalid DNS response: %x", buffer[:n])
	}
	if got := addr.String(); got != "1.1.1.1:53" {
		t.Fatalf("response address = %q", got)
	}
}

func TestDNSOverTCPRejectsNonDNSDestination(t *testing.T) {
	packetConn, err := newDNSOverTCPDialer(dnsTestSOCKS{}, netip.AddrPort{}).DialUDP(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer packetConn.Close()

	destination := &M.Metadata{
		Network: M.UDP,
		DstIP:   netip.MustParseAddr("1.1.1.1"),
		DstPort: 443,
	}
	if _, err := packetConn.WriteTo(dnsProbeQuery(), destination.Addr()); err == nil {
		t.Fatal("expected non-DNS destination to be rejected")
	}
}

func TestDNSOverTCPUsesTrustedResolverAndPreservesResponseAddress(t *testing.T) {
	targets := make(chan netip.AddrPort, 1)
	resolver := netip.MustParseAddrPort("8.8.8.8:53")
	dialer := newDNSOverTCPDialer(recordingDNSSOCKS{targets: targets}, resolver)
	packetConn, err := dialer.DialUDP(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer packetConn.Close()
	if err := packetConn.SetDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}

	original := &M.Metadata{
		Network: M.UDP,
		DstIP:   netip.MustParseAddr("223.5.5.5"),
		DstPort: 53,
	}
	query := dnsProbeQuery()
	if _, err := packetConn.WriteTo(query, original.Addr()); err != nil {
		t.Fatal(err)
	}
	if target := <-targets; target != resolver {
		t.Fatalf("SOCKS DNS target = %s, want %s", target, resolver)
	}
	buffer := make([]byte, 4096)
	_, responseAddr, err := packetConn.ReadFrom(buffer)
	if err != nil {
		t.Fatal(err)
	}
	if got := responseAddr.String(); got != "223.5.5.5:53" {
		t.Fatalf("rewritten DNS response address = %q", got)
	}
}

func TestDNSOverTCPStreamUsesTrustedResolver(t *testing.T) {
	targets := make(chan netip.AddrPort, 1)
	resolver := netip.MustParseAddrPort("8.8.8.8:53")
	dialer := newDNSOverTCPDialer(recordingDNSSOCKS{targets: targets}, resolver)
	conn, err := dialer.DialContext(context.Background(), &M.Metadata{
		Network: M.TCP,
		DstIP:   netip.MustParseAddr("223.5.5.5"),
		DstPort: 53,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if target := <-targets; target != resolver {
		t.Fatalf("SOCKS TCP DNS target = %s, want %s", target, resolver)
	}
}

func TestCheckTrustedDNS(t *testing.T) {
	proxyURL := "socks5://127.0.0.1:1"
	resolver := netip.MustParseAddrPort("8.8.8.8:53")

	// Exercise the response validation independently of a real SOCKS server by
	// using the same packet-connection path as checkTrustedDNS.
	packetConn, err := newDNSOverTCPDialer(dnsTestSOCKS{}, resolver).DialUDP(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer packetConn.Close()
	if err := packetConn.SetDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	query := dnsProbeQuery()
	if _, err := packetConn.WriteTo(query, net.UDPAddrFromAddrPort(resolver)); err != nil {
		t.Fatal(err)
	}
	response := make([]byte, 4096)
	n, _, err := packetConn.ReadFrom(response)
	if err != nil {
		t.Fatal(err)
	}
	if n < 12 || response[0] != query[0] || response[1] != query[1] || response[2]&0x80 == 0 {
		t.Fatalf("invalid trusted DNS response: %x", response[:n])
	}

	// Keep a compile-time call-site check on the public startup helper. The
	// unreachable local endpoint must fail instead of accepting HTTPS-only
	// proxy reachability as sufficient.
	if err := checkTrustedDNS(proxyURL, resolver); err == nil {
		t.Fatal("trusted DNS check unexpectedly succeeded through a closed SOCKS endpoint")
	}
}

func TestParseTrustedDNS(t *testing.T) {
	for _, test := range []struct {
		input string
		want  string
	}{
		{input: "8.8.8.8", want: "8.8.8.8:53"},
		{input: "1.1.1.1:5353", want: "1.1.1.1:5353"},
		{input: "", want: "invalid AddrPort"},
	} {
		got, err := parseTrustedDNS(test.input)
		if err != nil {
			t.Fatalf("parse %q: %v", test.input, err)
		}
		if got.String() != test.want {
			t.Fatalf("parse %q = %q, want %q", test.input, got, test.want)
		}
	}
	for _, input := range []string{"localhost:53", "127.0.0.1:53", "0.0.0.0:53"} {
		if _, err := parseTrustedDNS(input); err == nil {
			t.Fatalf("parse %q unexpectedly succeeded", input)
		}
	}
}
