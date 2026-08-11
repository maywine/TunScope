package tunscope

import (
	"encoding/binary"
	"net/netip"
	"testing"
	"time"
)

func TestParseAndRewriteIPv4Echo(t *testing.T) {
	source := netip.MustParseAddr("198.18.0.1")
	target := netip.MustParseAddr("8.8.8.8")
	requestPacket := testIPv4EchoPacket(source, target, 0x1234, 7, []byte("tunscope-icmp4"))
	request, ok := parseICMPEchoRequest(requestPacket)
	if !ok {
		t.Fatal("valid IPv4 echo request was not recognized")
	}
	if request.family != icmpFamily4 || request.source != source || request.target != target || request.originalID != 0x1234 || request.sequence != 7 {
		t.Fatalf("unexpected request: %+v", request)
	}

	relayed := relayedEchoRequest(request.message, request.family, 0xabcd)
	if got := binary.BigEndian.Uint16(relayed[4:6]); got != 0xabcd {
		t.Fatalf("relay ID = %#x, want 0xabcd", got)
	}
	if !checksumValid(relayed) {
		t.Fatal("relayed IPv4 ICMP checksum is invalid")
	}
	relayed[0] = icmpEchoReply4
	relayed[2], relayed[3] = 0, 0
	binary.BigEndian.PutUint16(relayed[2:4], internetChecksum(relayed))
	mapping := icmpMapping{
		family: icmpFamily4, relayID: 0xabcd, sequence: 7, originalID: 0x1234,
		source: source, target: target, originalPacket: request.originalPacket,
		requestData: append([]byte(nil), request.message[8:]...), createdAt: time.Now(),
	}
	reply, ok := buildEchoReply(mapping, relayed, target)
	if !ok {
		t.Fatal("valid IPv4 relay reply was rejected")
	}
	if reply[0]>>4 != 4 || netip.AddrFrom4([4]byte(reply[12:16])) != target || netip.AddrFrom4([4]byte(reply[16:20])) != source {
		t.Fatalf("unexpected rewritten IPv4 addresses: %v", reply[:20])
	}
	if !checksumValid(reply[:20]) || !checksumValid(reply[20:]) {
		t.Fatal("rewritten IPv4 packet has an invalid checksum")
	}
	if got := binary.BigEndian.Uint16(reply[24:26]); got != 0x1234 {
		t.Fatalf("restored echo ID = %#x, want 0x1234", got)
	}
}

func TestParseAndRewriteIPv6Echo(t *testing.T) {
	source := netip.MustParseAddr("fd7a:6d61:6374:756e::1")
	target := netip.MustParseAddr("2606:4700:4700::1111")
	requestPacket := testIPv6EchoPacket(source, target, 0x4321, 9, []byte("tunscope-icmp6"))
	request, ok := parseICMPEchoRequest(requestPacket)
	if !ok {
		t.Fatal("valid IPv6 echo request was not recognized")
	}
	relayed := relayedEchoRequest(request.message, request.family, 0xbcde)
	if got := binary.BigEndian.Uint16(relayed[2:4]); got != 0 {
		t.Fatalf("raw IPv6 request checksum = %#x, want kernel-computed zero", got)
	}
	relayed[0] = icmpEchoReply6
	mapping := icmpMapping{
		family: icmpFamily6, relayID: 0xbcde, sequence: 9, originalID: 0x4321,
		source: source, target: target, originalPacket: request.originalPacket,
		requestData: append([]byte(nil), request.message[8:]...), createdAt: time.Now(),
	}
	reply, ok := buildEchoReply(mapping, relayed, target)
	if !ok {
		t.Fatal("valid IPv6 relay reply was rejected")
	}
	if reply[0]>>4 != 6 {
		t.Fatalf("reply version = %d, want 6", reply[0]>>4)
	}
	var gotSource, gotTarget [16]byte
	copy(gotSource[:], reply[8:24])
	copy(gotTarget[:], reply[24:40])
	if netip.AddrFrom16(gotSource) != target || netip.AddrFrom16(gotTarget) != source {
		t.Fatalf("unexpected rewritten IPv6 addresses: %s -> %s", netip.AddrFrom16(gotSource), netip.AddrFrom16(gotTarget))
	}
	if !icmpv6ChecksumValid(target, source, reply[40:]) {
		t.Fatal("rewritten IPv6 ICMP checksum is invalid")
	}
	if got := binary.BigEndian.Uint16(reply[44:46]); got != 0x4321 {
		t.Fatalf("restored echo ID = %#x, want 0x4321", got)
	}
}

