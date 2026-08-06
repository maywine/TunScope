package tunscope

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	M "github.com/xjasonlyu/tun2socks/v2/metadata"
	"github.com/xjasonlyu/tun2socks/v2/proxy"
)

type fixedProcessMatcher struct {
	decision processDecision
	err      error
}

func (m fixedProcessMatcher) Decide(*M.Metadata) (processDecision, error) {
	return m.decision, m.err
}

func (fixedProcessMatcher) Close() error { return nil }

type markerDialer struct{ name string }

func (*markerDialer) DialContext(context.Context, *M.Metadata) (net.Conn, error) {
	return nil, errors.New("not implemented")
}

func (*markerDialer) DialUDP(*M.Metadata) (net.PacketConn, error) {
	return nil, errors.New("not implemented")
}

type pipeDialer struct {
	peers chan net.Conn
}

func (d *pipeDialer) DialContext(context.Context, *M.Metadata) (net.Conn, error) {
	local, peer := net.Pipe()
	d.peers <- peer
	return local, nil
}

func (*pipeDialer) DialUDP(*M.Metadata) (net.PacketConn, error) {
	return nil, errors.New("not implemented")
}

type blockingDialer struct {
	started chan struct{}
}

func (d *blockingDialer) DialContext(ctx context.Context, _ *M.Metadata) (net.Conn, error) {
	close(d.started)
	<-ctx.Done()
	return nil, ctx.Err()
}

func (*blockingDialer) DialUDP(*M.Metadata) (net.PacketConn, error) {
	return nil, errors.New("not implemented")
}

func TestPerAppDialerRoutesDNSByOwner(t *testing.T) {
	tests := []struct {
		name      string
		decision  processDecision
		network   M.Network
		port      uint16
		proxyUDP  bool
		want      string
		wantError string
	}{
		{name: "selected UDP DNS uses DNS over TCP", decision: processProxy, network: M.UDP, port: 53, want: "dns"},
		{name: "direct UDP DNS remains direct", decision: processDirect, network: M.UDP, port: 53, want: "direct"},
		{name: "selected DNS over TLS uses SOCKS", decision: processProxy, network: M.TCP, port: 853, want: "socks"},
		{name: "direct DNS over TLS remains direct", decision: processDirect, network: M.TCP, port: 853, want: "direct"},
		{name: "selected QUIC uses UDP relay", decision: processProxy, network: M.UDP, port: 443, proxyUDP: true, want: "socks"},
		{name: "selected non-DNS UDP is blocked without relay", decision: processProxy, network: M.UDP, port: 443, want: "reject", wantError: "SOCKS5 UDP is unavailable"},
		{name: "unknown DNS stays direct", decision: processUnknown, network: M.UDP, port: 53, want: "direct"},
		{name: "engine loop guard fails closed", decision: processReject, network: M.TCP, port: 443, want: "reject", wantError: "ambiguous ownership"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dialers := map[string]proxy.Dialer{
				"socks":  &markerDialer{name: "socks"},
				"dns":    &markerDialer{name: "dns"},
				"direct": &markerDialer{name: "direct"},
				"reject": &markerDialer{name: "reject"},
			}
			d := &PerAppDialer{
				matcher:  fixedProcessMatcher{decision: tt.decision},
				socks:    dialers["socks"],
				dns:      dialers["dns"],
				direct:   dialers["direct"],
				reject:   dialers["reject"],
				proxyUDP: tt.proxyUDP,
			}
			metadata := &M.Metadata{
				Network: tt.network,
				DstIP:   netip.MustParseAddr("203.0.113.10"),
				DstPort: tt.port,
			}
			got, err := d.dialerFor(metadata)
			if got != dialers[tt.want] {
				t.Fatalf("dialer = %v, want %s", got, tt.want)
			}
			if tt.wantError == "" && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.wantError != "" && (err == nil || !strings.Contains(err.Error(), tt.wantError)) {
				t.Fatalf("error = %v, want substring %q", err, tt.wantError)
			}
		})
	}
}

