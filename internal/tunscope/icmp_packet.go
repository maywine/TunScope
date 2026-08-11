package tunscope

import (
	"bytes"
	"encoding/binary"
	"net/netip"
	"time"
)

type icmpFamily uint8

const (
	icmpFamily4 icmpFamily = 4
	icmpFamily6 icmpFamily = 6

	icmpHeaderSize       = 8
	ipv4HeaderSize       = 20
	ipv6HeaderSize       = 40
	icmpProtocol4        = 1
	icmpProtocol6        = 58
	icmpEchoRequest4     = 8
	icmpEchoReply4       = 0
	icmpEchoRequest6     = 128
	icmpEchoReply6       = 129
	defaultICMPHopLimit  = 64
	maximumICMPErrorBody = 1280
)

type icmpEchoRequest struct {
	family         icmpFamily
	source         netip.Addr
	target         netip.Addr
	originalID     uint16
	sequence       uint16
	message        []byte
	originalPacket []byte
}

type icmpMapping struct {
	family         icmpFamily
	relayID        uint16
	sequence       uint16
	originalID     uint16
	source         netip.Addr
	target         netip.Addr
	originalPacket []byte
	requestData    []byte
	createdAt      time.Time
	generation     uint64
}

type icmpMappingKey struct {
	family   icmpFamily
	relayID  uint16
	sequence uint16
}

// parseICMPEchoRequest recognizes only complete, unfragmented echo requests.
// Other ICMP messages stay in the existing gVisor stack so transport errors
// for proxied TCP and UDP retain their current behavior.
func parseICMPEchoRequest(packet []byte) (icmpEchoRequest, bool) {
	if len(packet) < 1 {
		return icmpEchoRequest{}, false
	}
	switch packet[0] >> 4 {
	case 4:
		return parseICMPEchoRequest4(packet)
	case 6:
		return parseICMPEchoRequest6(packet)
	default:
		return icmpEchoRequest{}, false
	}
}

func parseICMPEchoRequest4(packet []byte) (icmpEchoRequest, bool) {
	if len(packet) < ipv4HeaderSize || packet[9] != icmpProtocol4 {
		return icmpEchoRequest{}, false
	}
	headerLength := int(packet[0]&0x0f) * 4
	if headerLength < ipv4HeaderSize || len(packet) < headerLength+icmpHeaderSize {
		return icmpEchoRequest{}, false
	}
	totalLength := int(binary.BigEndian.Uint16(packet[2:4]))
	if totalLength < headerLength+icmpHeaderSize || totalLength > len(packet) {
		return icmpEchoRequest{}, false
	}
	if !checksumValid(packet[:headerLength]) {
		return icmpEchoRequest{}, false
	}
	// Reject both non-zero fragment offsets and the More Fragments flag. ID
	// rewriting is not safe unless the entire ICMP message is present.
	if binary.BigEndian.Uint16(packet[6:8])&0x3fff != 0 {
		return icmpEchoRequest{}, false
	}
	message := packet[headerLength:totalLength]
	if message[0] != icmpEchoRequest4 || message[1] != 0 || !checksumValid(message) {
		return icmpEchoRequest{}, false
	}
	var sourceBytes, targetBytes [4]byte
	copy(sourceBytes[:], packet[12:16])
	copy(targetBytes[:], packet[16:20])
	source, target := netip.AddrFrom4(sourceBytes), netip.AddrFrom4(targetBytes)
	if !usableICMPTarget(target) {
		return icmpEchoRequest{}, false
	}
	return icmpEchoRequest{
		family:         icmpFamily4,
		source:         source,
		target:         target,
		originalID:     binary.BigEndian.Uint16(message[4:6]),
		sequence:       binary.BigEndian.Uint16(message[6:8]),
		message:        append([]byte(nil), message...),
		originalPacket: append([]byte(nil), packet[:totalLength]...),
	}, true
}

