package tunscope

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"strings"
	"sync"
	"time"

	M "github.com/xjasonlyu/tun2socks/v2/metadata"
	"github.com/xjasonlyu/tun2socks/v2/proxy"
)

const dnsTCPTimeout = 8 * time.Second

// dnsOverTCPDialer keeps DNS working when a local SOCKS5 server cannot carry
// UDP. Each UDP DNS request is translated to RFC 7766 DNS-over-TCP over
// SOCKS5, then translated back to UDP for the virtual network stack. In
// trusted-DNS mode the destination is also rewritten because macOS commonly
// resolves on behalf of applications inside mDNSResponder, after the original
// process identity has been lost.
type dnsOverTCPDialer struct {
	socks    proxy.Dialer
	resolver netip.AddrPort
}

func newDNSOverTCPDialer(socks proxy.Dialer, resolver netip.AddrPort) proxy.Dialer {
	return &dnsOverTCPDialer{socks: socks, resolver: resolver}
}

// checkTrustedDNS exercises the same UDP-to-TCP translation used by the live
// TUN before any system route is changed. A SOCKS5 server can allow HTTPS while
// denying TCP port 53, so the generic proxy CONNECT check is not sufficient.
func checkTrustedDNS(proxyURL string, resolver netip.AddrPort) error {
	socks, err := newSOCKS5Dialer(proxyURL)
	if err != nil {
		return err
	}
	packetConn, err := newDNSOverTCPDialer(socks, resolver).DialUDP(nil)
	if err != nil {
		return err
	}
	defer packetConn.Close()
	if err := packetConn.SetDeadline(time.Now().Add(6 * time.Second)); err != nil {
		return err
	}

	query := dnsProbeQuery()
	target := net.UDPAddrFromAddrPort(resolver)
	if _, err := packetConn.WriteTo(query, target); err != nil {
		return err
	}
	response := make([]byte, 4096)
	n, _, err := packetConn.ReadFrom(response)
	if err != nil {
		return err
	}
	if n < 12 || response[0] != query[0] || response[1] != query[1] || response[2]&0x80 == 0 {
		return fmt.Errorf("trusted DNS returned an invalid response")
	}
	return nil
}

func (d *dnsOverTCPDialer) DialContext(ctx context.Context, metadata *M.Metadata) (net.Conn, error) {
	if metadata == nil || metadata.DstPort != 53 {
		return nil, fmt.Errorf("DNS-over-TCP requires a DNS destination")
	}
	destination := *metadata
	if d.resolver.IsValid() {
		destination.DstIP = d.resolver.Addr()
		destination.DstPort = d.resolver.Port()
	}
	return d.socks.DialContext(ctx, &destination)
}

func (d *dnsOverTCPDialer) DialUDP(*M.Metadata) (net.PacketConn, error) {
	return &dnsTCPPacketConn{
		socks:    d.socks,
		resolver: d.resolver,
		results:  make(chan dnsTCPResult, 16),
		done:     make(chan struct{}),
	}, nil
}

type dnsTCPResult struct {
	payload []byte
	addr    net.Addr
	err     error
}

type dnsTCPPacketConn struct {
	socks    proxy.Dialer
	resolver netip.AddrPort

	results chan dnsTCPResult
	done    chan struct{}
	once    sync.Once

	deadlineMu    sync.RWMutex
	readDeadline  time.Time
	writeDeadline time.Time
}

func (c *dnsTCPPacketConn) WriteTo(payload []byte, addr net.Addr) (int, error) {
	destination, responseAddr, err := dnsDestination(addr, c.resolver)
	if err != nil {
		return 0, err
	}
	if len(payload) == 0 || len(payload) > 65535 {
		return 0, fmt.Errorf("invalid DNS message length %d", len(payload))
	}

	select {
	case <-c.done:
		return 0, net.ErrClosed
	default:
	}

	query := append([]byte(nil), payload...)
	go c.exchange(query, destination, responseAddr)
	return len(payload), nil
}

func (c *dnsTCPPacketConn) exchange(query []byte, destination *M.Metadata, responseAddr net.Addr) {
	deadline := c.currentWriteDeadline()
	if deadline.IsZero() || time.Until(deadline) > dnsTCPTimeout {
		deadline = time.Now().Add(dnsTCPTimeout)
	}
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()

	conn, err := c.socks.DialContext(ctx, destination)
	if err != nil {
		c.deliver(dnsTCPResult{err: fmt.Errorf("DNS-over-TCP SOCKS5 connect: %w", err)})
		return
	}
	defer conn.Close()
	_ = conn.SetDeadline(deadline)

	frame := make([]byte, len(query)+2)
	binary.BigEndian.PutUint16(frame, uint16(len(query)))
	copy(frame[2:], query)
	if _, err := conn.Write(frame); err != nil {
		c.deliver(dnsTCPResult{err: fmt.Errorf("DNS-over-TCP write: %w", err)})
		return
	}

	var size [2]byte
	if _, err := io.ReadFull(conn, size[:]); err != nil {
		c.deliver(dnsTCPResult{err: fmt.Errorf("DNS-over-TCP response header: %w", err)})
		return
	}
	responseSize := int(binary.BigEndian.Uint16(size[:]))
	if responseSize < 12 {
		c.deliver(dnsTCPResult{err: fmt.Errorf("DNS-over-TCP returned invalid message length %d", responseSize)})
		return
	}
	response := make([]byte, responseSize)
	if _, err := io.ReadFull(conn, response); err != nil {
		c.deliver(dnsTCPResult{err: fmt.Errorf("DNS-over-TCP response: %w", err)})
		return
	}
	c.deliver(dnsTCPResult{payload: response, addr: responseAddr})
}

