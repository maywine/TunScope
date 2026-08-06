//go:build windows

package mactun

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
)

const (
	windowsNetworkPollInterval = time.Second
	windowsNetworkLossGrace    = 30 * time.Second
)

func (a *App) Up(cfg Config) (returnErr error) {
	if !windows.GetCurrentProcessToken().IsElevated() {
		return fmt.Errorf("administrator privileges are required; open an elevated Terminal")
	}
	info, err := validateWindowsConfig(cfg)
	if err != nil {
		return err
	}
	configuredApplications, _, err := validateApplicationTargets(cfg.Applications)
	if err != nil {
		return err
	}
	cfg.Applications = configuredApplications

	if err := acquireLock(); err != nil {
		return err
	}
	defer releaseLock()
	if err := a.recoverWindowsStale(); err != nil {
		return err
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	defer signal.Stop(sigCh)

	capabilities, err := checkSOCKS5(info)
	if err != nil {
		return err
	}
	trustedDNS, err := parseTrustedDNS(cfg.TrustedDNS)
	if err != nil {
		return fmt.Errorf("--trusted-dns: %w", err)
	}
	if len(cfg.Applications) > 0 && trustedDNS.IsValid() {
		if err := checkTrustedDNS(cfg.Proxy, trustedDNS); err != nil {
			return fmt.Errorf("trusted DNS check through SOCKS5 failed for %s: %w", trustedDNS, err)
		}
		fmt.Fprintf(a.out, "trusted DNS check passed through SOCKS5: %s\n", trustedDNS)
	}
	if capabilities.UDP {
		fmt.Fprintf(a.out, "proxy check passed: %s (TCP + UDP data)\n", redactProxy(cfg.Proxy))
	} else if len(cfg.Applications) > 0 {
		fmt.Fprintf(a.out, "proxy TCP check passed: %s\n", redactProxy(cfg.Proxy))
		fmt.Fprintf(a.out, "warning: SOCKS5 UDP data is unavailable; selected applications will use DNS-over-TCP and TCP fallback: %s\n", capabilities.UDPWarning)
	} else {
		return fmt.Errorf("SOCKS5 UDP data is unavailable and global mode requires UDP: %s", capabilities.UDPWarning)
	}
	if cfg.TCPOnly {
		if len(cfg.Applications) == 0 {
			return fmt.Errorf("TCP-only compatibility mode requires at least one --app")
		}
		capabilities.UDP = false
		fmt.Fprintln(a.out, "TCP-only compatibility mode enabled: selected-application UDP is blocked so applications fall back to proxied TCP")
	}

	engineProxy, err := proxyURLWithResolvedHost(info)
	if err != nil {
		return err
	}
	physical, err := readWindowsPhysicalNetwork(a.runner, cfg.Interface, cfg.IPv6)
	if err != nil {
		return err
	}
	if cfg.Gateway4 != "" {
		physical.Gateway4 = cfg.Gateway4
	}
	if strings.EqualFold(physical.InterfaceAlias, cfg.Device) {
		return fmt.Errorf("--device must differ from the physical interface %q", physical.InterfaceAlias)
	}

	baseBypassValues := append([]string(nil), cfg.Bypass...)
	if !info.Loopback {
		baseBypassValues = append(baseBypassValues, info.Host)
	}
	baseBypasses, err := resolveBypasses(baseBypassValues)
	if err != nil {
		return err
	}
	discoverPeers := info.Loopback && (cfg.AutoBypass || len(cfg.Applications) > 0)
	var autoPeers []string
	if discoverPeers {
		autoPeers = discoverProxyPeers(a.runner, info.Port)
	}
	bypasses, err := mergeWindowsBypasses(baseBypasses, autoPeers)
	if err != nil {
		return err
	}
	if info.Loopback && len(cfg.Applications) == 0 && len(bypasses) == 0 {
		return fmt.Errorf("a loopback proxy in global mode requires --bypass <remote-node-host-or-IP> or --auto-bypass to prevent a proxy loop")
	}

	ownerIdentity, err := readProcessIdentity(os.Getpid())
	if err != nil {
		return fmt.Errorf("record mactun owner identity: %w", err)
	}
	ownerToken := currentLockToken()
	if ownerToken == "" {
		return fmt.Errorf("mactun lock has no owner token")
	}
	stopHandle, stopEvent, err := createWindowsStopEvent(ownerToken)
	if err != nil {
		return err
	}
	stopEventCh := waitWindowsEvent(stopHandle)
	defer func() {
		_ = windows.SetEvent(stopHandle)
		_ = windows.CloseHandle(stopHandle)
	}()

	state := &State{
		Version:        stateVersion,
		Phase:          "starting",
		OwnerPID:       os.Getpid(),
		OwnerToken:     ownerToken,
		OwnerStartedAt: ownerIdentity.StartedAt,
		OwnerCommand:   ownerIdentity.Command,
		StopEvent:      stopEvent,
		StartedAt:      time.Now(),
		Proxy:          redactProxy(cfg.Proxy),
		Device:         cfg.Device,
		Interface:      physical.InterfaceAlias,
		Interface6:     physical.Interface6Alias,
		PhysicalIPv4:   []string{physical.Source4},
		Gateway4:       physical.Gateway4,
		Gateway6:       physical.Gateway6,
		AutoBypasses:   append([]string(nil), autoPeers...),
		Applications:   append([]string(nil), cfg.Applications...),
	}
	if physical.Source6 != "" {
		state.PhysicalIPv6 = []string{physical.Source6}
	}
	if err := saveState(state); err != nil {
		return err
	}

	engineCfg := EngineConfig{
		Proxy:            engineProxy,
		Device:           cfg.Device,
		DirectInterface:  physical.InterfaceAlias,
		DirectInterface6: physical.Interface6Alias,
		DirectSource4:    physical.Source4,
		Applications:     append([]string(nil), cfg.Applications...),
		ProxyUDP:         capabilities.UDP,
		TrustedDNS:       cfg.TrustedDNS,
		MTU:              cfg.MTU,
		LogLevel:         cfg.LogLevel,
	}
	if !info.Loopback {
		engineCfg.Interface = physical.InterfaceAlias
	}
	cmd, waitCh, control, err := a.startWindowsEngine(engineCfg)
	if err != nil {
		if removeErr := os.Remove(statePath()); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return errors.Join(err, fmt.Errorf("remove failed startup state: %w", removeErr))
		}
		return err
	}
	state.EnginePID = cmd.Process.Pid
	cleanupDone := false
	defer func() {
		if !cleanupDone {
			if cleanupErr := a.cleanupWindows(state, cmd.Process, control); cleanupErr != nil {
				returnErr = errors.Join(returnErr, fmt.Errorf("restore Windows routes: %w", cleanupErr))
			}
		}
	}()
	engineIdentity, err := readProcessIdentity(state.EnginePID)
	if err != nil {
		return fmt.Errorf("record TUN engine identity: %w", err)
	}
	state.EngineStartedAt = engineIdentity.StartedAt
	state.EngineCommand = engineIdentity.Command
	if err := saveState(state); err != nil {
		return err
	}
	if err := control.WaitReady(waitCh, 8*time.Second); err != nil {
		return err
	}

	tunInterface, err := waitForWindowsInterface(cfg.Device, waitCh, 3*time.Second)
	if err != nil {
		return err
	}
	state.DeviceIndex = tunInterface.Index
	if err := saveState(state); err != nil {
		return err
	}
	if err := configureWindowsTUN(a.runner, tunInterface.Index, cfg.MTU, cfg.IPv6); err != nil {
		return err
	}
	dnsServers := windowsDNSAddresses(physical.DNSServers)
	for _, route := range windowsPhysicalRoutes(cfg, physical, bypasses, dnsServers) {
		if err := a.addAndSaveWindowsRoute(state, route); err != nil {
			return fmt.Errorf("add %s route %s: %w", route.Purpose, route.Target, err)
		}
	}
	for _, route := range windowsCaptureRoutes(tunInterface.Index, cfg.IPv6) {
		if err := a.addAndSaveWindowsRoute(state, route); err != nil {
			return fmt.Errorf("add TUN route %s: %w", route.Target, err)
		}
	}
	for _, route := range windowsTUNDNSRoutes(cfg, tunInterface.Index, dnsServers) {
		if err := a.addAndSaveWindowsRoute(state, route); err != nil {
			return fmt.Errorf("add TUN DNS route %s: %w", route.Target, err)
		}
	}

	state.Phase = "active"
	if err := saveState(state); err != nil {
		return err
	}
	fmt.Fprintf(a.out, "TUN is active on %s via %s; press Ctrl-C to stop\n", cfg.Device, physical.InterfaceAlias)
	if len(cfg.Applications) > 0 {
		fmt.Fprintf(a.out, "per-app mode is active for %d application(s); unselected and unknown owners stay on the physical interface\n", len(cfg.Applications))
	}
	if len(autoPeers) > 0 {
		fmt.Fprintf(a.out, "auto-bypassed %d current proxy peer(s): %s\n", len(autoPeers), strings.Join(autoPeers, ", "))
	}

	automaticNetwork := cfg.Interface == "" && cfg.Gateway4 == ""
	var networkTicker *time.Ticker
	var networkTicks <-chan time.Time
	if automaticNetwork {
		networkTicker = time.NewTicker(windowsNetworkPollInterval)
		networkTicks = networkTicker.C
		defer networkTicker.Stop()
	}
	physicalSignature := windowsPhysicalSignature(physical)
	candidateSignature := ""
	candidateCount := 0
	var networkUnavailableSince time.Time

activeLoop:
	for {
		select {
		case <-sigCh:
			fmt.Fprintln(a.out, "received interrupt, restoring routes...")
			break activeLoop
		case eventErr := <-stopEventCh:
			if eventErr != nil {
				returnErr = fmt.Errorf("owner stop event failed: %w", eventErr)
			} else {
				fmt.Fprintln(a.out, "received mactun down request, restoring routes...")
			}
			break activeLoop
		case engineErr := <-waitCh:
			if engineErr == nil {
				returnErr = fmt.Errorf("TUN engine stopped unexpectedly")
			} else {
				returnErr = fmt.Errorf("TUN engine stopped unexpectedly: %w", engineErr)
			}
			break activeLoop
		case <-networkTicks:
			next, sampleErr := readWindowsPhysicalNetwork(a.runner, "", cfg.IPv6)
			if sampleErr != nil {
				if networkUnavailableSince.IsZero() {
					networkUnavailableSince = time.Now()
					fmt.Fprintf(a.errOut, "network update: physical route is temporarily unavailable: %v\n", sampleErr)
				}
				if time.Since(networkUnavailableSince) >= windowsNetworkLossGrace {
					returnErr = fmt.Errorf("physical network was unavailable for %s: %w", windowsNetworkLossGrace, sampleErr)
					break activeLoop
				}
				continue
			}
			if !networkUnavailableSince.IsZero() {
				fmt.Fprintln(a.out, "network update: physical route is available again")
				networkUnavailableSince = time.Time{}
			}
			nextSignature := windowsPhysicalSignature(next)
			if nextSignature == physicalSignature {
				candidateSignature = ""
				candidateCount = 0
				continue
			}
			if nextSignature != candidateSignature {
				candidateSignature = nextSignature
				candidateCount = 1
				continue
			}
			candidateCount++
			if candidateCount < 2 {
				continue
			}
			if next.InterfaceIndex != physical.InterfaceIndex || !strings.EqualFold(next.InterfaceAlias, physical.InterfaceAlias) {
				returnErr = fmt.Errorf("physical interface changed from %s to %s; stopping safely so mactun can be started against the new adapter", physical.InterfaceAlias, next.InterfaceAlias)
				break activeLoop
			}
			if cfg.IPv6 && (next.Interface6Index != physical.Interface6Index || !strings.EqualFold(next.Interface6Alias, physical.Interface6Alias)) {
				returnErr = fmt.Errorf("physical IPv6 interface changed from %s to %s; stopping safely so mactun can be started against the new adapter", physical.Interface6Alias, next.Interface6Alias)
				break activeLoop
			}
			if err := a.reconcileWindowsPhysicalRoutes(state, cfg, next, bypasses, windowsDNSAddresses(next.DNSServers)); err != nil {
				returnErr = fmt.Errorf("reconcile Windows physical routes: %w", err)
				break activeLoop
			}
			closed, err := control.RebindNetwork(next.Source4, 3*time.Second)
			if err != nil {
				returnErr = fmt.Errorf("rebind TUN flows after physical network change: %w", err)
				break activeLoop
			}
			fmt.Fprintf(a.out, "network update: gateway/source changed on %s; refreshed routes and closed %d stale egress connection(s)\n", next.InterfaceAlias, closed)
			physical = next
			physicalSignature = nextSignature
			candidateSignature = ""
			candidateCount = 0
		}
	}

	cleanupErr := a.cleanupWindows(state, cmd.Process, control)
	cleanupDone = true
	if cleanupErr != nil {
		return errors.Join(returnErr, fmt.Errorf("restore Windows routes: %w", cleanupErr))
	}
	fmt.Fprintln(a.out, "TUN stopped; routes restored")
	return returnErr
}

