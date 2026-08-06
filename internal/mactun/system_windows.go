//go:build windows

package mactun

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

type commandRunner interface {
	Run(name string, args ...string) (string, error)
}

type execRunner struct{}

func (execRunner) Run(name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return out.String(), fmt.Errorf("%s timed out", name)
	}
	if err != nil {
		message := strings.TrimSpace(out.String())
		if message == "" {
			return out.String(), fmt.Errorf("%s: %w", name, err)
		}
		return out.String(), fmt.Errorf("%s: %w: %s", name, err, message)
	}
	return out.String(), nil
}

type windowsPhysicalNetwork struct {
	InterfaceIndex  int      `json:"InterfaceIndex"`
	InterfaceAlias  string   `json:"InterfaceAlias"`
	Gateway4        string   `json:"Gateway4"`
	Source4         string   `json:"Source4"`
	Interface6Index int      `json:"Interface6Index"`
	Interface6Alias string   `json:"Interface6Alias"`
	Gateway6        string   `json:"Gateway6"`
	Source6         string   `json:"Source6"`
	DNSServers      []string `json:"DNSServers"`
}

func runPowerShell(r commandRunner, script string) (string, error) {
	return r.Run(
		"powershell.exe",
		"-NoLogo",
		"-NoProfile",
		"-NonInteractive",
		"-ExecutionPolicy", "Bypass",
		"-Command", script,
	)
}

