//go:build windows

package tunscope

import (
	"context"
	"fmt"
	"math/bits"
	"net"
	"syscall"

	"golang.org/x/sys/windows"
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
				// Windows expects IP_UNICAST_IF in network byte order.
				index := bits.ReverseBytes32(uint32(iface.Index))
				socketErr = windows.SetsockoptInt(windows.Handle(fd), windows.IPPROTO_IP, windowsIPUnicastIF, int(index))
			} else {
				socketErr = windows.SetsockoptInt(windows.Handle(fd), windows.IPPROTO_IPV6, windowsIPv6UnicastIF, iface.Index)
			}
		}); err != nil {
			return err
		}
		return socketErr
	}}
	return listenConfig.ListenPacket(context.Background(), network, source)
}