func validateWindowsConfig(cfg Config) (proxyInfo, error) {
	info, err := validateProxy(cfg.Proxy)
	if err != nil {
		return proxyInfo{}, err
	}
	device := strings.TrimSpace(cfg.Device)
	if device == "" || device != cfg.Device || len(device) > 128 || strings.ContainsAny(device, `\/:*?"<>|`) {
		return proxyInfo{}, fmt.Errorf("--device must be a valid Windows adapter name")
	}
	if cfg.MTU < 1280 || cfg.MTU > 9000 {
		return proxyInfo{}, fmt.Errorf("--mtu must be between 1280 and 9000")
	}
	validLevels := map[string]bool{"debug": true, "info": true, "warn": true, "error": true, "silent": true}
	if !validLevels[cfg.LogLevel] {
		return proxyInfo{}, fmt.Errorf("invalid --log-level %q", cfg.LogLevel)
	}
	if _, err := parseTrustedDNS(cfg.TrustedDNS); err != nil {
		return proxyInfo{}, fmt.Errorf("--trusted-dns: %w", err)
	}
	if cfg.Gateway4 != "" {
		if ip, err := netip.ParseAddr(cfg.Gateway4); err != nil || !ip.Is4() || ip.IsUnspecified() {
			return proxyInfo{}, fmt.Errorf("--gateway must be a usable IPv4 address")
		}
	}
	return info, nil
}

