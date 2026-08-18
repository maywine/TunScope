package tunscope

import (
	"errors"
	"net"
	"net/netip"
	"syscall"
	"testing"
	"time"

	M "github.com/xjasonlyu/tun2socks/v2/metadata"
	"golang.org/x/sys/unix"
)

func TestBoundDirectUDPSetsIPv4BoundInterface(t *testing.T) {
	loopback, err := net.InterfaceByName("lo0")
	if err != nil {
		t.Fatal(err)
	}
	rawDialer, err := newBoundDirectDialer("lo0", "lo0", "")
	if err != nil {
		t.Fatal(err)
	}
	dialer := rawDialer.(*boundDirectDialer)
	packetConn, err := dialer.DialUDP(&M.Metadata{
		Network: M.UDP,
		DstIP:   netip.MustParseAddr("127.0.0.1"),
		DstPort: 53,
	})
	if err != nil {
		if errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES) {
			t.Skipf("sandbox does not permit a UDP socket: %v", err)
		}
		t.Fatal(err)
	}
	defer packetConn.Close()

	wrapped := packetConn.(*boundDirectPacketConn)
	udpConn := wrapped.PacketConn.(*net.UDPConn)
	raw, err := udpConn.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	got := 0
	if err := raw.Control(func(fd uintptr) {
		got, err = unix.GetsockoptInt(int(fd), syscall.IPPROTO_IP, syscall.IP_BOUND_IF)
	}); err != nil {
		t.Fatal(err)
	}
	if err != nil {
		t.Fatal(err)
	}
	if got != loopback.Index {
		t.Fatalf("IP_BOUND_IF = %d, want %d", got, loopback.Index)
	}
}

func TestBoundDirectDialerRejectsMissingInterface(t *testing.T) {
	if _, err := newBoundDirectDialer("tunscope-interface-does-not-exist", "", ""); err == nil {
		t.Fatal("expected a missing-interface error")
	}
}

func TestProbeBoundDirectIPv4RouteToLoopback(t *testing.T) {
	err := probeBoundDirectIPv4RouteTo(
		"lo0",
		"127.0.0.1",
		netip.MustParseAddr("127.0.0.1"),
	)
	if err != nil {
		if errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES) {
			t.Skipf("sandbox does not permit a UDP route probe: %v", err)
		}
		t.Fatal(err)
	}
}

func TestBoundDirectDialerUpdatesValidatedIPv4Source(t *testing.T) {
	rawDialer, err := newBoundDirectDialer("lo0", "", "192.0.2.10")
	if err != nil {
		t.Fatal(err)
	}
	dialer := rawDialer.(*boundDirectDialer)
	if got := dialer.currentSource4().String(); got != "192.0.2.10" {
		t.Fatalf("initial source = %q", got)
	}
	if err := dialer.setSource4("192.0.2.37"); err != nil {
		t.Fatal(err)
	}
	if got := dialer.currentSource4().String(); got != "192.0.2.37" {
		t.Fatalf("updated source = %q", got)
	}
	if err := dialer.setSource4("127.0.0.1"); err == nil {
		t.Fatal("expected loopback source to be rejected")
	}
}

func TestPerAppDialerRecoveryClosesUDPFlowOpenedWhileUnavailable(t *testing.T) {
	rawDirect, err := newBoundDirectDialer("lo0", "", "192.0.2.10")
	if err != nil {
		t.Fatal(err)
	}
	direct := rawDirect.(*boundDirectDialer)
	d := &PerAppDialer{direct: direct}

	first, firstPeer := net.Pipe()
	defer firstPeer.Close()
	if _, err := d.flows.trackConn(d.flows.currentGeneration(), first); err != nil {
		t.Fatal(err)
	}
	closed, err := d.InvalidateNetwork()
	if err != nil {
		t.Fatal(err)
	}
	if closed != 1 {
		t.Fatalf("flows closed at invalidation = %d, want 1", closed)
	}
	if source := direct.currentSource4(); source.IsValid() {
		t.Fatalf("direct source after invalidation = %s, want unset", source)
	}

	gapFlow, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		if errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES) {
			t.Skipf("sandbox does not permit a UDP socket: %v", err)
		}
		t.Fatal(err)
	}
	trackedGapFlow, err := d.flows.trackPacketConn(d.flows.currentGeneration(), gapFlow)
	if err != nil {
		t.Fatal(err)
	}
	defer trackedGapFlow.Close()
	closed, err = d.RebindNetwork("192.0.2.37")
	if err != nil {
		t.Fatal(err)
	}
	if closed != 1 {
		t.Fatalf("flows closed at recovery = %d, want 1", closed)
	}
	if err := gapFlow.SetDeadline(time.Now()); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("UDP flow opened while unavailable close error = %v, want net.ErrClosed", err)
	}
	if source := direct.currentSource4().String(); source != "192.0.2.37" {
		t.Fatalf("direct source after recovery = %q", source)
	}

	stable, stablePeer := net.Pipe()
	defer stablePeer.Close()
	if _, err := d.flows.trackConn(d.flows.currentGeneration(), stable); err != nil {
		t.Fatal(err)
	}
	closed, err = d.RebindNetwork("192.0.2.38")
	if err != nil {
		t.Fatal(err)
	}
	if closed != 1 {
		t.Fatalf("flows closed by later live change = %d, want 1", closed)
	}
}