func parseICMPEchoRequest6(packet []byte) (icmpEchoRequest, bool) {
	if len(packet) < ipv6HeaderSize {
		return icmpEchoRequest{}, false
	}
	totalLength := ipv6HeaderSize + int(binary.BigEndian.Uint16(packet[4:6]))
	if totalLength < ipv6HeaderSize+icmpHeaderSize || totalLength > len(packet) {
		return icmpEchoRequest{}, false
	}
	icmpOffset, ok := ipv6ICMPOffset(packet[:totalLength], false)
	if !ok || totalLength < icmpOffset+icmpHeaderSize {
		return icmpEchoRequest{}, false
	}
	message := packet[icmpOffset:totalLength]
	if message[0] != icmpEchoRequest6 || message[1] != 0 {
		return icmpEchoRequest{}, false
	}
	var sourceBytes, targetBytes [16]byte
	copy(sourceBytes[:], packet[8:24])
	copy(targetBytes[:], packet[24:40])
	source, target := netip.AddrFrom16(sourceBytes), netip.AddrFrom16(targetBytes)
	if !usableICMPTarget(target) || !icmpv6ChecksumValid(source, target, message) {
		return icmpEchoRequest{}, false
	}
	return icmpEchoRequest{
		family:         icmpFamily6,
		source:         source,
		target:         target,
		originalID:     binary.BigEndian.Uint16(message[4:6]),
		sequence:       binary.BigEndian.Uint16(message[6:8]),
		message:        append([]byte(nil), message...),
		originalPacket: append([]byte(nil), packet[:totalLength]...),
	}, true
}

func usableICMPTarget(target netip.Addr) bool {
	if !target.IsValid() || target.IsUnspecified() || target.IsLoopback() || target.IsMulticast() {
		return false
	}
	return !target.Is4() || target != netip.AddrFrom4([4]byte{255, 255, 255, 255})
}

// ipv6ICMPOffset walks the extension-header chain. Fragmented ICMP is left to
// the normal stack; a first fragment is still incomplete when M is set.
func ipv6ICMPOffset(packet []byte, allowTruncatedPayload bool) (int, bool) {
	if len(packet) < ipv6HeaderSize || packet[0]>>4 != 6 {
		return 0, false
	}
	next := packet[6]
	offset := ipv6HeaderSize
	for next != icmpProtocol6 {
		switch next {
		case 0, 43, 60: // Hop-by-Hop, Routing, Destination Options.
			if len(packet) < offset+2 {
				return 0, false
			}
			headerLength := (int(packet[offset+1]) + 1) * 8
			if headerLength < 8 || len(packet) < offset+headerLength {
				return 0, false
			}
			next = packet[offset]
			offset += headerLength
		case 44: // Fragment.
			if len(packet) < offset+8 {
				return 0, false
			}
			fragment := binary.BigEndian.Uint16(packet[offset+2 : offset+4])
			if fragment&0xfff9 != 0 { // offset or M flag; reserved bits ignored.
				return 0, false
			}
			next = packet[offset]
			offset += 8
		case 51: // Authentication Header.
			if len(packet) < offset+2 {
				return 0, false
			}
			headerLength := (int(packet[offset+1]) + 2) * 4
			if headerLength < 8 || len(packet) < offset+headerLength {
				return 0, false
			}
			next = packet[offset]
			offset += headerLength
		default:
			return 0, false
		}
	}
	if offset+icmpHeaderSize > len(packet) && !allowTruncatedPayload {
		return 0, false
	}
	return offset, offset+icmpHeaderSize <= len(packet)
}

