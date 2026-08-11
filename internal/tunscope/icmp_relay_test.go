package tunscope

import (
	"encoding/binary"
	"io"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"
)

type fakeICMPPacket struct {
	message []byte
	addr    net.Addr
}

type fakeICMPConn struct {
	mu       sync.Mutex
	writes   []fakeICMPPacket
	reads    chan fakeICMPPacket
	closed   chan struct{}
	closeOne sync.Once
}

func newFakeICMPConn() *fakeICMPConn {
	return &fakeICMPConn{reads: make(chan fakeICMPPacket), closed: make(chan struct{})}
}

func (c *fakeICMPConn) ReadFrom(buffer []byte) (int, net.Addr, error) {
	select {
	case packet := <-c.reads:
		return copy(buffer, packet.message), packet.addr, nil
	case <-c.closed:
		return 0, nil, net.ErrClosed
	}
}

func (c *fakeICMPConn) WriteTo(message []byte, addr net.Addr) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	select {
	case <-c.closed:
		return 0, net.ErrClosed
	default:
	}
	c.writes = append(c.writes, fakeICMPPacket{message: append([]byte(nil), message...), addr: addr})
	return len(message), nil
}

func (c *fakeICMPConn) Close() error {
	c.closeOne.Do(func() { close(c.closed) })
	return nil
}
func (c *fakeICMPConn) LocalAddr() net.Addr              { return &net.IPAddr{} }
func (c *fakeICMPConn) SetDeadline(time.Time) error      { return nil }
func (c *fakeICMPConn) SetReadDeadline(time.Time) error  { return nil }
func (c *fakeICMPConn) SetWriteDeadline(time.Time) error { return nil }

func (c *fakeICMPConn) lastWrite() (fakeICMPPacket, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.writes) == 0 {
		return fakeICMPPacket{}, false
	}
	return c.writes[len(c.writes)-1], true
}

func TestICMPRelayMapsReplyAndRestoresIdentifier(t *testing.T) {
	conn4 := newFakeICMPConn()
	factory := func(family icmpFamily, _, _ string) (net.PacketConn, error) {
		if family != icmpFamily4 {
			t.Fatalf("unexpected socket family %d", family)
		}
		return conn4, nil
	}
	relay := newICMPRelayWithFactory(true, false, "en0", "", 1500, factory)
	injected := make(chan []byte, 1)
	relay.setInjector(func(packet []byte) error {
		injected <- append([]byte(nil), packet...)
		return nil
	})
	if err := relay.start("192.0.2.37"); err != nil {
		t.Fatal(err)
	}
	defer relay.close()

	request := testIPv4EchoPacket(netip.MustParseAddr("198.18.0.1"), netip.MustParseAddr("8.8.8.8"), 0x3344, 5, []byte("relay-data"))
	if !relay.handlePacket(request) {
		t.Fatal("echo request was not intercepted")
	}
	write, ok := conn4.lastWrite()
	if !ok {
		t.Fatal("raw echo request was not written")
	}
	relayID := binary.BigEndian.Uint16(write.message[4:6])
	if relayID == 0x3344 {
		t.Fatal("relay did not translate the echo identifier")
	}
	reply := append([]byte(nil), write.message...)
	reply[0] = icmpEchoReply4
	reply[2], reply[3] = 0, 0
	binary.BigEndian.PutUint16(reply[2:4], internetChecksum(reply))
	relay.mu.Lock()
	generation := relay.generation
	relay.mu.Unlock()
	relay.handleRawMessage(icmpFamily4, reply, netip.MustParseAddr("8.8.8.8"), generation)
	select {
	case packet := <-injected:
		if got := binary.BigEndian.Uint16(packet[24:26]); got != 0x3344 {
			t.Fatalf("injected identifier = %#x, want 0x3344", got)
		}
	case <-time.After(time.Second):
		t.Fatal("relay did not inject the echo reply")
	}
}

func TestICMPRelayNetworkRebindDropsStaleMapping(t *testing.T) {
	var mu sync.Mutex
	var connections []*fakeICMPConn
	factory := func(family icmpFamily, _, _ string) (net.PacketConn, error) {
		if family != icmpFamily4 {
			return nil, io.ErrUnexpectedEOF
		}
		conn := newFakeICMPConn()
		mu.Lock()
		connections = append(connections, conn)
		mu.Unlock()
		return conn, nil
	}
	relay := newICMPRelayWithFactory(true, false, "en0", "", 1500, factory)
	relay.setInjector(func([]byte) error { return nil })
	if err := relay.start("192.0.2.37"); err != nil {
		t.Fatal(err)
	}
	defer relay.close()
	request := testIPv4EchoPacket(netip.MustParseAddr("198.18.0.1"), netip.MustParseAddr("1.1.1.1"), 10, 1, nil)
	if !relay.handlePacket(request) {
		t.Fatal("request was not intercepted")
	}
	closed, err := relay.rebindNetwork("192.0.2.38")
	if err != nil {
		t.Fatal(err)
	}
	if closed != 1 {
		t.Fatalf("rebind cleared %d mappings, want 1", closed)
	}
	relay.mu.Lock()
	remaining := len(relay.mappings)
	generation := relay.generation
	relay.mu.Unlock()
	if remaining != 0 || generation == 0 {
		t.Fatalf("after rebind mappings=%d generation=%d", remaining, generation)
	}
	mu.Lock()
	count := len(connections)
	mu.Unlock()
	if count != 2 {
		t.Fatalf("raw socket count = %d, want 2", count)
	}
}
