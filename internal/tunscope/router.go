package tunscope

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strings"
	"sync"

	"github.com/xjasonlyu/tun2socks/v2/log"
	M "github.com/xjasonlyu/tun2socks/v2/metadata"
	"github.com/xjasonlyu/tun2socks/v2/proxy"
)

type processDecision uint8

const (
	processUnknown processDecision = iota
	processDirect
	processProxy
	processReject
)

type processMatcher interface {
	Decide(*M.Metadata) (processDecision, error)
	Close() error
}

// PerAppDialer selects the SOCKS5 or physical-interface dialer for each flow.
// An unidentifiable owner stays on the physical network so a transient libproc
// miss cannot break an unrelated application. Connections proven to belong to
// this engine, or with conflicting ownership evidence, remain fail-closed.
type PerAppDialer struct {
	matcher    processMatcher
	socks      proxy.Dialer
	dns        proxy.Dialer
	direct     proxy.Dialer
	reject     proxy.Dialer
	proxyUDP   bool
	trustedDNS netip.AddrPort
	flows      flowTracker
}

// TrackedProxyDialer gives global mode the same in-place flow invalidation as
// per-app mode. It otherwise behaves exactly like the configured SOCKS5
// dialer, including UDP relay support.
type TrackedProxyDialer struct {
	socks proxy.Dialer
	flows flowTracker
}

// flowTracker owns the egress side of every connection created by the
// per-application router. Keeping the tracker in the engine lets the parent
// invalidate old TCP and UDP flows after a Wi-Fi handoff without tearing down
// the TUN interface or removing its capture routes.
type flowTracker struct {
	mu            sync.Mutex
	flows         map[trackedFlow]struct{}
	pendingDials  map[uint64]context.CancelFunc
	nextPendingID uint64
	generation    uint64
}

type trackedFlow interface {
	Close() error
}

type trackedConn struct {
	net.Conn
	tracker  *flowTracker
	once     sync.Once
	closeErr error
}

type trackedPacketConn struct {
	net.PacketConn
	tracker  *flowTracker
	once     sync.Once
	closeErr error
}

func (t *flowTracker) remove(flow trackedFlow) {
	t.mu.Lock()
	delete(t.flows, flow)
	t.mu.Unlock()
}

func (t *flowTracker) currentGeneration() uint64 {
	t.mu.Lock()
	generation := t.generation
	t.mu.Unlock()
	return generation
}

// beginDial attaches an in-flight TCP dial to the current physical-network
// generation. reset cancels it immediately during a Wi-Fi handoff instead of
// waiting for a stale source-address attempt to reach its own timeout.
func (t *flowTracker) beginDial(parent context.Context) (context.Context, uint64, func()) {
	ctx, cancel := context.WithCancel(parent)
	t.mu.Lock()
	t.nextPendingID++
	id := t.nextPendingID
	generation := t.generation
	if t.pendingDials == nil {
		t.pendingDials = make(map[uint64]context.CancelFunc)
	}
	t.pendingDials[id] = cancel
	t.mu.Unlock()
	finish := func() {
		t.mu.Lock()
		delete(t.pendingDials, id)
		t.mu.Unlock()
		cancel()
	}
	return ctx, generation, finish
}

func (t *flowTracker) trackConn(generation uint64, conn net.Conn) (net.Conn, error) {
	tracked := &trackedConn{Conn: conn, tracker: t}
	t.mu.Lock()
	if generation != t.generation {
		t.mu.Unlock()
		_ = conn.Close()
		return nil, fmt.Errorf("physical network changed while opening the connection; retry")
	}
	if t.flows == nil {
		t.flows = make(map[trackedFlow]struct{})
	}
	t.flows[tracked] = struct{}{}
	t.mu.Unlock()
	return tracked, nil
}

func (t *flowTracker) trackPacketConn(generation uint64, conn net.PacketConn) (net.PacketConn, error) {
	tracked := &trackedPacketConn{PacketConn: conn, tracker: t}
	t.mu.Lock()
	if generation != t.generation {
		t.mu.Unlock()
		_ = conn.Close()
		return nil, fmt.Errorf("physical network changed while opening the packet connection; retry")
	}
	if t.flows == nil {
		t.flows = make(map[trackedFlow]struct{})
	}
	t.flows[tracked] = struct{}{}
	t.mu.Unlock()
	return tracked, nil
}