func mergeWindowsBypasses(base []netip.Prefix, peers []string) ([]netip.Prefix, error) {
	values := make([]string, 0, len(base)+len(peers))
	for _, prefix := range base {
		values = append(values, prefix.String())
	}
	values = append(values, peers...)
	return resolveBypasses(values)
}

func windowsDNSAddresses(values []string) []netip.Addr {
	seen := make(map[netip.Addr]struct{})
	for _, value := range values {
		addr, err := netip.ParseAddr(strings.TrimSpace(value))
		if err != nil {
			continue
		}
		addr = addr.Unmap()
		if addr.IsUnspecified() || addr.IsLoopback() || addr.IsMulticast() || addr.IsLinkLocalUnicast() {
			continue
		}
		seen[addr] = struct{}{}
	}
	result := make([]netip.Addr, 0, len(seen))
	for addr := range seen {
		result = append(result, addr)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].String() < result[j].String() })
	return result
}

func windowsPhysicalRoutes(cfg Config, physical windowsPhysicalNetwork, bypasses []netip.Prefix, dns []netip.Addr) []Route {
	routes := make([]Route, 0, len(bypasses)+len(dns))
	for _, prefix := range bypasses {
		if prefix.Addr().Is4() {
			routes = append(routes, Route{Family: "inet", Kind: "net", Target: prefix.String(), Gateway: physical.Gateway4, Interface: strconv.Itoa(physical.InterfaceIndex), Purpose: "bypass"})
			continue
		}
		if physical.Interface6Index > 0 {
			routes = append(routes, Route{Family: "inet6", Kind: "net", Target: prefix.String(), Gateway: physical.Gateway6, Interface: strconv.Itoa(physical.Interface6Index), Purpose: "bypass"})
		}
	}
	if len(cfg.Applications) > 0 && strings.TrimSpace(cfg.TrustedDNS) == "" {
		for _, server := range dns {
			if server.Is4() {
				routes = append(routes, Route{Family: "inet", Kind: "host", Target: server.String(), Gateway: physical.Gateway4, Interface: strconv.Itoa(physical.InterfaceIndex), Purpose: "dns-direct"})
			} else if cfg.IPv6 && physical.Interface6Index > 0 {
				routes = append(routes, Route{Family: "inet6", Kind: "host", Target: server.String(), Gateway: physical.Gateway6, Interface: strconv.Itoa(physical.Interface6Index), Purpose: "dns-direct"})
			}
		}
	}
	return routes
}