func TestBuildIPv4TimeExceededRestoresOriginalRequest(t *testing.T) {
	virtualSource := netip.MustParseAddr("198.18.0.1")
	target := netip.MustParseAddr("203.0.113.9")
	physicalSource := netip.MustParseAddr("192.0.2.37")
	original := testIPv4EchoPacket(virtualSource, target, 0x1111, 4, []byte("payload"))
	request, ok := parseICMPEchoRequest(original)
	if !ok {
		t.Fatal("request parse failed")
	}
	relayedMessage := relayedEchoRequest(request.message, icmpFamily4, 0x2222)
	relayedPacket := testIPv4Packet(physicalSource, target, relayedMessage)
	rawError := make([]byte, 8+28)
	rawError[0] = 11
	copy(rawError[8:], relayedPacket[:28])
	binary.BigEndian.PutUint16(rawError[2:4], internetChecksum(rawError))
	key, embeddedTarget, ok := relayedEchoKeyFromEmbedded(icmpFamily4, rawError[8:])
	if !ok || key.relayID != 0x2222 || key.sequence != 4 || embeddedTarget != target {
		t.Fatalf("unexpected embedded key: %+v target=%s ok=%v", key, embeddedTarget, ok)
	}
	mapping := icmpMapping{
		family: icmpFamily4, relayID: key.relayID, sequence: key.sequence, originalID: 0x1111,
		source: virtualSource, target: target, originalPacket: original,
	}
	responder := netip.MustParseAddr("192.0.2.1")
	packet, ok := buildICMPError(mapping, rawError, responder, 1500)
	if !ok || packet[20] != 11 {
		t.Fatalf("failed to build time-exceeded response: ok=%v", ok)
	}
	if !checksumValid(packet[20:]) {
		t.Fatal("rewritten ICMP error checksum is invalid")
	}
	innerOffset := 20 + 8
	if got := binary.BigEndian.Uint16(packet[innerOffset+20+4 : innerOffset+20+6]); got != 0x1111 {
		t.Fatalf("embedded original ID = %#x, want 0x1111", got)
	}
}

func TestBuildIPv6PacketTooBigRestoresOriginalRequest(t *testing.T) {
	virtualSource := netip.MustParseAddr("fd7a:6d61:6374:756e::1")
	target := netip.MustParseAddr("2001:db8:2::9")
	physicalSource := netip.MustParseAddr("2001:db8:1::37")
	original := testIPv6EchoPacket(virtualSource, target, 0x5151, 6, []byte("payload-v6"))
	request, ok := parseICMPEchoRequest(original)
	if !ok {
		t.Fatal("request parse failed")
	}
	relayedMessage := relayedEchoRequest(request.message, icmpFamily6, 0x6262)
	binary.BigEndian.PutUint16(relayedMessage[2:4], icmpv6Checksum(physicalSource, target, relayedMessage))
	relayedPacket := testIPv6Packet(physicalSource, target, relayedMessage)
	rawError := make([]byte, 8+len(relayedPacket))
	rawError[0] = 2
	binary.BigEndian.PutUint32(rawError[4:8], 1280)
	copy(rawError[8:], relayedPacket)
	key, embeddedTarget, ok := relayedEchoKeyFromEmbedded(icmpFamily6, rawError[8:])
	if !ok || key.relayID != 0x6262 || key.sequence != 6 || embeddedTarget != target {
		t.Fatalf("unexpected embedded key: %+v target=%s ok=%v", key, embeddedTarget, ok)
	}
	mapping := icmpMapping{
		family: icmpFamily6, relayID: key.relayID, sequence: key.sequence, originalID: 0x5151,
		source: virtualSource, target: target, originalPacket: original,
	}
	responder := netip.MustParseAddr("2001:db8:1::1")
	packet, ok := buildICMPError(mapping, rawError, responder, 1280)
	if !ok || packet[40] != 2 {
		t.Fatalf("failed to build packet-too-big response: ok=%v", ok)
	}
	if !icmpv6ChecksumValid(responder, virtualSource, packet[40:]) {
		t.Fatal("rewritten ICMPv6 error checksum is invalid")
	}
	innerOffset := 40 + 8
	if got := binary.BigEndian.Uint16(packet[innerOffset+40+4 : innerOffset+40+6]); got != 0x5151 {
		t.Fatalf("embedded original ID = %#x, want 0x5151", got)
	}
}

