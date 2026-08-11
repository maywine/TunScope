//go:build darwin

package tunscope

import (
	"context"
	"fmt"
	"net"
	"syscall"

	"golang.org/x/sys/unix"
)

func listenRawICMP(family icmpFamily, interfaceName, source string) (net.PacketConn, error) {
	iface, err := net.InterfaceByName(interfaceName)
	if err != nil {
		return nil, fmt.Errorf("find physical interface %s: %w", interfaceName, err)
	}
	network := "ip4:icmp"
	if family == icmpFamily6 {
		network = "ip6:ipv6-icmp"
	}
	listenConfig := net.ListenConfig{Control: func(_ string, _ string, raw syscall.RawConn) error {
		var socketErr error
		if err := raw.Control(func(fd uintptr) {
			if family == icmpFamily4 {
				socketErr = unix.SetsockoptInt(int(fd), syscall.IPPROTO_IP, syscall.IP_BOUND_IF, iface.Index)
			} else {
				socketErr = unix.SetsockoptInt(int(fd), syscall.IPPROTO_IPV6, syscall.IPV6_BOUND_IF, iface.Index)
			}
		}); err != nil {
			return err
		}
		return socketErr
	}}
	return listenConfig.ListenPacket(context.Background(), network, source)
}