func readWindowsPhysicalNetwork(r commandRunner, requestedInterface string, includeIPv6 bool) (windowsPhysicalNetwork, error) {
	wantedIndex := 0
	if requestedInterface != "" {
		iface, err := net.InterfaceByName(requestedInterface)
		if err != nil {
			return windowsPhysicalNetwork{}, fmt.Errorf("find physical interface %q: %w", requestedInterface, err)
		}
		wantedIndex = iface.Index
	}
	wanted := strconv.Itoa(wantedIndex)
	include6 := "$false"
	if includeIPv6 {
		include6 = "$true"
	}
	script := strings.Join([]string{
		"$ErrorActionPreference='Stop'",
		"$wanted=" + wanted,
		"$routes=@(Get-NetRoute -PolicyStore ActiveStore -AddressFamily IPv4 -DestinationPrefix '0.0.0.0/0' | Where-Object { $_.NextHop -ne '0.0.0.0' -and ($wanted -eq 0 -or $_.InterfaceIndex -eq $wanted) } | Sort-Object @{Expression={ [int]$_.RouteMetric + [int]$_.InterfaceMetric }})",
		"$r=$routes | Select-Object -First 1",
		"if ($null -eq $r) { throw 'no physical IPv4 default route found' }",
		"$ip=Get-NetIPAddress -InterfaceIndex $r.InterfaceIndex -AddressFamily IPv4 -ErrorAction Stop | Where-Object { $_.IPAddress -ne '0.0.0.0' -and $_.PrefixOrigin -ne 'WellKnown' -and $_.AddressState -eq 'Preferred' -and -not $_.SkipAsSource } | Select-Object -First 1",
		"if ($null -eq $ip) { throw 'physical interface has no usable IPv4 address' }",
		"$dns=@((Get-DnsClientServerAddress -InterfaceIndex $r.InterfaceIndex -AddressFamily IPv4 -ErrorAction SilentlyContinue).ServerAddresses)",
		"$r6=$null; $ip6=$null",
		"if (" + include6 + ") { $r6=Get-NetRoute -PolicyStore ActiveStore -AddressFamily IPv6 -DestinationPrefix '::/0' -ErrorAction SilentlyContinue | Where-Object { $_.NextHop -ne '::' -and ($wanted -eq 0 -or $_.InterfaceIndex -eq $wanted) } | Sort-Object @{Expression={ [int]$_.RouteMetric + [int]$_.InterfaceMetric }} | Select-Object -First 1; if ($null -ne $r6) { $ip6=Get-NetIPAddress -InterfaceIndex $r6.InterfaceIndex -AddressFamily IPv6 -ErrorAction SilentlyContinue | Where-Object { $_.IPAddress -notlike 'fe80:*' -and $_.IPAddress -ne '::1' -and $_.AddressState -eq 'Preferred' -and -not $_.SkipAsSource } | Select-Object -First 1; $dns += @((Get-DnsClientServerAddress -InterfaceIndex $r6.InterfaceIndex -AddressFamily IPv6 -ErrorAction SilentlyContinue).ServerAddresses) } }",
		"[pscustomobject]@{InterfaceIndex=[int]$r.InterfaceIndex;InterfaceAlias=[string]$r.InterfaceAlias;Gateway4=[string]$r.NextHop;Source4=[string]$ip.IPAddress;Interface6Index=$(if ($null -ne $r6) {[int]$r6.InterfaceIndex} else {0});Interface6Alias=$(if ($null -ne $r6) {[string]$r6.InterfaceAlias} else {''});Gateway6=$(if ($null -ne $r6) {[string]$r6.NextHop} else {''});Source6=$(if ($null -ne $ip6) {[string]$ip6.IPAddress} else {''});DNSServers=@($dns | Where-Object { $_ } | Select-Object -Unique)} | ConvertTo-Json -Compress",
	}, "; ")
	out, err := runPowerShell(r, script)
	if err != nil {
		return windowsPhysicalNetwork{}, fmt.Errorf("detect Windows physical network: %w", err)
	}
	var snapshot windowsPhysicalNetwork
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &snapshot); err != nil {
		return windowsPhysicalNetwork{}, fmt.Errorf("decode Windows physical network: %w", err)
	}
	if snapshot.InterfaceIndex <= 0 || snapshot.InterfaceAlias == "" {
		return windowsPhysicalNetwork{}, fmt.Errorf("Windows physical network snapshot is incomplete")
	}
	if gateway, err := netip.ParseAddr(snapshot.Gateway4); err != nil || !gateway.Is4() || gateway.IsUnspecified() || gateway.IsLoopback() || gateway.IsLinkLocalUnicast() {
		return windowsPhysicalNetwork{}, fmt.Errorf("Windows physical gateway is invalid: %q", snapshot.Gateway4)
	}
	if source, err := netip.ParseAddr(snapshot.Source4); err != nil || !source.Is4() || source.IsUnspecified() || source.IsLoopback() || source.IsLinkLocalUnicast() {
		return windowsPhysicalNetwork{}, fmt.Errorf("Windows physical IPv4 address is invalid: %q", snapshot.Source4)
	}
	if snapshot.Interface6Index > 0 {
		gateway6, gatewayErr := netip.ParseAddr(snapshot.Gateway6)
		if gatewayErr != nil || !gateway6.Is6() || gateway6.IsUnspecified() {
			return windowsPhysicalNetwork{}, fmt.Errorf("Windows physical IPv6 gateway is invalid: %q", snapshot.Gateway6)
		}
		if snapshot.Source6 != "" {
			source6, sourceErr := netip.ParseAddr(snapshot.Source6)
			if sourceErr != nil || !source6.Is6() || source6.IsUnspecified() || source6.IsLoopback() || source6.IsLinkLocalUnicast() {
				return windowsPhysicalNetwork{}, fmt.Errorf("Windows physical IPv6 address is invalid: %q", snapshot.Source6)
			}
		}
	}
	return snapshot, nil
}

func configureWindowsTUN(r commandRunner, interfaceIndex, mtu int, includeIPv6 bool) error {
	if interfaceIndex <= 0 {
		return fmt.Errorf("invalid Wintun interface index %d", interfaceIndex)
	}
	include6 := "$false"
	if includeIPv6 {
		include6 = "$true"
	}
	script := strings.Join([]string{
		"$ErrorActionPreference='Stop'",
		"$idx=" + strconv.Itoa(interfaceIndex),
		"$existing=Get-NetIPAddress -InterfaceIndex $idx -AddressFamily IPv4 -IPAddress '198.18.0.1' -ErrorAction SilentlyContinue",
		"if ($null -eq $existing) { New-NetIPAddress -InterfaceIndex $idx -IPAddress '198.18.0.1' -PrefixLength 15 -AddressFamily IPv4 -PolicyStore ActiveStore | Out-Null }",
		"Set-NetIPInterface -InterfaceIndex $idx -AddressFamily IPv4 -NlMtuBytes " + strconv.Itoa(mtu) + " -AutomaticMetric Disabled -InterfaceMetric 1",
		"if (" + include6 + ") { $existing6=Get-NetIPAddress -InterfaceIndex $idx -AddressFamily IPv6 -IPAddress 'fd7a:6d61:6374:756e::1' -ErrorAction SilentlyContinue; if ($null -eq $existing6) { New-NetIPAddress -InterfaceIndex $idx -IPAddress 'fd7a:6d61:6374:756e::1' -PrefixLength 128 -AddressFamily IPv6 -PolicyStore ActiveStore | Out-Null }; Set-NetIPInterface -InterfaceIndex $idx -AddressFamily IPv6 -NlMtuBytes " + strconv.Itoa(mtu) + " -AutomaticMetric Disabled -InterfaceMetric 1 }",
	}, "; ")
	if _, err := runPowerShell(r, script); err != nil {
		return fmt.Errorf("configure Wintun interface: %w", err)
	}
	return nil
}

