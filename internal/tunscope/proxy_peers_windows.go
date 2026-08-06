//go:build windows

package tunscope

import (
	"encoding/json"
	"net/netip"
	"sort"
	"strconv"
	"strings"
)

func discoverProxyPeers(r commandRunner, port int) []string {
	script := strings.Join([]string{
		"$ErrorActionPreference='SilentlyContinue'",
		"$pids=@(Get-NetTCPConnection -State Listen -LocalPort " + strconv.Itoa(port) + " | Select-Object -ExpandProperty OwningProcess -Unique)",
		"if ($pids.Count -eq 0) { @() | ConvertTo-Json -Compress } else { @(Get-NetTCPConnection -State Established | Where-Object { $pids -contains $_.OwningProcess } | Select-Object -ExpandProperty RemoteAddress -Unique) | ConvertTo-Json -Compress }",
	}, "; ")
	out, err := runPowerShell(r, script)
	if err != nil {
		return nil
	}
	trimmed := strings.TrimSpace(out)
	if trimmed == "" || trimmed == "null" || trimmed == "[]" {
		return nil
	}
	var values []string
	if strings.HasPrefix(trimmed, "[") {
		if json.Unmarshal([]byte(trimmed), &values) != nil {
			return nil
		}
	} else {
		var value string
		if json.Unmarshal([]byte(trimmed), &value) != nil {
			return nil
		}
		values = []string{value}
	}
	seen := make(map[string]struct{})
	for _, value := range values {
		addr, err := netip.ParseAddr(strings.TrimSpace(value))
		if err != nil || addr.IsLoopback() || addr.IsUnspecified() || addr.IsMulticast() {
			continue
		}
		seen[addr.Unmap().String()] = struct{}{}
	}
	peers := make([]string, 0, len(seen))
	for peer := range seen {
		peers = append(peers, peer)
	}
	sort.Strings(peers)
	return peers
}
