//go:build !darwin && !windows

package tunscope

import (
	"fmt"
	"net"
)

func listenRawICMP(icmpFamily, string, string) (net.PacketConn, error) {
	return nil, fmt.Errorf("direct ICMP is supported on macOS and Windows only")
}
