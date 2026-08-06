//go:build !darwin && !windows

package tunscope

import (
	"context"
	"fmt"
	"net"

	M "github.com/xjasonlyu/tun2socks/v2/metadata"
	"github.com/xjasonlyu/tun2socks/v2/proxy"
)

type boundDirectDialer struct{}

func newBoundDirectDialer(string, string, string) (proxy.Dialer, error) {
	return nil, fmt.Errorf("physical-interface direct routing is supported on macOS and Windows only")
}

func (*boundDirectDialer) DialContext(context.Context, *M.Metadata) (net.Conn, error) {
	return nil, fmt.Errorf("physical-interface direct routing is unavailable")
}

func (*boundDirectDialer) DialUDP(*M.Metadata) (net.PacketConn, error) {
	return nil, fmt.Errorf("physical-interface direct routing is unavailable")
}

func (*boundDirectDialer) setSource4(string) error {
	return fmt.Errorf("physical-interface direct routing is unavailable")
}