func (t *flowTracker) reset() int {
	t.mu.Lock()
	t.generation++
	flows := make([]trackedFlow, 0, len(t.flows))
	for flow := range t.flows {
		flows = append(flows, flow)
	}
	t.flows = make(map[trackedFlow]struct{})
	pending := make([]context.CancelFunc, 0, len(t.pendingDials))
	for _, cancel := range t.pendingDials {
		pending = append(pending, cancel)
	}
	t.pendingDials = make(map[uint64]context.CancelFunc)
	t.mu.Unlock()
	for _, cancel := range pending {
		cancel()
	}
	for _, flow := range flows {
		_ = flow.Close()
	}
	return len(flows)
}

func (c *trackedConn) Close() error {
	c.once.Do(func() {
		c.tracker.remove(c)
		c.closeErr = c.Conn.Close()
	})
	return c.closeErr
}

func (c *trackedPacketConn) Close() error {
	c.once.Do(func() {
		c.tracker.remove(c)
		c.closeErr = c.PacketConn.Close()
	})
	return c.closeErr
}

func newSOCKS5Dialer(proxyURL string) (proxy.Dialer, error) {
	u, err := url.Parse(proxyURL)
	if err != nil {
		return nil, err
	}
	if !strings.EqualFold(u.Scheme, "socks5") || u.Host == "" {
		return nil, fmt.Errorf("routing requires a SOCKS5 URL with host and port")
	}
	username := ""
	password := ""
	if u.User != nil {
		username = u.User.Username()
		password, _ = u.User.Password()
	}
	socks, err := proxy.NewSocks5(u.Host, username, password)
	if err != nil {
		return nil, err
	}
	return socks, nil
}

func NewTrackedProxyDialer(proxyURL string) (*TrackedProxyDialer, error) {
	socks, err := newSOCKS5Dialer(proxyURL)
	if err != nil {
		return nil, err
	}
	return &TrackedProxyDialer{socks: socks}, nil
}

func (d *TrackedProxyDialer) DialContext(ctx context.Context, metadata *M.Metadata) (net.Conn, error) {
	dialCtx, generation, finish := d.flows.beginDial(ctx)
	defer finish()
	conn, err := d.socks.DialContext(dialCtx, metadata)
	if err != nil {
		return nil, fmt.Errorf("proxy path: %w", err)
	}
	return d.flows.trackConn(generation, conn)
}

func (d *TrackedProxyDialer) DialUDP(metadata *M.Metadata) (net.PacketConn, error) {
	generation := d.flows.currentGeneration()
	conn, err := d.socks.DialUDP(metadata)
	if err != nil {
		return nil, fmt.Errorf("proxy path: %w", err)
	}
	return d.flows.trackPacketConn(generation, conn)
}

func (d *TrackedProxyDialer) RebindNetwork(string) (int, error) {
	if d == nil {
		return 0, fmt.Errorf("global proxy dialer is nil")
	}
	return d.flows.reset(), nil
}

func (d *TrackedProxyDialer) Close() error {
	if d != nil {
		d.flows.reset()
	}
	return nil
}

func NewPerAppDialer(proxyURL string, applicationPaths []string, proxyUDP bool, trustedDNS, directInterface4, directInterface6, directSource4 string) (*PerAppDialer, error) {
	socks, err := newSOCKS5Dialer(proxyURL)
	if err != nil {
		return nil, err
	}
	resolver, err := parseTrustedDNS(trustedDNS)
	if err != nil {
		return nil, err
	}
	matcher, err := newProcessMatcher(applicationPaths)
	if err != nil {
		return nil, err
	}
	direct, err := newBoundDirectDialer(directInterface4, directInterface6, directSource4)
	if err != nil {
		matcher.Close()
		return nil, err
	}
	return &PerAppDialer{
		matcher:    matcher,
		socks:      socks,
		dns:        newDNSOverTCPDialer(socks, resolver),
		direct:     direct,
		reject:     proxy.NewReject(),
		proxyUDP:   proxyUDP,
		trustedDNS: resolver,
	}, nil
}

func (d *PerAppDialer) DialContext(ctx context.Context, metadata *M.Metadata) (net.Conn, error) {
	dialCtx, generation, finish := d.flows.beginDial(ctx)
	defer finish()
	dialer, err := d.dialerFor(metadata)
	if err != nil {
		return nil, err
	}
	conn, err := dialer.DialContext(dialCtx, metadata)
	if err != nil {
		return nil, fmt.Errorf("%s path: %w", d.dialerLabel(dialer), err)
	}
	return d.flows.trackConn(generation, conn)
}

