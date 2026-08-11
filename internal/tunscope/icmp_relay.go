package tunscope

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/xjasonlyu/tun2socks/v2/log"
)

const (
	icmpMappingLifetime = 30 * time.Second
	maximumICMPMappings = 4096
)

type rawICMPFactory func(icmpFamily, string, string) (net.PacketConn, error)

// icmpRelay translates echo identifiers between packets read from the TUN and
// privileged raw sockets bound to the physical interface. It deliberately has
// no process matcher: ICMP has no TCP/UDP owner tuple on macOS or Windows, so
// enabling this feature means ICMP from every local process bypasses SOCKS.
type icmpRelay struct {
	enabled    bool
	ipv6       bool
	interface4 string
	interface6 string
	mtu        int
	factory    rawICMPFactory

	mu         sync.Mutex
	closed     bool
	generation uint64
	nextID     uint16
	conn4      net.PacketConn
	conn6      net.PacketConn
	mappings   map[icmpMappingKey]icmpMapping
	ids4       map[uint16]struct{}
	ids6       map[uint16]struct{}
	inject     func([]byte) error
	wg         sync.WaitGroup
}

func newICMPRelay(enabled, ipv6 bool, interface4, interface6 string, mtu int) *icmpRelay {
	return newICMPRelayWithFactory(enabled, ipv6, interface4, interface6, mtu, listenRawICMP)
}

func newICMPRelayWithFactory(enabled, ipv6 bool, interface4, interface6 string, mtu int, factory rawICMPFactory) *icmpRelay {
	var seed [2]byte
	if _, err := rand.Read(seed[:]); err != nil {
		binary.BigEndian.PutUint16(seed[:], uint16(time.Now().UnixNano()))
	}
	return &icmpRelay{
		enabled: enabled, ipv6: ipv6,
		interface4: interface4, interface6: interface6,
		mtu: mtu, factory: factory,
		nextID:   binary.BigEndian.Uint16(seed[:]),
		mappings: make(map[icmpMappingKey]icmpMapping),
		ids4:     make(map[uint16]struct{}), ids6: make(map[uint16]struct{}),
	}
}

func (r *icmpRelay) setInjector(inject func([]byte) error) {
	r.mu.Lock()
	r.inject = inject
	r.mu.Unlock()
}

func (r *icmpRelay) start(source4 string) error {
	if r == nil || !r.enabled {
		return nil
	}
	return r.activate(source4)
}

func (r *icmpRelay) activate(source4 string) error {
	if r.factory == nil {
		return fmt.Errorf("raw ICMP socket factory is unavailable")
	}
	if r.interface4 == "" {
		return fmt.Errorf("a physical IPv4 interface is required for direct ICMP")
	}
	source, err := netip.ParseAddr(source4)
	if err != nil || !source.Is4() || source.IsUnspecified() || source.IsLoopback() || source.IsLinkLocalUnicast() {
		return fmt.Errorf("direct ICMP IPv4 source is not usable: %q", source4)
	}
	conn4, err := r.factory(icmpFamily4, r.interface4, source.Unmap().String())
	if err != nil {
		return fmt.Errorf("open physical IPv4 ICMP socket on %s: %w", r.interface4, err)
	}
	var conn6 net.PacketConn
	if r.ipv6 && r.interface6 != "" {
		conn6, err = r.factory(icmpFamily6, r.interface6, "::")
		if err != nil {
			_ = conn4.Close()
			return fmt.Errorf("open physical IPv6 ICMP socket on %s: %w", r.interface6, err)
		}
	}

	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		_ = conn4.Close()
		if conn6 != nil {
			_ = conn6.Close()
		}
		return net.ErrClosed
	}
	if r.conn4 != nil || r.conn6 != nil {
		r.mu.Unlock()
		_ = conn4.Close()
		if conn6 != nil {
			_ = conn6.Close()
		}
		return fmt.Errorf("direct ICMP sockets are already active")
	}
	generation := r.generation
	r.conn4, r.conn6 = conn4, conn6
	r.wg.Add(1)
	go r.readLoop(icmpFamily4, conn4, generation)
	if conn6 != nil {
		r.wg.Add(1)
		go r.readLoop(icmpFamily6, conn6, generation)
	}
	r.mu.Unlock()
	return nil
}

// handlePacket returns true when the packet belongs to the direct ICMP data
// plane. A true result means the caller must not also inject it into gVisor.
func (r *icmpRelay) handlePacket(packet []byte) bool {
	if r == nil || !r.enabled {
		return false
	}
	request, ok := parseICMPEchoRequest(packet)
	if !ok {
		return false
	}
	if err := r.sendEcho(request); err != nil {
		log.Debugf("[ICMP] drop direct echo %s -> %s: %v", request.source, request.target, err)
	}
	return true
}

