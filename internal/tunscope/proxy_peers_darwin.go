//go:build darwin

package tunscope

import (
	"bufio"
	"net/netip"
	"sort"
	"strconv"
	"strings"
)

func discoverProxyPeers(r commandRunner, port int) []string {
	out, err := r.Run("/usr/sbin/lsof", "-nP", "-iTCP:"+strconv.Itoa(port), "-sTCP:LISTEN")
	if err != nil {
		return nil
	}
	var pids []string
	scanner := bufio.NewScanner(strings.NewReader(out))
	first := true
	for scanner.Scan() {
		if first {
			first = false
			continue
		}
		fields := strings.Fields(scanner.Text())
		if len(fields) > 1 {
			if _, err := strconv.Atoi(fields[1]); err == nil {
				pids = append(pids, fields[1])
			}
		}
	}
	seen := make(map[string]struct{})
	for _, pid := range pids {
		connections, err := r.Run("/usr/sbin/lsof", "-nP", "-a", "-p", pid, "-i")
		if err != nil {
			continue
		}
		for _, match := range peerPattern.FindAllStringSubmatch(connections, -1) {
			host := match[1]
			if host == "" {
				host = match[2]
			}
			if addr, err := netip.ParseAddr(strings.TrimSuffix(host, ".")); err == nil && !addr.IsLoopback() && !addr.IsUnspecified() {
				seen[addr.String()] = struct{}{}
			}
		}
	}
	peers := make([]string, 0, len(seen))
	for peer := range seen {
		peers = append(peers, peer)
	}
	sort.Strings(peers)
	return peers
}