func (d *PerAppDialer) DialUDP(metadata *M.Metadata) (net.PacketConn, error) {
	generation := d.flows.currentGeneration()
	dialer, err := d.dialerFor(metadata)
	if err != nil {
		return nil, err
	}
	conn, err := dialer.DialUDP(metadata)
	if err != nil {
		return nil, fmt.Errorf("%s path: %w", d.dialerLabel(dialer), err)
	}
	return d.flows.trackPacketConn(generation, conn)
}

func (d *PerAppDialer) dialerFor(metadata *M.Metadata) (proxy.Dialer, error) {
	// macOS commonly performs getaddrinfo on behalf of an application inside
	// mDNSResponder, where the original application identity is no longer
	// observable. When trusted DNS is enabled, capture every port-53 flow and
	// send it over SOCKS5 so a Wi-Fi resolver cannot poison selected apps.
	if d.trustedDNS.IsValid() && metadata != nil && metadata.DstPort == 53 &&
		(metadata.Network == M.UDP || metadata.Network == M.TCP) {
		return d.dns, nil
	}
	decision, err := d.matcher.Decide(metadata)
	if err != nil {
		return d.reject, fmt.Errorf("identify connection owner: %w", err)
	}
	log.Debugf(
		"[ROUTE] decision=%s network=%s %s -> %s",
		decisionName(decision),
		metadata.Network,
		metadata.SourceAddress(),
		metadata.DestinationAddress(),
	)
	switch decision {
	case processProxy:
		// A selected application still gets working DNS when the SOCKS5 server
		// has no UDP relay or TCP-only compatibility mode is enabled. System
		// resolver traffic is routed outside TUN in per-app mode, so this branch
		// only handles DNS sockets that can actually be attributed to a selected
		// process.
		if metadata.Network == M.UDP && metadata.DstPort == 53 {
			return d.dns, nil
		}
		if metadata.Network == M.UDP && !d.proxyUDP {
			return d.reject, fmt.Errorf("SOCKS5 UDP is unavailable; blocked so the selected application can fall back to TCP")
		}
		return d.socks, nil
	case processDirect:
		return d.direct, nil
	case processUnknown:
		return d.direct, nil
	case processReject:
		return d.reject, fmt.Errorf("connection belongs to the TUN data plane or has ambiguous ownership; blocked to prevent a routing loop")
	default:
		return d.reject, fmt.Errorf("invalid connection ownership decision")
	}
}

func decisionName(decision processDecision) string {
	switch decision {
	case processProxy:
		return "proxy"
	case processDirect:
		return "direct"
	case processUnknown:
		return "unknown-direct"
	case processReject:
		return "reject"
	default:
		return "invalid"
	}
}

func (d *PerAppDialer) dialerLabel(dialer proxy.Dialer) string {
	switch dialer {
	case d.socks:
		return "proxy"
	case d.dns:
		return "proxy-dns-tcp"
	case d.direct:
		return "direct"
	case d.reject:
		return "reject"
	default:
		return "unknown"
	}
}

func (d *PerAppDialer) Close() error {
	if d == nil || d.matcher == nil {
		return nil
	}
	d.ResetConnections()
	return d.matcher.Close()
}

// ResetConnections closes all egress sockets while leaving the TUN device and
// its capture routes intact. The virtual peers see their old flows fail and
// immediately create fresh sockets against the reconciled physical network.
func (d *PerAppDialer) ResetConnections() int {
	if d == nil {
		return 0
	}
	return d.flows.reset()
}

// RebindNetwork publishes the new physical source before advancing the flow
// generation. A dial which raced with the update is rejected by the generation
// check, so no connection created with the removed address can survive.
func (d *PerAppDialer) RebindNetwork(source4 string) (int, error) {
	if d == nil {
		return 0, fmt.Errorf("per-app dialer is nil")
	}
	direct, ok := d.direct.(*boundDirectDialer)
	if !ok {
		return 0, fmt.Errorf("direct dialer does not support network rebinding")
	}
	if err := direct.setSource4(source4); err != nil {
		return 0, err
	}
	return d.flows.reset(), nil
}