func windowsCaptureRoutes(interfaceIndex int, ipv6 bool) []Route {
	routes := make([]Route, 0, len(ipv4TunNetworks)+2)
	for _, network := range ipv4TunNetworks {
		if network == "198.18.0.0/15" {
			// Assigning 198.18.0.1/15 creates this connected route. Adding a
			// second route for the same prefix can fail on Windows, while the
			// connected route already delivers that range to Wintun.
			continue
		}
		routes = append(routes, Route{Family: "inet", Kind: "net", Target: network, Gateway: tunGateway4, Interface: strconv.Itoa(interfaceIndex), Purpose: "tun"})
	}
	if ipv6 {
		for _, network := range []string{"::/1", "8000::/1"} {
			routes = append(routes, Route{Family: "inet6", Kind: "net", Target: network, Interface: strconv.Itoa(interfaceIndex), Purpose: "tun"})
		}
	}
	return routes
}

func windowsTUNDNSRoutes(cfg Config, interfaceIndex int, dns []netip.Addr) []Route {
	if len(cfg.Applications) > 0 && strings.TrimSpace(cfg.TrustedDNS) == "" {
		return nil
	}
	routes := make([]Route, 0, len(dns))
	for _, server := range dns {
		if server.Is6() && !cfg.IPv6 {
			continue
		}
		family := "inet"
		if server.Is6() {
			family = "inet6"
		}
		route := Route{Family: family, Kind: "host", Target: server.String(), Interface: strconv.Itoa(interfaceIndex), Purpose: "dns"}
		if server.Is4() {
			route.Gateway = tunGateway4
		}
		routes = append(routes, route)
	}
	return routes
}

func (a *App) addAndSaveWindowsRoute(state *State, route Route) error {
	added, err := addWindowsRoute(a.runner, route)
	if err != nil {
		return err
	}
	if !added {
		return nil
	}
	state.Routes = append(state.Routes, route)
	return saveState(state)
}

func windowsRouteKey(route Route) string {
	return strings.Join([]string{route.Family, route.Target, route.Gateway, route.Interface, route.Purpose}, "\x00")
}