func (c *dnsTCPPacketConn) deliver(result dnsTCPResult) {
	select {
	case c.results <- result:
	case <-c.done:
	}
}

func (c *dnsTCPPacketConn) ReadFrom(buffer []byte) (int, net.Addr, error) {
	deadline := c.currentReadDeadline()
	var timer *time.Timer
	var timeout <-chan time.Time
	if !deadline.IsZero() {
		duration := time.Until(deadline)
		if duration <= 0 {
			return 0, nil, os.ErrDeadlineExceeded
		}
		timer = time.NewTimer(duration)
		timeout = timer.C
		defer timer.Stop()
	}

	select {
	case result := <-c.results:
		if result.err != nil {
			return 0, nil, result.err
		}
		if len(result.payload) > len(buffer) {
			return 0, nil, io.ErrShortBuffer
		}
		return copy(buffer, result.payload), result.addr, nil
	case <-timeout:
		return 0, nil, os.ErrDeadlineExceeded
	case <-c.done:
		return 0, nil, net.ErrClosed
	}
}

func (c *dnsTCPPacketConn) Close() error {
	c.once.Do(func() { close(c.done) })
	return nil
}

func (c *dnsTCPPacketConn) LocalAddr() net.Addr {
	return &net.UDPAddr{IP: net.IPv4zero, Port: 0}
}

func (c *dnsTCPPacketConn) SetDeadline(deadline time.Time) error {
	c.deadlineMu.Lock()
	c.readDeadline = deadline
	c.writeDeadline = deadline
	c.deadlineMu.Unlock()
	return nil
}

func (c *dnsTCPPacketConn) SetReadDeadline(deadline time.Time) error {
	c.deadlineMu.Lock()
	c.readDeadline = deadline
	c.deadlineMu.Unlock()
	return nil
}

func (c *dnsTCPPacketConn) SetWriteDeadline(deadline time.Time) error {
	c.deadlineMu.Lock()
	c.writeDeadline = deadline
	c.deadlineMu.Unlock()
	return nil
}

func (c *dnsTCPPacketConn) currentReadDeadline() time.Time {
	c.deadlineMu.RLock()
	defer c.deadlineMu.RUnlock()
	return c.readDeadline
}

func (c *dnsTCPPacketConn) currentWriteDeadline() time.Time {
	c.deadlineMu.RLock()
	defer c.deadlineMu.RUnlock()
	return c.writeDeadline
}

func dnsDestination(addr net.Addr, resolver netip.AddrPort) (*M.Metadata, net.Addr, error) {
	var destination netip.AddrPort
	switch value := addr.(type) {
	case *M.Addr:
		destination = value.Metadata().DestinationAddrPort()
	case *net.UDPAddr:
		mapped, ok := netip.AddrFromSlice(value.IP)
		if !ok || value.Port < 1 || value.Port > 65535 {
			return nil, nil, fmt.Errorf("invalid DNS destination %q", value)
		}
		destination = netip.AddrPortFrom(mapped.Unmap(), uint16(value.Port))
	default:
		resolved, err := net.ResolveUDPAddr("udp", addr.String())
		if err != nil {
			return nil, nil, fmt.Errorf("resolve DNS destination %q: %w", addr, err)
		}
		return dnsDestination(resolved, resolver)
	}
	if !destination.IsValid() || destination.Port() != 53 {
		return nil, nil, fmt.Errorf("invalid DNS destination %s", destination)
	}
	responseAddr := net.UDPAddrFromAddrPort(destination)
	if resolver.IsValid() {
		destination = resolver
	}
	return &M.Metadata{
		Network: M.TCP,
		DstIP:   destination.Addr(),
		DstPort: destination.Port(),
	}, responseAddr, nil
}

func parseTrustedDNS(value string) (netip.AddrPort, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return netip.AddrPort{}, nil
	}
	if addr, err := netip.ParseAddr(value); err == nil {
		value = netip.AddrPortFrom(addr.Unmap(), 53).String()
	}
	resolver, err := netip.ParseAddrPort(value)
	if err != nil {
		return netip.AddrPort{}, fmt.Errorf("trusted DNS must be an IP address with an optional port: %q", value)
	}
	addr := resolver.Addr().Unmap()
	if addr.IsUnspecified() || addr.IsLoopback() || addr.IsMulticast() || addr.IsLinkLocalUnicast() || resolver.Port() == 0 {
		return netip.AddrPort{}, fmt.Errorf("trusted DNS is not usable: %q", value)
	}
	return netip.AddrPortFrom(addr, resolver.Port()), nil
}
