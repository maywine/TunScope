//go:build darwin

package tunscope

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"syscall"

	M "github.com/xjasonlyu/tun2socks/v2/metadata"
	"github.com/xjasonlyu/tun2socks/v2/proxy"
	"golang.org/x/sys/unix"
)

// boundDirectDialer is used only for applications that are not selected.
// Binding this branch, instead of tun2socks' global dialer, lets a local
// SOCKS5 server keep using loopback while direct traffic bypasses TUN routes.
type boundDirectDialer struct {
	interface4Index int
	interface6Index int
	mu              sync.RWMutex
	source4         netip.Addr
}

func newBoundDirectDialer(interface4, interface6, source4 string) (proxy.Dialer, error) {
	if interface4 == "" {
		return nil, fmt.Errorf("an IPv4 direct interface is required")
	}
	iface4, err := net.InterfaceByName(interface4)
	if err != nil {
		return nil, fmt.Errorf("find IPv4 direct interface %s: %w", interface4, err)
	}
	d := &boundDirectDialer{interface4Index: iface4.Index}
	if err := d.setSource4(source4); err != nil {
		return nil, err
	}
	if interface6 != "" {
		iface6, err := net.InterfaceByName(interface6)
		if err != nil {
			return nil, fmt.Errorf("find IPv6 direct interface %s: %w", interface6, err)
		}
		d.interface6Index = iface6.Index
	}
	return d, nil
}

func (d *boundDirectDialer) DialContext(ctx context.Context, metadata *M.Metadata) (net.Conn, error) {
	network := "tcp4"
	if metadata.DstIP.Is6() {
		network = "tcp6"
	}
	control, err := d.control(metadata.DstIP.Is6())
	if err != nil {
		return nil, err
	}
	dialer := net.Dialer{Control: control}
	if !metadata.DstIP.Is6() {
		if source4 := d.currentSource4(); source4.IsValid() {
			dialer.LocalAddr = &net.TCPAddr{IP: net.IP(source4.AsSlice())}
		}
	}
	conn, err := dialer.DialContext(ctx, network, metadata.DestinationAddress())
	if err != nil {
		return nil, err
	}
	if tcpConn, ok := conn.(*net.TCPConn); ok {
		_ = tcpConn.SetKeepAlive(true)
	}
	return conn, nil
}

func (d *boundDirectDialer) DialUDP(metadata *M.Metadata) (net.PacketConn, error) {
	network := "udp4"
	address := "0.0.0.0:0"
	if metadata.DstIP.Is6() {
		network = "udp6"
		address = "[::]:0"
	} else if source4 := d.currentSource4(); source4.IsValid() {
		address = net.JoinHostPort(source4.String(), "0")
	}
	control, err := d.control(metadata.DstIP.Is6())
	if err != nil {
		return nil, err
	}
	listenConfig := net.ListenConfig{Control: control}
	packetConn, err := listenConfig.ListenPacket(context.Background(), network, address)
	if err != nil {
		return nil, err
	}
	return &boundDirectPacketConn{PacketConn: packetConn}, nil
}

func (d *boundDirectDialer) setSource4(value string) error {
	var source netip.Addr
	if value != "" {
		parsed, err := netip.ParseAddr(value)
		if err != nil || !parsed.Is4() || parsed.IsUnspecified() || parsed.IsLoopback() || parsed.IsLinkLocalUnicast() {
			return fmt.Errorf("direct IPv4 source is not usable: %q", value)
		}
		source = parsed.Unmap()
	}
	d.mu.Lock()
	d.source4 = source
	d.mu.Unlock()
	return nil
}

func (d *boundDirectDialer) currentSource4() netip.Addr {
	d.mu.RLock()
	source := d.source4
	d.mu.RUnlock()
	return source
}