func windowsPhysicalSignature(physical windowsPhysicalNetwork) string {
	dns := append([]string(nil), physical.DNSServers...)
	sort.Strings(dns)
	return strings.Join([]string{
		strconv.Itoa(physical.InterfaceIndex), physical.InterfaceAlias,
		physical.Gateway4, physical.Source4,
		strconv.Itoa(physical.Interface6Index), physical.Interface6Alias,
		physical.Gateway6, strings.Join(dns, ","),
	}, "\x00")
}

func windowsManagedPhysicalRoute(route Route) bool {
	return route.Purpose == "bypass" || route.Purpose == "dns-direct" || route.Purpose == "dns"
}

func (a *App) reconcileWindowsPhysicalRoutes(state *State, cfg Config, physical windowsPhysicalNetwork, bypasses []netip.Prefix, dns []netip.Addr) error {
	desired := windowsPhysicalRoutes(cfg, physical, bypasses, dns)
	desired = append(desired, windowsTUNDNSRoutes(cfg, state.DeviceIndex, dns)...)
	desiredKeys := make(map[string]struct{}, len(desired))
	currentKeys := make(map[string]struct{})
	for _, route := range state.Routes {
		if windowsManagedPhysicalRoute(route) {
			currentKeys[windowsRouteKey(route)] = struct{}{}
		}
	}
	for _, route := range desired {
		key := windowsRouteKey(route)
		desiredKeys[key] = struct{}{}
		if _, exists := currentKeys[key]; exists {
			continue
		}
		if err := a.addAndSaveWindowsRoute(state, route); err != nil {
			return fmt.Errorf("add replacement %s route %s: %w", route.Purpose, route.Target, err)
		}
	}
	for i := len(state.Routes) - 1; i >= 0; i-- {
		route := state.Routes[i]
		if !windowsManagedPhysicalRoute(route) {
			continue
		}
		if _, keep := desiredKeys[windowsRouteKey(route)]; keep {
			continue
		}
		if err := deleteWindowsRoute(a.runner, route); err != nil {
			return fmt.Errorf("delete replaced %s route %s: %w", route.Purpose, route.Target, err)
		}
		state.Routes = append(state.Routes[:i], state.Routes[i+1:]...)
		if err := saveState(state); err != nil {
			return err
		}
	}
	state.Interface = physical.InterfaceAlias
	state.Interface6 = physical.Interface6Alias
	state.Gateway4 = physical.Gateway4
	state.Gateway6 = physical.Gateway6
	state.PhysicalIPv4 = []string{physical.Source4}
	state.PhysicalIPv6 = nil
	if physical.Source6 != "" {
		state.PhysicalIPv6 = []string{physical.Source6}
	}
	return saveState(state)
}

type windowsEngineController struct {
	mu           sync.Mutex
	commands     *os.File
	responseFile *os.File
	responses    <-chan EngineControlResponse
	responseErr  <-chan error
	generation   uint64
	closeOnce    sync.Once
}

func newWindowsEngineController(commands, responseFile *os.File) *windowsEngineController {
	responses := make(chan EngineControlResponse, 1)
	errorsCh := make(chan error, 1)
	go func() {
		decoder := json.NewDecoder(responseFile)
		for {
			var response EngineControlResponse
			if err := decoder.Decode(&response); err != nil {
				errorsCh <- err
				return
			}
			responses <- response
		}
	}()
	return &windowsEngineController{commands: commands, responseFile: responseFile, responses: responses, responseErr: errorsCh}
}