func (r *icmpRelay) sendEcho(request icmpEchoRequest) error {
	now := time.Now()
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return net.ErrClosed
	}
	r.expireMappingsLocked(now)
	if len(r.mappings) >= maximumICMPMappings {
		r.mu.Unlock()
		return fmt.Errorf("too many outstanding direct ICMP requests")
	}
	conn := r.conn4
	if request.family == icmpFamily6 {
		conn = r.conn6
	}
	if conn == nil {
		r.mu.Unlock()
		return fmt.Errorf("physical IPv%d ICMP socket is paused", request.family)
	}
	relayID, ok := r.allocateIDLocked(request.family)
	if !ok {
		r.mu.Unlock()
		return fmt.Errorf("direct ICMP identifier space is exhausted")
	}
	key := icmpMappingKey{family: request.family, relayID: relayID, sequence: request.sequence}
	mapping := icmpMapping{
		family: request.family, relayID: relayID,
		sequence: request.sequence, originalID: request.originalID,
		source: request.source, target: request.target,
		originalPacket: request.originalPacket,
		requestData:    append([]byte(nil), request.message[icmpHeaderSize:]...),
		createdAt:      now, generation: r.generation,
	}
	r.mappings[key] = mapping
	generation := r.generation
	r.mu.Unlock()

	message := relayedEchoRequest(request.message, request.family, relayID)
	zone := ""
	if request.family == icmpFamily6 && request.target.IsLinkLocalUnicast() {
		zone = r.interface6
	}
	_, err := conn.WriteTo(message, &net.IPAddr{IP: net.IP(request.target.AsSlice()), Zone: zone})
	if err != nil {
		r.removeMapping(key, generation)
		return err
	}
	return nil
}

func (r *icmpRelay) allocateIDLocked(family icmpFamily) (uint16, bool) {
	ids := r.ids4
	if family == icmpFamily6 {
		ids = r.ids6
	}
	for attempts := 0; attempts <= 0xffff; attempts++ {
		r.nextID++
		candidate := r.nextID
		if _, exists := ids[candidate]; exists {
			continue
		}
		ids[candidate] = struct{}{}
		return candidate, true
	}
	return 0, false
}

func (r *icmpRelay) readLoop(family icmpFamily, conn net.PacketConn, generation uint64) {
	defer r.wg.Done()
	buffer := make([]byte, 64<<10)
	for {
		n, source, err := conn.ReadFrom(buffer)
		if err != nil {
			if !r.connectionCurrent(family, conn, generation) || errors.Is(err, net.ErrClosed) {
				return
			}
			log.Warnf("[ICMP] physical IPv%d receive failed: %v", family, err)
			return
		}
		responder, ok := addressFromNetAddr(source)
		wantBits := 32
		if family == icmpFamily6 {
			wantBits = 128
		}
		if !ok || responder.BitLen() != wantBits || n < icmpHeaderSize {
			continue
		}
		r.handleRawMessage(family, append([]byte(nil), buffer[:n]...), responder, generation)
	}
}

func (r *icmpRelay) connectionCurrent(family icmpFamily, conn net.PacketConn, generation uint64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.generation != generation {
		return false
	}
	if family == icmpFamily4 {
		return r.conn4 == conn
	}
	return r.conn6 == conn
}

func (r *icmpRelay) handleRawMessage(family icmpFamily, message []byte, responder netip.Addr, generation uint64) {
	if len(message) < icmpHeaderSize {
		return
	}
	if family == icmpFamily4 && !checksumValid(message) {
		return
	}
	var key icmpMappingKey
	var embeddedTarget netip.Addr
	var isError bool
	switch {
	case family == icmpFamily4 && message[0] == icmpEchoReply4 && message[1] == 0:
		key = icmpMappingKey{family: family, relayID: binary.BigEndian.Uint16(message[4:6]), sequence: binary.BigEndian.Uint16(message[6:8])}
		embeddedTarget = responder
	case family == icmpFamily6 && message[0] == icmpEchoReply6 && message[1] == 0:
		key = icmpMappingKey{family: family, relayID: binary.BigEndian.Uint16(message[4:6]), sequence: binary.BigEndian.Uint16(message[6:8])}
		embeddedTarget = responder
	case icmpErrorType(family, message[0]):
		var ok bool
		key, embeddedTarget, ok = relayedEchoKeyFromEmbedded(family, message[icmpHeaderSize:])
		if !ok {
			return
		}
		isError = true
	default:
		return
	}

	mapping, ok := r.lookupMapping(key, embeddedTarget, generation)
	if !ok {
		return
	}
	var packet []byte
	if isError {
		packet, ok = buildICMPError(mapping, message, responder, r.mtu)
	} else {
		packet, ok = buildEchoReply(mapping, message, responder)
	}
	if !ok {
		return
	}
	if err := r.consumeAndInject(key, mapping, packet); err != nil {
		log.Debugf("[ICMP] inject IPv%d response from %s: %v", family, responder, err)
	}
}