func (d *boundDirectDialer) control(ipv6 bool) (func(string, string, syscall.RawConn) error, error) {
	index := d.interface4Index
	if ipv6 {
		index = d.interface6Index
	}
	if index == 0 {
		return nil, fmt.Errorf("no direct interface is available for IPv6")
	}
	return func(network, _ string, raw syscall.RawConn) error {
		var socketErr error
		if err := raw.Control(func(fd uintptr) {
			switch network {
			case "tcp4", "udp4":
				socketErr = unix.SetsockoptInt(int(fd), syscall.IPPROTO_IP, syscall.IP_BOUND_IF, index)
			case "tcp6", "udp6":
				socketErr = unix.SetsockoptInt(int(fd), syscall.IPPROTO_IPV6, syscall.IPV6_BOUND_IF, index)
			default:
				socketErr = fmt.Errorf("unsupported direct network %s", network)
			}
		}); err != nil {
			return err
		}
		return socketErr
	}, nil
}

type boundDirectPacketConn struct {
	net.PacketConn
}

// probeBoundDirectIPv4Route performs the same source-address and IP_BOUND_IF
// route lookup used by unselected applications without sending a packet. A
// connected UDP socket asks XNU to resolve and cache the egress route during
// connect(2), which exposes stale interface-scoped routes that route(8) can
// still print after a Wi-Fi roam.
func probeBoundDirectIPv4Route(interfaceName, source4 string) error {
	return probeBoundDirectIPv4RouteTo(
		interfaceName,
		source4,
		netip.MustParseAddr("192.0.2.1"),
	)
}

func probeBoundDirectIPv4RouteTo(interfaceName, source4 string, destination netip.Addr) error {
	if interfaceName == "" {
		return fmt.Errorf("an IPv4 direct interface is required")
	}
	iface, err := net.InterfaceByName(interfaceName)
	if err != nil {
		return fmt.Errorf("find IPv4 direct interface %s: %w", interfaceName, err)
	}
	source, err := netip.ParseAddr(source4)
	if err != nil || !source.Is4() || source.IsUnspecified() {
		return fmt.Errorf("direct IPv4 probe source is not usable: %q", source4)
	}
	if !destination.IsValid() || !destination.Is4() || destination.IsUnspecified() {
		return fmt.Errorf("direct IPv4 probe destination is not usable: %q", destination)
	}

	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM, unix.IPPROTO_UDP)
	if err != nil {
		return fmt.Errorf("open direct IPv4 probe socket: %w", err)
	}
	unix.CloseOnExec(fd)
	defer unix.Close(fd)
	if err := unix.SetsockoptInt(fd, syscall.IPPROTO_IP, syscall.IP_BOUND_IF, iface.Index); err != nil {
		return fmt.Errorf("bind direct IPv4 probe to %s: %w", interfaceName, err)
	}
	if err := unix.Bind(fd, &unix.SockaddrInet4{Addr: source.Unmap().As4()}); err != nil {
		return fmt.Errorf("bind direct IPv4 probe source %s: %w", source, err)
	}
	if err := unix.Connect(fd, &unix.SockaddrInet4{
		Port: 9,
		Addr: destination.Unmap().As4(),
	}); err != nil {
		return fmt.Errorf("resolve direct IPv4 route to %s on %s: %w", destination, interfaceName, err)
	}
	return nil
}

func (c *boundDirectPacketConn) WriteTo(payload []byte, addr net.Addr) (int, error) {
	switch value := addr.(type) {
	case *M.Addr:
		udpAddr := value.Metadata().UDPAddr()
		if udpAddr == nil {
			return 0, fmt.Errorf("invalid UDP destination %s", value)
		}
		return c.PacketConn.WriteTo(payload, udpAddr)
	case *net.UDPAddr:
		return c.PacketConn.WriteTo(payload, value)
	default:
		udpAddr, err := net.ResolveUDPAddr("udp", addr.String())
		if err != nil {
			return 0, err
		}
		return c.PacketConn.WriteTo(payload, udpAddr)
	}
}
