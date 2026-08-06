package mactun

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	M "github.com/xjasonlyu/tun2socks/v2/metadata"
	tunproxy "github.com/xjasonlyu/tun2socks/v2/proxy"
)

type proxyInfo struct {
	URL      *url.URL
	Host     string
	Port     int
	Loopback bool
}

func validateProxy(raw string) (proxyInfo, error) {
	if raw == "" {
		return proxyInfo{}, fmt.Errorf("--proxy is required")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return proxyInfo{}, fmt.Errorf("invalid proxy URL: %w", err)
	}
	if strings.ToLower(u.Scheme) != "socks5" {
		return proxyInfo{}, fmt.Errorf("proxy scheme must be socks5 (HTTP proxies cannot carry UDP/DNS safely)")
	}
	host := u.Hostname()
	portText := u.Port()
	if host == "" || portText == "" {
		return proxyInfo{}, fmt.Errorf("proxy URL must include host and port")
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return proxyInfo{}, fmt.Errorf("invalid proxy port %q", portText)
	}
	loopback := strings.EqualFold(host, "localhost")
	if ip, err := netip.ParseAddr(host); err == nil {
		loopback = ip.IsLoopback()
	}
	return proxyInfo{URL: u, Host: host, Port: port, Loopback: loopback}, nil
}

func redactProxy(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "<invalid>"
	}
	// Treat the complete userinfo component as a credential. Usernames can be
	// identifying or secret too, so neither usernames nor passwords belong in
	// logs or the persisted runtime state.
	u.User = nil
	return u.String()
}

type proxyCapabilities struct {
	UDP        bool
	UDPWarning string
}

// checkSOCKS5 performs real CONNECT and UDP relay operations. Some SOCKS5
// servers accept UDP ASSOCIATE but close the relay as soon as data is sent.
func checkSOCKS5(info proxyInfo) (proxyCapabilities, error) {
	username := ""
	password := ""
	if info.URL.User != nil {
		username = info.URL.User.Username()
		password, _ = info.URL.User.Password()
	}
	socks, err := tunproxy.NewSocks5(info.URL.Host, username, password)
	if err != nil {
		return proxyCapabilities{}, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	tcpProbe := &M.Metadata{
		Network: M.TCP,
		DstIP:   netip.MustParseAddr("1.1.1.1"),
		DstPort: 443,
	}
	conn, err := socks.DialContext(ctx, tcpProbe)
	if err != nil {
		return proxyCapabilities{}, fmt.Errorf("SOCKS5 TCP CONNECT failed: %w", err)
	}
	_ = conn.Close()

	if err := probeSOCKS5UDP(socks); err != nil {
		return proxyCapabilities{UDPWarning: err.Error()}, nil
	}
	return proxyCapabilities{UDP: true}, nil
}

func probeSOCKS5UDP(socks tunproxy.Dialer) error {
	metadata := &M.Metadata{
		Network: M.UDP,
		DstIP:   netip.MustParseAddr("1.1.1.1"),
		DstPort: 53,
	}
	packetConn, err := socks.DialUDP(metadata)
	if err != nil {
		return fmt.Errorf("UDP ASSOCIATE failed: %w", err)
	}
	defer packetConn.Close()
	_ = packetConn.SetDeadline(time.Now().Add(5 * time.Second))

	query := dnsProbeQuery()
	if _, err := packetConn.WriteTo(query, metadata.Addr()); err != nil {
		return fmt.Errorf("UDP relay write failed: %w", err)
	}
	response := make([]byte, 4096)
	n, _, err := packetConn.ReadFrom(response)
	if err != nil {
		return fmt.Errorf("UDP relay read failed: %w", err)
	}
	if n < 12 || response[0] != query[0] || response[1] != query[1] || response[2]&0x80 == 0 {
		return fmt.Errorf("UDP relay returned an invalid DNS response")
	}
	return nil
}

func dnsProbeQuery() []byte {
	// Standard recursive A query for example.com with transaction ID 0x4d54.
	return []byte{
		0x4d, 0x54, 0x01, 0x00, 0x00, 0x01, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x07, 'e', 'x', 'a', 'm', 'p', 'l', 'e',
		0x03, 'c', 'o', 'm', 0x00, 0x00, 0x01, 0x00, 0x01,
	}
}

func proxyURLWithResolvedHost(info proxyInfo) (string, error) {
	if _, err := netip.ParseAddr(info.Host); err == nil || info.Loopback {
		return info.URL.String(), nil
	}
	ips, err := net.LookupIP(info.Host)
	if err != nil {
		return "", fmt.Errorf("cannot resolve proxy host %q: %w", info.Host, err)
	}
	var selected netip.Addr
	for _, ip := range ips {
		addr, ok := netip.AddrFromSlice(ip)
		if !ok {
			continue
		}
		addr = addr.Unmap()
		if !selected.IsValid() || addr.Is4() {
			selected = addr
		}
		if addr.Is4() {
			break
		}
	}
	if !selected.IsValid() {
		return "", fmt.Errorf("proxy host %q has no usable IP address", info.Host)
	}
	resolved := *info.URL
	resolved.Host = net.JoinHostPort(selected.String(), strconv.Itoa(info.Port))
	return resolved.String(), nil
}

func resolveBypasses(values []string) ([]netip.Prefix, error) {
	seen := make(map[netip.Prefix]struct{})
	for _, raw := range values {
		value := strings.TrimSpace(raw)
		if prefix, err := netip.ParsePrefix(value); err == nil {
			prefix = prefix.Masked()
			if prefix.Bits() == 0 {
				return nil, fmt.Errorf("bypass %q is too broad", raw)
			}
			seen[prefix] = struct{}{}
			continue
		}
		if addr, err := netip.ParseAddr(value); err == nil {
			seen[netip.PrefixFrom(addr, addr.BitLen())] = struct{}{}
			continue
		}
		ips, err := net.LookupIP(value)
		if err != nil {
			return nil, fmt.Errorf("cannot resolve bypass host %q: %w", value, err)
		}
		for _, ip := range ips {
			if addr, ok := netip.AddrFromSlice(ip); ok {
				seen[netip.PrefixFrom(addr.Unmap(), addr.Unmap().BitLen())] = struct{}{}
			}
		}
	}
	result := make([]netip.Prefix, 0, len(seen))
	for prefix := range seen {
		result = append(result, prefix)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].String() < result[j].String() })
	return result, nil
}

var peerPattern = regexp.MustCompile(`->(?:\[([^]]+)\]|([^: ]+)):[0-9]+`)
