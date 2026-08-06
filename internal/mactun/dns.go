//go:build darwin

package mactun

import (
	"net/netip"
	"regexp"
	"sort"
	"strings"
)

var nameserverPattern = regexp.MustCompile(`(?m)^\s*nameserver\[[0-9]+\]\s*:\s*(\S+)\s*$`)

func systemDNSServers(r commandRunner) []netip.Addr {
	servers, _ := readSystemDNSServers(r)
	return servers
}

func readSystemDNSServers(r commandRunner) ([]netip.Addr, error) {
	out, err := r.Run("/usr/sbin/scutil", "--dns")
	if err != nil {
		return nil, err
	}
	seen := make(map[netip.Addr]struct{})
	for _, match := range nameserverPattern.FindAllStringSubmatch(out, -1) {
		value := strings.TrimSpace(match[1])
		addr, err := netip.ParseAddr(value)
		if err != nil {
			continue
		}
		addr = addr.Unmap()
		if addr.IsUnspecified() || addr.IsMulticast() {
			continue
		}
		seen[addr] = struct{}{}
	}
	servers := make([]netip.Addr, 0, len(seen))
	for server := range seen {
		servers = append(servers, server)
	}
	sort.Slice(servers, func(i, j int) bool { return servers[i].String() < servers[j].String() })
	return servers, nil
}

// routedSystemDNSServers returns resolver endpoints that need an explicit
// route while TUN capture is active. Loopback resolvers are valid system DNS
// configuration, but their traffic must stay on lo0 rather than being routed
// through the TUN or a physical gateway.
func routedSystemDNSServers(servers []netip.Addr, includeIPv6 bool) []netip.Addr {
	routed := make([]netip.Addr, 0, len(servers))
	for _, server := range servers {
		if !server.IsValid() {
			continue
		}
		server = server.Unmap()
		if server.IsLoopback() || server.IsUnspecified() || server.IsMulticast() {
			continue
		}
		if server.Is6() && !includeIPv6 {
			continue
		}
		routed = append(routed, server)
	}
	return routed
}