func (c *windowsEngineController) RebindNetwork(source4 string, timeout time.Duration) (int, error) {
	if c == nil {
		return 0, fmt.Errorf("engine control channel is unavailable")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.generation++
	command := NewEngineNetworkCommand(c.generation, source4)
	if err := json.NewEncoder(c.commands).Encode(command); err != nil {
		return 0, fmt.Errorf("send engine network update: %w", err)
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case response := <-c.responses:
		if response.Action != command.Action || response.Generation != command.Generation {
			return 0, fmt.Errorf("unexpected engine acknowledgement")
		}
		if response.Error != "" {
			return 0, errors.New(response.Error)
		}
		return response.Closed, nil
	case err := <-c.responseErr:
		return 0, fmt.Errorf("engine control channel closed: %w", err)
	case <-timer.C:
		return 0, fmt.Errorf("timed out waiting for engine network update")
	}
}

func (c *windowsEngineController) WaitReady(child <-chan error, timeout time.Duration) error {
	if c == nil {
		return fmt.Errorf("engine control channel is unavailable")
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case response := <-c.responses:
		if response.Action != "ready" || response.Error != "" {
			if response.Error != "" {
				return fmt.Errorf("TUN engine startup failed: %s", response.Error)
			}
			return fmt.Errorf("unexpected TUN engine startup response %q", response.Action)
		}
		return nil
	case err := <-c.responseErr:
		return fmt.Errorf("TUN engine closed its startup channel: %w", err)
	case err := <-child:
		if err == nil {
			return fmt.Errorf("TUN engine exited during startup")
		}
		return fmt.Errorf("TUN engine exited during startup: %w", err)
	case <-timer.C:
		return fmt.Errorf("timed out waiting for TUN engine startup; ensure wintun.dll is next to mactun.exe")
	}
}

func (c *windowsEngineController) Close() {
	if c == nil {
		return
	}
	c.closeOnce.Do(func() {
		_ = c.commands.Close()
		_ = c.responseFile.Close()
	})
}

func (a *App) startWindowsEngine(cfg EngineConfig) (*exec.Cmd, <-chan error, *windowsEngineController, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, nil, nil, err
	}
	configData, err := json.Marshal(cfg)
	if err != nil {
		return nil, nil, nil, err
	}
	commandRead, commandWrite, err := os.Pipe()
	if err != nil {
		return nil, nil, nil, err
	}
	responseRead, responseWrite, err := os.Pipe()
	if err != nil {
		commandRead.Close()
		commandWrite.Close()
		return nil, nil, nil, err
	}
	closeAll := func() {
		commandRead.Close()
		commandWrite.Close()
		responseRead.Close()
		responseWrite.Close()
	}
	commandHandle := windows.Handle(commandRead.Fd())
	responseHandle := windows.Handle(responseWrite.Fd())
	for _, handle := range []windows.Handle{commandHandle, responseHandle} {
		if err := windows.SetHandleInformation(handle, windows.HANDLE_FLAG_INHERIT, windows.HANDLE_FLAG_INHERIT); err != nil {
			closeAll()
			return nil, nil, nil, fmt.Errorf("make engine pipe inheritable: %w", err)
		}
	}
	cmd := exec.Command(
		exe,
		"__engine",
		"--command-handle", strconv.FormatUint(uint64(commandHandle), 10),
		"--response-handle", strconv.FormatUint(uint64(responseHandle), 10),
	)
	cmd.Stdin = bytes.NewReader(append(configData, '\n'))
	cmd.Stdout = a.out
	cmd.Stderr = a.errOut
	cmd.SysProcAttr = &syscall.SysProcAttr{
		AdditionalInheritedHandles: []syscall.Handle{syscall.Handle(commandHandle), syscall.Handle(responseHandle)},
	}
	if err := cmd.Start(); err != nil {
		closeAll()
		return nil, nil, nil, fmt.Errorf("start TUN engine: %w", err)
	}
	_ = windows.SetHandleInformation(commandHandle, windows.HANDLE_FLAG_INHERIT, 0)
	_ = windows.SetHandleInformation(responseHandle, windows.HANDLE_FLAG_INHERIT, 0)
	commandRead.Close()
	responseWrite.Close()
	control := newWindowsEngineController(commandWrite, responseRead)
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	return cmd, waitCh, control, nil
}

func waitForWindowsInterface(name string, child <-chan error, timeout time.Duration) (*net.Interface, error) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case err := <-child:
			if err == nil {
				return nil, fmt.Errorf("TUN engine exited before %s appeared", name)
			}
			return nil, fmt.Errorf("TUN engine failed before %s appeared: %w", name, err)
		case <-ticker.C:
			if iface, err := net.InterfaceByName(name); err == nil {
				return iface, nil
			}
		case <-deadline.C:
			return nil, fmt.Errorf("timed out waiting for Wintun interface %s; ensure wintun.dll is next to mactun.exe", name)
		}
	}
}

func windowsCleanupPriority(route Route) int {
	switch route.Purpose {
	case "dns":
		return 0
	case "tun":
		return 1
	default:
		return 2
	}
}

func (a *App) cleanupWindows(state *State, engineProcess *os.Process, control *windowsEngineController) error {
	if state == nil {
		return fmt.Errorf("cleanup state is nil")
	}
	routes := append([]Route(nil), state.Routes...)
	sort.SliceStable(routes, func(i, j int) bool { return windowsCleanupPriority(routes[i]) < windowsCleanupPriority(routes[j]) })
	var cleanupErrs []error
	failed, routeCleanupErr := deleteWindowsRoutes(a.runner, routes)
	if routeCleanupErr != nil {
		cleanupErrs = append(cleanupErrs, fmt.Errorf("delete Windows routes: %w", routeCleanupErr))
	}
	state.Routes = failed
	state.RouteReconcile = nil
	if control != nil {
		control.Close()
	}
	if err := stopWindowsEngine(state, engineProcess); err != nil {
		cleanupErrs = append(cleanupErrs, err)
	}
	if err := clearWindowsTUNForState(a.runner, state); err != nil {
		cleanupErrs = append(cleanupErrs, err)
	} else {
		state.DeviceIndex = 0
	}
	if len(cleanupErrs) == 0 {
		if err := os.Remove(statePath()); err == nil || errors.Is(err, os.ErrNotExist) {
			return nil
		} else {
			cleanupErrs = append(cleanupErrs, fmt.Errorf("remove cleanup state: %w", err))
		}
	}
	state.Phase = "cleanup_failed"
	if err := saveState(state); err != nil {
		cleanupErrs = append(cleanupErrs, fmt.Errorf("save retryable cleanup state: %w", err))
	}
	return errors.Join(cleanupErrs...)
}