func windowsRoutePrefix(route Route) (string, error) {
	target := strings.TrimSpace(route.Target)
	if strings.Contains(target, "/") {
		prefix, err := netip.ParsePrefix(target)
		if err != nil {
			return "", err
		}
		return prefix.Masked().String(), nil
	}
	addr, err := netip.ParseAddr(target)
	if err != nil {
		return "", err
	}
	return netip.PrefixFrom(addr, addr.BitLen()).String(), nil
}

// addWindowsRoute returns true only when MacTun created the route. An exact
// pre-existing route is left untouched and is therefore never removed during
// cleanup.
func addWindowsRoute(r commandRunner, route Route) (bool, error) {
	prefix, index, nextHop, err := windowsRouteParts(route)
	if err != nil {
		return false, err
	}
	// All interpolated values have already been parsed as numbers, prefixes, or
	// IP addresses; no user-controlled PowerShell syntax reaches this command.
	script := strings.Join([]string{
		"$ErrorActionPreference='Stop'",
		"$existing=@(Get-NetRoute -PolicyStore ActiveStore -DestinationPrefix '" + prefix + "' -InterfaceIndex " + strconv.Itoa(index) + " -ErrorAction SilentlyContinue | Where-Object { $_.NextHop -eq '" + nextHop + "' })",
		"if ($existing.Count -gt 0) { 'exists' } else { New-NetRoute -PolicyStore ActiveStore -DestinationPrefix '" + prefix + "' -InterfaceIndex " + strconv.Itoa(index) + " -NextHop '" + nextHop + "' -RouteMetric 0 | Out-Null; 'added' }",
	}, "; ")
	out, err := runPowerShell(r, script)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) == "added", nil
}

func deleteWindowsRoute(r commandRunner, route Route) error {
	prefix, index, nextHop, err := windowsRouteParts(route)
	if err != nil {
		return err
	}
	script := strings.Join([]string{
		"$ErrorActionPreference='Stop'",
		"$routes=@(Get-NetRoute -PolicyStore ActiveStore -DestinationPrefix '" + prefix + "' -InterfaceIndex " + strconv.Itoa(index) + " -ErrorAction SilentlyContinue | Where-Object { $_.NextHop -eq '" + nextHop + "' })",
		"foreach ($route in $routes) { Remove-NetRoute -InputObject $route -Confirm:$false -ErrorAction Stop }",
	}, "; ")
	_, err = runPowerShell(r, script)
	return err
}

func windowsRouteParts(route Route) (prefix string, index int, nextHop string, err error) {
	prefix, err = windowsRoutePrefix(route)
	if err != nil {
		return "", 0, "", fmt.Errorf("invalid route target %q: %w", route.Target, err)
	}
	index, err = strconv.Atoi(route.Interface)
	if err != nil || index <= 0 {
		return "", 0, "", fmt.Errorf("invalid route interface index %q", route.Interface)
	}
	nextHop = route.Gateway
	if nextHop == "" {
		if strings.Contains(prefix, ":") {
			nextHop = "::"
		} else {
			nextHop = "0.0.0.0"
		}
	}
	hop, parseErr := netip.ParseAddr(nextHop)
	if parseErr != nil || hop.IsUnspecified() != (nextHop == "0.0.0.0" || nextHop == "::") {
		return "", 0, "", fmt.Errorf("invalid route next hop %q", nextHop)
	}
	if (strings.Contains(prefix, ":")) != hop.Is6() {
		return "", 0, "", fmt.Errorf("route target %q and next hop %q use different address families", prefix, nextHop)
	}
	return prefix, index, nextHop, nil
}

