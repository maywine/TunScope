//go:build darwin || windows

package tunscope

import (
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/xjasonlyu/tun2socks/v2/core"
	"github.com/xjasonlyu/tun2socks/v2/core/device"
	"github.com/xjasonlyu/tun2socks/v2/core/device/iobased"
	defaultdialer "github.com/xjasonlyu/tun2socks/v2/dialer"
	"github.com/xjasonlyu/tun2socks/v2/log"
	"github.com/xjasonlyu/tun2socks/v2/proxy"
	"github.com/xjasonlyu/tun2socks/v2/tunnel"
	"golang.zx2c4.com/wireguard/tun"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
)

// EngineDataPlane owns the native TUN and gVisor stack. TunScope creates this
// layer itself instead of calling tun2socks/engine so it can intercept ICMP
// before TCP and UDP packets enter the unchanged tun2socks transport stack.
type EngineDataPlane struct {
	device    *engineTUNDevice
	stack     *stack.Stack
	icmp      *icmpRelay
	closeOnce sync.Once
}

type engineTUNIO struct {
	native tun.Device
	offset int
	relay  *icmpRelay

	readSizes []int
	readBufs  [][]byte
	writeBufs [][]byte
	readMu    sync.Mutex
	writeMu   sync.Mutex
}

type engineTUNDevice struct {
	*iobased.Endpoint
	io   *engineTUNIO
	name string
}

func StartEngineDataPlane(cfg EngineConfig, dialer proxy.Dialer) (*EngineDataPlane, error) {
	if dialer == nil {
		return nil, fmt.Errorf("TUN transport dialer is nil")
	}
	level, err := log.ParseLevel(cfg.LogLevel)
	if err != nil {
		return nil, fmt.Errorf("parse engine log level: %w", err)
	}
	logger, err := log.NewLeveled(level)
	if err != nil {
		return nil, fmt.Errorf("create engine logger: %w", err)
	}
	log.SetLogger(logger)
	if cfg.Interface != "" {
		iface, err := net.InterfaceByName(cfg.Interface)
		if err != nil {
			return nil, fmt.Errorf("find proxy egress interface %s: %w", cfg.Interface, err)
		}
		defaultdialer.DefaultDialer.InterfaceName.Store(iface.Name)
		defaultdialer.DefaultDialer.InterfaceIndex.Store(int32(iface.Index))
		log.Infof("[DIALER] bind proxy transport to interface: %s", cfg.Interface)
	}
	tunnel.T().SetUDPTimeout(2 * time.Minute)
	tunnel.T().SetDialer(dialer)

	native, err := openNativeTUN(cfg.Device, cfg.MTU)
	if err != nil {
		return nil, err
	}
	closeNative := true
	defer func() {
		if closeNative {
			_ = native.Close()
		}
	}()
	mtu, err := native.MTU()
	if err != nil {
		return nil, fmt.Errorf("read TUN MTU: %w", err)
	}
	name, err := native.Name()
	if err != nil {
		return nil, fmt.Errorf("read TUN name: %w", err)
	}

	relay := newICMPRelay(cfg.ICMPDirect, cfg.IPv6, cfg.DirectInterface, cfg.DirectInterface6, mtu)
	rw := &engineTUNIO{
		native: native, offset: engineTUNOffset, relay: relay,
		readSizes: make([]int, 1), readBufs: make([][]byte, 1), writeBufs: make([][]byte, 1),
	}
	endpoint, err := iobased.New(rw, uint32(mtu), engineTUNOffset)
	if err != nil {
		return nil, fmt.Errorf("create TUN endpoint: %w", err)
	}
	tunDevice := &engineTUNDevice{Endpoint: endpoint, io: rw, name: name}
	relay.setInjector(rw.inject)
	if err := relay.start(cfg.DirectSource4); err != nil {
		endpoint.Close()
		return nil, fmt.Errorf("start direct ICMP data plane: %w", err)
	}

	netstack, err := core.CreateStack(&core.Config{
		LinkEndpoint:     tunDevice,
		TransportHandler: tunnel.T(),
	})
	if err != nil {
		_ = relay.close()
		endpoint.Close()
		return nil, fmt.Errorf("create TUN network stack: %w", err)
	}
	closeNative = false
	dataPlane := &EngineDataPlane{device: tunDevice, stack: netstack, icmp: relay}
	log.Infof("[STACK] tun://%s <-> SOCKS5 transport", name)
	if cfg.ICMPDirect {
		log.Infof("[ICMP] direct echo forwarding is active on %s (traffic bypasses SOCKS5)", cfg.DirectInterface)
	}
	return dataPlane, nil
}

func openNativeTUN(name string, mtu int) (native tun.Device, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("create TUN: %v", recovered)
		}
	}()
	configureNativeTUNPlatform()
	native, err = tun.CreateTUN(name, mtu)
	if err != nil {
		return nil, fmt.Errorf("create TUN: %w", err)
	}
	return native, nil
}

func (d *EngineDataPlane) InvalidateNetwork() (int, error) {
	if d == nil {
		return 0, nil
	}
	return d.icmp.invalidateNetwork()
}

func (d *EngineDataPlane) RebindNetwork(source4 string) (int, error) {
	if d == nil {
		return 0, nil
	}
	return d.icmp.rebindNetwork(source4)
}

func (d *EngineDataPlane) Close() error {
	if d == nil {
		return nil
	}
	var closeErr error
	d.closeOnce.Do(func() {
		closeErr = d.icmp.close()
		d.device.Close()
		d.stack.Close()
		d.stack.Wait()
	})
	return closeErr
}

func (rw *engineTUNIO) Read(packet []byte) (int, error) {
	rw.readMu.Lock()
	defer rw.readMu.Unlock()
	for {
		rw.readBufs[0] = packet
		packets, err := rw.native.Read(rw.readBufs, rw.readSizes, rw.offset)
		if err != nil {
			return 0, err
		}
		if packets == 0 || rw.readSizes[0] <= 0 || rw.offset+rw.readSizes[0] > len(packet) {
			continue
		}
		size := rw.readSizes[0]
		if rw.relay.handlePacket(packet[rw.offset : rw.offset+size]) {
			continue
		}
		return size, nil
	}
}

func (rw *engineTUNIO) Write(packet []byte) (int, error) {
	rw.writeMu.Lock()
	defer rw.writeMu.Unlock()
	rw.writeBufs[0] = packet
	return rw.native.Write(rw.writeBufs, rw.offset)
}

func (rw *engineTUNIO) inject(packet []byte) error {
	buffer := make([]byte, rw.offset+len(packet))
	copy(buffer[rw.offset:], packet)
	rw.writeMu.Lock()
	rw.writeBufs[0] = buffer
	_, err := rw.native.Write(rw.writeBufs, rw.offset)
	rw.writeMu.Unlock()
	return err
}

func (d *engineTUNDevice) Name() string { return d.name }
func (d *engineTUNDevice) Type() string { return "tun" }

func (d *engineTUNDevice) Close() {
	_ = d.io.native.Close()
	d.Endpoint.Close()
}

var _ device.Device = (*engineTUNDevice)(nil)