func relayedEchoKeyFromEmbedded(family icmpFamily, packet []byte) (icmpMappingKey, netip.Addr, bool) {
	switch family {
	case icmpFamily4:
		if len(packet) < ipv4HeaderSize || packet[0]>>4 != 4 || packet[9] != icmpProtocol4 {
			return icmpMappingKey{}, netip.Addr{}, false
		}
		headerLength := int(packet[0]&0x0f) * 4
		if headerLength < ipv4HeaderSize || len(packet) < headerLength+icmpHeaderSize {
			return icmpMappingKey{}, netip.Addr{}, false
		}
		message := packet[headerLength:]
		if message[0] != icmpEchoRequest4 {
			return icmpMappingKey{}, netip.Addr{}, false
		}
		var targetBytes [4]byte
		copy(targetBytes[:], packet[16:20])
		return icmpMappingKey{
			family: family, relayID: binary.BigEndian.Uint16(message[4:6]), sequence: binary.BigEndian.Uint16(message[6:8]),
		}, netip.AddrFrom4(targetBytes), true
	case icmpFamily6:
		if len(packet) < ipv6HeaderSize || packet[0]>>4 != 6 {
			return icmpMappingKey{}, netip.Addr{}, false
		}
		offset, ok := ipv6ICMPOffset(packet, true)
		if !ok || len(packet) < offset+icmpHeaderSize || packet[offset] != icmpEchoRequest6 {
			return icmpMappingKey{}, netip.Addr{}, false
		}
		var targetBytes [16]byte
		copy(targetBytes[:], packet[24:40])
		return icmpMappingKey{
			family: family, relayID: binary.BigEndian.Uint16(packet[offset+4 : offset+6]), sequence: binary.BigEndian.Uint16(packet[offset+6 : offset+8]),
		}, netip.AddrFrom16(targetBytes), true
	default:
		return icmpMappingKey{}, netip.Addr{}, false
	}
}

func relayedEchoRequest(message []byte, family icmpFamily, relayID uint16) []byte {
	request := append([]byte(nil), message...)
	request[2], request[3] = 0, 0
	binary.BigEndian.PutUint16(request[4:6], relayID)
	if family == icmpFamily4 {
		binary.BigEndian.PutUint16(request[2:4], internetChecksum(request))
	}
	// An ICMPv6 raw socket calculates the mandatory pseudo-header checksum.
	return request
}

func buildEchoReply(mapping icmpMapping, rawReply []byte, responder netip.Addr) ([]byte, bool) {
	if len(rawReply) < icmpHeaderSize || responder != mapping.target {
		return nil, false
	}
	wantType := byte(icmpEchoReply4)
	if mapping.family == icmpFamily6 {
		wantType = icmpEchoReply6
	}
	if rawReply[0] != wantType || rawReply[1] != 0 ||
		binary.BigEndian.Uint16(rawReply[4:6]) != mapping.relayID ||
		binary.BigEndian.Uint16(rawReply[6:8]) != mapping.sequence ||
		!bytes.Equal(rawReply[8:], mapping.requestData) {
		return nil, false
	}
	message := append([]byte(nil), rawReply...)
	message[2], message[3] = 0, 0
	binary.BigEndian.PutUint16(message[4:6], mapping.originalID)
	if mapping.family == icmpFamily4 {
		binary.BigEndian.PutUint16(message[2:4], internetChecksum(message))
		return buildIPv4Packet(mapping, responder, message), true
	}
	binary.BigEndian.PutUint16(message[2:4], icmpv6Checksum(responder, mapping.source, message))
	return buildIPv6Packet(mapping, responder, message), true
}

func buildICMPError(mapping icmpMapping, rawError []byte, responder netip.Addr, mtu int) ([]byte, bool) {
	if len(rawError) < icmpHeaderSize || !responder.IsValid() || responder.BitLen() != mapping.source.BitLen() {
		return nil, false
	}
	message := append([]byte(nil), rawError[:icmpHeaderSize]...)
	message[2], message[3] = 0, 0
	headerSize := ipv4HeaderSize
	if mapping.family == icmpFamily6 {
		headerSize = ipv6HeaderSize
	}
	limit := mtu - headerSize - icmpHeaderSize
	if limit <= 0 {
		return nil, false
	}
	if limit > maximumICMPErrorBody {
		limit = maximumICMPErrorBody
	}
	inner := mapping.originalPacket
	if len(inner) > limit {
		inner = inner[:limit]
	}
	message = append(message, inner...)
	if mapping.family == icmpFamily4 {
		binary.BigEndian.PutUint16(message[2:4], internetChecksum(message))
		return buildIPv4Packet(mapping, responder, message), true
	}
	binary.BigEndian.PutUint16(message[2:4], icmpv6Checksum(responder, mapping.source, message))
	return buildIPv6Packet(mapping, responder, message), true
}