// deleteWindowsRoutes removes a cleanup batch in one PowerShell process while
// retaining the indexes of individual failures. Missing routes are successful,
// which makes stale recovery idempotent after a partial previous cleanup.
func deleteWindowsRoutes(r commandRunner, routes []Route) ([]Route, error) {
	if len(routes) == 0 {
		return nil, nil
	}
	commands := []string{"$ErrorActionPreference='Stop'", "$failed=@()"}
	for i, route := range routes {
		prefix, index, nextHop, err := windowsRouteParts(route)
		if err != nil {
			return append([]Route(nil), routes...), err
		}
		indexText := strconv.Itoa(index)
		commands = append(commands,
			"try { $matches=@(Get-NetRoute -PolicyStore ActiveStore -DestinationPrefix '"+prefix+"' -InterfaceIndex "+indexText+" -ErrorAction SilentlyContinue | Where-Object { $_.NextHop -eq '"+nextHop+"' }); foreach ($match in $matches) { Remove-NetRoute -InputObject $match -Confirm:$false -ErrorAction Stop } } catch { $failed += "+strconv.Itoa(i)+" }",
		)
	}
	commands = append(commands, "[pscustomobject]@{Failed=@($failed)} | ConvertTo-Json -Compress")
	out, err := runPowerShell(r, strings.Join(commands, "; "))
	if err != nil {
		return append([]Route(nil), routes...), err
	}
	var result struct {
		Failed []int `json:"Failed"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &result); err != nil {
		return append([]Route(nil), routes...), fmt.Errorf("decode route cleanup result: %w", err)
	}
	failed := make([]Route, 0, len(result.Failed))
	seen := make(map[int]struct{}, len(result.Failed))
	for _, index := range result.Failed {
		if index < 0 || index >= len(routes) {
			return append([]Route(nil), routes...), fmt.Errorf("route cleanup returned invalid index %d", index)
		}
		if _, duplicate := seen[index]; duplicate {
			continue
		}
		seen[index] = struct{}{}
		failed = append(failed, routes[index])
	}
	if len(failed) > 0 {
		return failed, fmt.Errorf("failed to remove %d of %d Windows routes", len(failed), len(routes))
	}
	return nil, nil
}

func clearWindowsTUNConfiguration(r commandRunner, interfaceIndex int) error {
	if interfaceIndex <= 0 {
		return nil
	}
	script := strings.Join([]string{
		"$ErrorActionPreference='Stop'",
		"$idx=" + strconv.Itoa(interfaceIndex),
		"Get-NetIPAddress -InterfaceIndex $idx -AddressFamily IPv4 -IPAddress '198.18.0.1' -ErrorAction SilentlyContinue | Remove-NetIPAddress -Confirm:$false -ErrorAction Stop",
		"Get-NetIPAddress -InterfaceIndex $idx -AddressFamily IPv6 -IPAddress 'fd7a:6d61:6374:756e::1' -ErrorAction SilentlyContinue | Remove-NetIPAddress -Confirm:$false -ErrorAction Stop",
	}, "; ")
	if _, err := runPowerShell(r, script); err != nil {
		return fmt.Errorf("clear Wintun interface configuration: %w", err)
	}
	return nil
}

func readSystemDNSServers(r commandRunner) ([]netip.Addr, error) {
	script := "@((Get-DnsClientServerAddress -ErrorAction SilentlyContinue).ServerAddresses) | Where-Object { $_ } | Select-Object -Unique | ConvertTo-Json -Compress"
	out, err := runPowerShell(r, script)
	if err != nil {
		return nil, err
	}
	trimmed := strings.TrimSpace(out)
	if trimmed == "" || trimmed == "null" {
		return nil, nil
	}
	var values []string
	if strings.HasPrefix(trimmed, "[") {
		if err := json.Unmarshal([]byte(trimmed), &values); err != nil {
			return nil, err
		}
	} else {
		var value string
		if err := json.Unmarshal([]byte(trimmed), &value); err != nil {
			return nil, err
		}
		values = []string{value}
	}
	seen := make(map[netip.Addr]struct{})
	for _, value := range values {
		if addr, err := netip.ParseAddr(value); err == nil && !addr.IsUnspecified() && !addr.IsMulticast() {
			seen[addr.Unmap()] = struct{}{}
		}
	}
	servers := make([]netip.Addr, 0, len(seen))
	for server := range seen {
		servers = append(servers, server)
	}
	return servers, nil
}

func systemDNSServers(r commandRunner) []netip.Addr {
	servers, _ := readSystemDNSServers(r)
	return servers
}