func icmpErrorType(family icmpFamily, messageType byte) bool {
	if family == icmpFamily4 {
		return messageType == 3 || messageType == 11 || messageType == 12
	}
	return messageType >= 1 && messageType <= 4
}

func (r *icmpRelay) lookupMapping(key icmpMappingKey, target netip.Addr, generation uint64) (icmpMapping, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.generation != generation {
		return icmpMapping{}, false
	}
	mapping, ok := r.mappings[key]
	return mapping, ok && mapping.generation == generation && mapping.target == target
}

func (r *icmpRelay) consumeAndInject(key icmpMappingKey, mapping icmpMapping, packet []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	current, ok := r.mappings[key]
	if r.closed || !ok || current.generation != mapping.generation || r.generation != mapping.generation {
		return net.ErrClosed
	}
	delete(r.mappings, key)
	r.releaseIDLocked(mapping.family, mapping.relayID)
	if r.inject == nil {
		return fmt.Errorf("TUN response injector is unavailable")
	}
	return r.inject(packet)
}

func (r *icmpRelay) removeMapping(key icmpMappingKey, generation uint64) {
	r.mu.Lock()
	if mapping, ok := r.mappings[key]; ok && mapping.generation == generation {
		delete(r.mappings, key)
		r.releaseIDLocked(mapping.family, mapping.relayID)
	}
	r.mu.Unlock()
}

func (r *icmpRelay) releaseIDLocked(family icmpFamily, id uint16) {
	if family == icmpFamily4 {
		delete(r.ids4, id)
	} else {
		delete(r.ids6, id)
	}
}

func (r *icmpRelay) expireMappingsLocked(now time.Time) {
	cutoff := now.Add(-icmpMappingLifetime)
	for key, mapping := range r.mappings {
		if mapping.createdAt.After(cutoff) {
			continue
		}
		delete(r.mappings, key)
		r.releaseIDLocked(mapping.family, mapping.relayID)
	}
}

func (r *icmpRelay) invalidateNetwork() (int, error) {
	if r == nil || !r.enabled {
		return 0, nil
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return 0, net.ErrClosed
	}
	r.generation++
	count := len(r.mappings)
	r.mappings = make(map[icmpMappingKey]icmpMapping)
	r.ids4 = make(map[uint16]struct{})
	r.ids6 = make(map[uint16]struct{})
	conn4, conn6 := r.conn4, r.conn6
	r.conn4, r.conn6 = nil, nil
	r.mu.Unlock()
	var err error
	if conn4 != nil {
		err = errors.Join(err, conn4.Close())
	}
	if conn6 != nil {
		err = errors.Join(err, conn6.Close())
	}
	return count, err
}

func (r *icmpRelay) rebindNetwork(source4 string) (int, error) {
	if r == nil || !r.enabled {
		return 0, nil
	}
	closed, invalidateErr := r.invalidateNetwork()
	if invalidateErr != nil {
		return closed, invalidateErr
	}
	if err := r.activate(source4); err != nil {
		return closed, err
	}
	return closed, nil
}

func (r *icmpRelay) close() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	r.generation++
	conn4, conn6 := r.conn4, r.conn6
	r.conn4, r.conn6 = nil, nil
	r.mappings = make(map[icmpMappingKey]icmpMapping)
	r.ids4 = make(map[uint16]struct{})
	r.ids6 = make(map[uint16]struct{})
	r.mu.Unlock()
	var err error
	if conn4 != nil {
		err = errors.Join(err, conn4.Close())
	}
	if conn6 != nil {
		err = errors.Join(err, conn6.Close())
	}
	r.wg.Wait()
	return err
}

func addressFromNetAddr(value net.Addr) (netip.Addr, bool) {
	var ip net.IP
	switch addr := value.(type) {
	case *net.IPAddr:
		ip = addr.IP
	case *net.UDPAddr:
		ip = addr.IP
	default:
		return netip.Addr{}, false
	}
	parsed, ok := netip.AddrFromSlice(ip)
	if !ok {
		return netip.Addr{}, false
	}
	return parsed.Unmap(), true
}