func TestRejectMalformedAndFragmentedEcho(t *testing.T) {
	packet := testIPv4EchoPacket(netip.MustParseAddr("198.18.0.1"), netip.MustParseAddr("1.1.1.1"), 1, 1, nil)
	packet[len(packet)-1] ^= 0xff
	if _, ok := parseICMPEchoRequest(packet); ok {
		t.Fatal("request with invalid checksum was accepted")
	}
	packet = testIPv4EchoPacket(netip.MustParseAddr("198.18.0.1"), netip.MustParseAddr("1.1.1.1"), 1, 1, nil)
	binary.BigEndian.PutUint16(packet[6:8], 0x2000)
	if _, ok := parseICMPEchoRequest(packet); ok {
		t.Fatal("fragmented request was accepted")
	}
	packet = testIPv4EchoPacket(netip.MustParseAddr("198.18.0.1"), netip.MustParseAddr("1.1.1.1"), 1, 1, nil)
	packet[10] ^= 0xff
	if _, ok := parseICMPEchoRequest(packet); ok {
		t.Fatal("request with an invalid IPv4 header checksum was accepted")
	}
	for _, target := range []string{"127.0.0.1", "255.255.255.255", "224.0.0.1", "::1", "ff02::1"} {
		if usableICMPTarget(netip.MustParseAddr(target)) {
			t.Fatalf("unsafe ICMP target %s was accepted", target)
		}
	}
}

func testIPv4EchoPacket(source, target netip.Addr, id, sequence uint16, data []byte) []byte {
	message := make([]byte, 8+len(data))
	message[0] = icmpEchoRequest4
	binary.BigEndian.PutUint16(message[4:6], id)
	binary.BigEndian.PutUint16(message[6:8], sequence)
	copy(message[8:], data)
	binary.BigEndian.PutUint16(message[2:4], internetChecksum(message))
	return testIPv4Packet(source, target, message)
}

func testIPv4Packet(source, target netip.Addr, payload []byte) []byte {
	packet := make([]byte, 20+len(payload))
	packet[0], packet[8], packet[9] = 0x45, 64, icmpProtocol4
	binary.BigEndian.PutUint16(packet[2:4], uint16(len(packet)))
	copy(packet[12:16], source.AsSlice())
	copy(packet[16:20], target.AsSlice())
	binary.BigEndian.PutUint16(packet[10:12], internetChecksum(packet[:20]))
	copy(packet[20:], payload)
	return packet
}

func testIPv6EchoPacket(source, target netip.Addr, id, sequence uint16, data []byte) []byte {
	message := make([]byte, 8+len(data))
	message[0] = icmpEchoRequest6
	binary.BigEndian.PutUint16(message[4:6], id)
	binary.BigEndian.PutUint16(message[6:8], sequence)
	copy(message[8:], data)
	binary.BigEndian.PutUint16(message[2:4], icmpv6Checksum(source, target, message))
	return testIPv6Packet(source, target, message)
}

func testIPv6Packet(source, target netip.Addr, payload []byte) []byte {
	packet := make([]byte, 40+len(payload))
	packet[0], packet[6], packet[7] = 0x60, icmpProtocol6, 64
	binary.BigEndian.PutUint16(packet[4:6], uint16(len(payload)))
	copy(packet[8:24], source.AsSlice())
	copy(packet[24:40], target.AsSlice())
	copy(packet[40:], payload)
	return packet
}