func TestPerAppDialerRejectsMatcherErrors(t *testing.T) {
	reject := &markerDialer{name: "reject"}
	d := &PerAppDialer{
		matcher: fixedProcessMatcher{err: errors.New("snapshot failed")},
		reject:  reject,
	}
	got, err := d.dialerFor(&M.Metadata{Network: M.TCP, DstPort: 443})
	if got != reject {
		t.Fatalf("dialer = %v, want reject", got)
	}
	if err == nil || !strings.Contains(err.Error(), "identify connection owner: snapshot failed") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestPerAppDialerRoutesAllDNSThroughTrustedResolver(t *testing.T) {
	dns := &markerDialer{name: "dns"}
	d := &PerAppDialer{
		matcher:    fixedProcessMatcher{decision: processDirect},
		dns:        dns,
		direct:     &markerDialer{name: "direct"},
		reject:     &markerDialer{name: "reject"},
		trustedDNS: netip.MustParseAddrPort("8.8.8.8:53"),
	}
	for _, network := range []M.Network{M.UDP, M.TCP} {
		got, err := d.dialerFor(&M.Metadata{
			Network: network,
			DstIP:   netip.MustParseAddr("223.5.5.5"),
			DstPort: 53,
		})
		if err != nil || got != dns {
			t.Fatalf("%s system DNS route = %v, %v; want trusted DNS dialer", network, got, err)
		}
	}
}

func TestPerAppDialerResetClosesTrackedConnection(t *testing.T) {
	pipes := &pipeDialer{peers: make(chan net.Conn, 1)}
	d := &PerAppDialer{
		matcher: fixedProcessMatcher{decision: processProxy},
		socks:   pipes,
		reject:  &markerDialer{name: "reject"},
	}
	conn, err := d.DialContext(context.Background(), &M.Metadata{
		Network: M.TCP,
		DstIP:   netip.MustParseAddr("203.0.113.10"),
		DstPort: 443,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	peer := <-pipes.peers
	defer peer.Close()

	if closed := d.ResetConnections(); closed != 1 {
		t.Fatalf("closed flows = %d, want 1", closed)
	}
	if closed := d.ResetConnections(); closed != 0 {
		t.Fatalf("second reset closed flows = %d, want 0", closed)
	}
	buffer := make([]byte, 1)
	if _, err := peer.Read(buffer); err == nil {
		t.Fatal("peer remained open after flow reset")
	}
}

func TestFlowTrackerRejectsDialThatCrossesGeneration(t *testing.T) {
	var tracker flowTracker
	generation := tracker.currentGeneration()
	local, peer := net.Pipe()
	defer peer.Close()
	tracker.reset()
	if conn, err := tracker.trackConn(generation, local); err == nil || conn != nil {
		t.Fatalf("cross-generation track = (%v, %v), want retry error", conn, err)
	}
	buffer := make([]byte, 1)
	if _, err := peer.Read(buffer); err == nil {
		t.Fatal("cross-generation connection was not closed")
	}
}

func TestTrackedProxyDialerRebindClosesGlobalFlow(t *testing.T) {
	pipes := &pipeDialer{peers: make(chan net.Conn, 1)}
	d := &TrackedProxyDialer{socks: pipes}
	conn, err := d.DialContext(context.Background(), &M.Metadata{
		Network: M.TCP,
		DstIP:   netip.MustParseAddr("203.0.113.20"),
		DstPort: 443,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	peer := <-pipes.peers
	defer peer.Close()
	closed, err := d.RebindNetwork("192.168.50.37")
	if err != nil {
		t.Fatal(err)
	}
	if closed != 1 {
		t.Fatalf("closed global flows = %d, want 1", closed)
	}
	buffer := make([]byte, 1)
	if _, err := peer.Read(buffer); err == nil {
		t.Fatal("global proxy flow remained open after network rebind")
	}
}

func TestTrackedProxyDialerRebindCancelsInFlightDial(t *testing.T) {
	blocking := &blockingDialer{started: make(chan struct{})}
	d := &TrackedProxyDialer{socks: blocking}
	dialDone := make(chan error, 1)
	go func() {
		_, err := d.DialContext(context.Background(), &M.Metadata{
			Network: M.TCP,
			DstIP:   netip.MustParseAddr("203.0.113.20"),
			DstPort: 443,
		})
		dialDone <- err
	}()
	<-blocking.started

	closed, err := d.RebindNetwork("192.168.50.37")
	if err != nil {
		t.Fatal(err)
	}
	if closed != 0 {
		t.Fatalf("closed established flows = %d, want 0", closed)
	}
	select {
	case err := <-dialDone:
		if err == nil || !errors.Is(err, context.Canceled) {
			t.Fatalf("in-flight dial error = %v, want context cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("in-flight dial was not canceled by network rebind")
	}
}