func clearWindowsTUNForState(r commandRunner, state *State) error {
	if state == nil || state.DeviceIndex <= 0 || state.Device == "" {
		return nil
	}
	iface, err := net.InterfaceByName(state.Device)
	if err != nil {
		// The adapter is already gone, so its ActiveStore addresses are gone too.
		return nil
	}
	if iface.Index != state.DeviceIndex {
		// Interface indexes are reusable. Never mutate a different adapter after
		// a reboot or a long-delayed stale-state cleanup.
		return nil
	}
	return clearWindowsTUNConfiguration(r, state.DeviceIndex)
}

func stopWindowsEngine(state *State, engineProcess *os.Process) error {
	pid := state.EnginePID
	trustedHandle := engineProcess != nil
	if engineProcess != nil {
		pid = engineProcess.Pid
	}
	if pid <= 0 || !processAlive(pid) {
		state.EnginePID = 0
		return nil
	}
	if !trustedHandle {
		matches, err := matchesProcessIdentity(pid, state.EngineStartedAt, state.EngineCommand)
		if err != nil {
			return fmt.Errorf("verify TUN engine PID %d before termination: %w", pid, err)
		}
		if !matches {
			state.EnginePID = 0
			return nil
		}
		var findErr error
		engineProcess, findErr = os.FindProcess(pid)
		if findErr != nil {
			return fmt.Errorf("find TUN engine PID %d: %w", pid, findErr)
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for processAlive(pid) && time.Now().Before(deadline) {
		time.Sleep(25 * time.Millisecond)
	}
	if !processAlive(pid) {
		state.EnginePID = 0
		return nil
	}
	if err := engineProcess.Kill(); err != nil && processAlive(pid) {
		return fmt.Errorf("terminate TUN engine PID %d: %w", pid, err)
	}
	deadline = time.Now().Add(time.Second)
	for processAlive(pid) && time.Now().Before(deadline) {
		time.Sleep(25 * time.Millisecond)
	}
	if processAlive(pid) {
		return fmt.Errorf("TUN engine PID %d is still running after termination", pid)
	}
	state.EnginePID = 0
	return nil
}

func (a *App) recoverWindowsStale() error {
	if currentLockToken() == "" {
		return fmt.Errorf("recover stale state requires the mactun lock")
	}
	state, err := loadState()
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if processAlive(state.OwnerPID) {
		matches, identityErr := matchesProcessIdentity(state.OwnerPID, state.OwnerStartedAt, state.OwnerCommand)
		if identityErr == nil && matches {
			return fmt.Errorf("mactun state belongs to a still-running owner PID %d", state.OwnerPID)
		}
		if identityErr != nil && processAlive(state.OwnerPID) {
			return fmt.Errorf("cannot prove stale owner PID %d is safe to replace: %w", state.OwnerPID, identityErr)
		}
	}
	fmt.Fprintln(a.errOut, "recovering stale Windows TUN state from a previous interrupted run")
	return a.cleanupWindows(state, nil, nil)
}

func (a *App) Down() error {
	if !windows.GetCurrentProcessToken().IsElevated() {
		return fmt.Errorf("administrator privileges are required; open an elevated Terminal")
	}
	if err := acquireLock(); err == nil {
		defer releaseLock()
		return a.downWindowsWhileLocked(false)
	} else if !errors.Is(err, errLockHeld) {
		return err
	}
	state, err := a.windowsStateForLockedOwner(750 * time.Millisecond)
	if err != nil {
		return err
	}
	if state.OwnerPID == os.Getpid() {
		return fmt.Errorf("refusing to signal the current owner process")
	}
	matches, err := matchesProcessIdentity(state.OwnerPID, state.OwnerStartedAt, state.OwnerCommand)
	if err != nil {
		if !processAlive(state.OwnerPID) {
			return a.takeWindowsLockAndFinishDown(8*time.Second, true)
		}
		return fmt.Errorf("verify mactun owner PID %d: %w", state.OwnerPID, err)
	}
	if !matches {
		return fmt.Errorf("mactun owner PID %d identity changed; refusing to signal a reused PID", state.OwnerPID)
	}
	if err := signalWindowsStopEvent(state); err != nil {
		if processAlive(state.OwnerPID) {
			return err
		}
	}
	return a.takeWindowsLockAndFinishDown(8*time.Second, true)
}

func (a *App) windowsStateForLockedOwner(timeout time.Duration) (*State, error) {
	deadline := time.Now().Add(timeout)
	for {
		state, stateErr := loadState()
		pid, token := lockRecord()
		if stateErr == nil && pid > 0 && token != "" && state.OwnerPID == pid && state.OwnerToken == token {
			return state, nil
		}
		if stateErr != nil && !errors.Is(stateErr, os.ErrNotExist) {
			return nil, stateErr
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("mactun lock/state owner identity is inconsistent")
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func (a *App) takeWindowsLockAndFinishDown(timeout time.Duration, ownerWasSignaled bool) error {
	deadline := time.Now().Add(timeout)
	for {
		if err := acquireLock(); err == nil {
			defer releaseLock()
			return a.downWindowsWhileLocked(ownerWasSignaled)
		} else if !errors.Is(err, errLockHeld) {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("mactun owner did not release the cleanup lock; state was retained")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (a *App) downWindowsWhileLocked(ownerWasSignaled bool) error {
	state, err := loadState()
	if errors.Is(err, os.ErrNotExist) {
		if ownerWasSignaled {
			fmt.Fprintln(a.out, "mactun stopped; routes restored")
		} else {
			fmt.Fprintln(a.out, "mactun is not running")
		}
		return nil
	}
	if err != nil {
		return err
	}
	if processAlive(state.OwnerPID) {
		matches, identityErr := matchesProcessIdentity(state.OwnerPID, state.OwnerStartedAt, state.OwnerCommand)
		if identityErr == nil && matches {
			return fmt.Errorf("owner PID %d is still running without the expected mutex", state.OwnerPID)
		}
		if identityErr != nil && processAlive(state.OwnerPID) {
			return fmt.Errorf("cannot prove owner PID %d is stale: %w", state.OwnerPID, identityErr)
		}
	}
	if err := a.cleanupWindows(state, nil, nil); err != nil {
		return fmt.Errorf("stop mactun: %w", err)
	}
	if ownerWasSignaled {
		fmt.Fprintln(a.out, "mactun stopped; routes restored")
	} else {
		fmt.Fprintln(a.out, "mactun stopped; stale routes removed")
	}
	return nil
}

func (a *App) Status() error {
	state, err := loadState()
	if errors.Is(err, os.ErrNotExist) {
		fmt.Fprintln(a.out, "status: stopped")
		return nil
	}
	if err != nil {
		return err
	}
	active, detail := activeWindowsStateIdentity(state)
	status := "stale"
	if active {
		status = "active"
	}
	fmt.Fprintf(a.out, "status: %s\n", status)
	if detail != "" {
		fmt.Fprintf(a.out, "status detail: %s\n", detail)
	}
	fmt.Fprintf(a.out, "phase: %s\nproxy: %s\ndevice: %s\nphysical interface: %s\nowner PID: %d\nengine PID: %d\n", state.Phase, state.Proxy, state.Device, state.Interface, state.OwnerPID, state.EnginePID)
	if len(state.Applications) > 0 {
		fmt.Fprintf(a.out, "applications: %d\n", len(state.Applications))
	}
	counts := make(map[string]int)
	for _, route := range state.Routes {
		counts[route.Purpose]++
	}
	keys := make([]string, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		fmt.Fprintf(a.out, "%s routes: %d\n", key, counts[key])
	}
	return nil
}

func activeWindowsStateIdentity(state *State) (bool, string) {
	if state.Phase != "active" {
		return false, fmt.Sprintf("state phase is %s", state.Phase)
	}
	pid, token := lockRecord()
	if state.OwnerToken == "" || pid != state.OwnerPID || token != state.OwnerToken {
		return false, "lock owner does not match the persisted session"
	}
	ownerMatches, err := matchesProcessIdentity(state.OwnerPID, state.OwnerStartedAt, state.OwnerCommand)
	if err != nil || !ownerMatches {
		return false, fmt.Sprintf("owner identity check failed: %v", err)
	}
	engineMatches, err := matchesProcessIdentity(state.EnginePID, state.EngineStartedAt, state.EngineCommand)
	if err != nil || !engineMatches {
		return false, fmt.Sprintf("engine identity check failed: %v", err)
	}
	return true, ""
}

func (a *App) Doctor(proxy string) error {
	info, err := validateProxy(proxy)
	if err != nil {
		return err
	}
	capabilities, err := checkSOCKS5(info)
	if err != nil {
		return err
	}
	if capabilities.UDP {
		fmt.Fprintf(a.out, "OK: %s carries SOCKS5 TCP and UDP data\n", redactProxy(proxy))
	} else {
		fmt.Fprintf(a.out, "OK: %s carries SOCKS5 TCP\nwarning: UDP data failed: %s\n", redactProxy(proxy), capabilities.UDPWarning)
	}
	if info.Loopback {
		peers := discoverProxyPeers(a.runner, info.Port)
		if len(peers) == 0 {
			fmt.Fprintln(a.out, "note: no active remote peer was found; global mode needs --bypass or --auto-bypass")
		} else {
			fmt.Fprintf(a.out, "active remote peers available for auto-bypass: %s\n", strings.Join(peers, ", "))
		}
	}
	return nil
}