func buildIPv4Packet(mapping icmpMapping, responder netip.Addr, message []byte) []byte {
	packet := make([]byte, ipv4HeaderSize+len(message))
	packet[0] = 0x45
	if len(mapping.originalPacket) >= ipv4HeaderSize && mapping.originalPacket[0]>>4 == 4 {
		packet[1] = mapping.originalPacket[1]
	}
	binary.BigEndian.PutUint16(packet[2:4], uint16(len(packet)))
	binary.BigEndian.PutUint16(packet[4:6], mapping.sequence)
	packet[8] = defaultICMPHopLimit
	packet[9] = icmpProtocol4
	copy(packet[12:16], responder.AsSlice())
	copy(packet[16:20], mapping.source.AsSlice())
	binary.BigEndian.PutUint16(packet[10:12], internetChecksum(packet[:ipv4HeaderSize]))
	copy(packet[ipv4HeaderSize:], message)
	return packet
}

func buildIPv6Packet(mapping icmpMapping, responder netip.Addr, message []byte) []byte {
	packet := make([]byte, ipv6HeaderSize+len(message))
	packet[0] = 0x60
	if len(mapping.originalPacket) >= ipv6HeaderSize && mapping.originalPacket[0]>>4 == 6 {
		copy(packet[:4], mapping.originalPacket[:4])
		packet[0] = 0x60 | packet[0]&0x0f
	}
	binary.BigEndian.PutUint16(packet[4:6], uint16(len(message)))
	packet[6] = icmpProtocol6
	packet[7] = defaultICMPHopLimit
	copy(packet[8:24], responder.AsSlice())
	copy(packet[24:40], mapping.source.AsSlice())
	copy(packet[ipv6HeaderSize:], message)
	return packet
}

func checksumValid(parts ...[]byte) bool {
	return checksumSum(parts...) == 0xffff
}

func internetChecksum(parts ...[]byte) uint16 {
	return ^checksumSum(parts...)
}

func checksumSum(parts ...[]byte) uint16 {
	var sum uint32
	var odd bool
	var trailing byte
	for _, part := range parts {
		index := 0
		if odd && len(part) > 0 {
			sum += uint32(uint16(trailing)<<8 | uint16(part[0]))
			index = 1
			odd = false
		}
		for ; index+1 < len(part); index += 2 {
			sum += uint32(binary.BigEndian.Uint16(part[index : index+2]))
		}
		if index < len(part) {
			trailing = part[index]
			odd = true
		}
	}
	if odd {
		sum += uint32(uint16(trailing) << 8)
	}
	for sum>>16 != 0 {
		sum = sum&0xffff + sum>>16
	}
	return uint16(sum)
}

func icmpv6Checksum(source, target netip.Addr, message []byte) uint16 {
	pseudo := ipv6PseudoHeader(source, target, len(message))
	return internetChecksum(pseudo, message)
}

func icmpv6ChecksumValid(source, target netip.Addr, message []byte) bool {
	pseudo := ipv6PseudoHeader(source, target, len(message))
	return checksumValid(pseudo, message)
}

func ipv6PseudoHeader(source, target netip.Addr, length int) []byte {
	pseudo := make([]byte, 40)
	copy(pseudo[:16], source.AsSlice())
	copy(pseudo[16:32], target.AsSlice())
	binary.BigEndian.PutUint32(pseudo[32:36], uint32(length))
	pseudo[39] = icmpProtocol6
	return pseudo
}
