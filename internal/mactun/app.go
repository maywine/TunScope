package mactun

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

var ipv4TunNetworks = []string{
	"1.0.0.0/8",
	"2.0.0.0/7",
	"4.0.0.0/6",
	"8.0.0.0/5",
	"16.0.0.0/4",
	"32.0.0.0/3",
	"64.0.0.0/2",
	"128.0.0.0/1",
	"198.18.0.0/15",
}

type App struct {
	runner commandRunner
	out    io.Writer
	errOut io.Writer
}

func New(out, errOut io.Writer) *App {
	return &App{runner: execRunner{}, out: out, errOut: errOut}
}

func (a *App) Up(cfg Config) (returnErr error) {
	return a.up(cfg, false, nil, nil)
}

// up can rebuild a session while retaining the supervisor's process-wide
// lock. Holding that lock across teardown/restart prevents `mactun down` from
// observing a false stopped gap immediately before TUN comes back up.
func (a *App) up(cfg Config, lockAlreadyHeld bool, sharedSigCh chan os.Signal, restartState *State) (returnErr error) {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("TUN setup is supported on macOS only")
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf("administrator privileges are required; run with sudo")
	}
	restartMarkerActive := restartState != nil
	defer func() {
		if restartMarkerActive {
			if cleanupErr := a.cleanup(restartState, nil); cleanupErr != nil {
				returnErr = errors.Join(returnErr, fmt.Errorf("remove automatic restart state: %w", cleanupErr))
			}
		}
	}()
	info, err := validateConfig(cfg)
	if err != nil {
		return err
	}
	configuredApplications, _, err := validateApplicationTargets(cfg.Applications)
	if err != nil {
		return err
	}

	if !lockAlreadyHeld {
		if err := acquireLock(); err != nil {
			return err
		}
		defer releaseLock()
	}
	sigCh := sharedSigCh
	if sigCh == nil {
		sigCh = make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
		defer signal.Stop(sigCh)
	}
	startupInterrupted := func() bool {
		select {
		case sig := <-sigCh:
			fmt.Fprintf(a.out, "received %s during startup, restoring routes...\n", sig)
			return true
		default:
			return false
		}
	}
	if !lockAlreadyHeld {
		if err := a.recoverStale(); err != nil {
			return err
		}
	}
	if startupInterrupted() {
		return nil
	}

	capabilities, err := checkSOCKS5(info)
	if err != nil {
		return err
	}
	trustedDNS, err := parseTrustedDNS(cfg.TrustedDNS)
	if err != nil {
		return fmt.Errorf("--trusted-dns: %w", err)
	}
	if len(configuredApplications) > 0 && trustedDNS.IsValid() {
		if err := checkTrustedDNS(cfg.Proxy, trustedDNS); err != nil {
			return fmt.Errorf("trusted DNS check through SOCKS5 failed for %s: %w", trustedDNS, err)
		}
		fmt.Fprintf(a.out, "trusted DNS check passed through SOCKS5: %s\n", trustedDNS)
	}
	if capabilities.UDP {
		fmt.Fprintf(a.out, "proxy check passed: %s (TCP + UDP data)\n", redactProxy(cfg.Proxy))
	} else if len(cfg.Applications) > 0 {
		fmt.Fprintf(a.out, "proxy TCP check passed: %s\n", redactProxy(cfg.Proxy))
		fmt.Fprintf(a.out, "warning: SOCKS5 UDP data is unavailable; using DNS-over-TCP and blocking selected-app non-DNS UDP for TCP fallback: %s\n", capabilities.UDPWarning)
	} else {
		return fmt.Errorf("SOCKS5 UDP data is unavailable and global mode requires UDP: %s", capabilities.UDPWarning)
	}
	if cfg.TCPOnly {
		if len(cfg.Applications) == 0 {
			return fmt.Errorf("TCP-only compatibility mode requires at least one --app")
		}
		capabilities.UDP = false
		capabilities.UDPWarning = "TCP-only compatibility mode is enabled"
		fmt.Fprintln(a.out, "TCP-only compatibility mode enabled: selected-app non-DNS UDP, including QUIC, is blocked so applications fall back to proxied TCP")
	}
	engineProxy, err := proxyURLWithResolvedHost(info)
	if err != nil {
		return err
	}

	gateway4, iface, err := defaultRoute4(a.runner)
	if err != nil {
		return fmt.Errorf("detect default IPv4 route: %w", err)
	}
	if cfg.Gateway4 != "" {
		gateway4 = cfg.Gateway4
	}
	if cfg.Interface != "" {
		iface = cfg.Interface
	}
	if strings.HasPrefix(iface, "utun") && (cfg.Interface == "" || cfg.Gateway4 == "") {
		return fmt.Errorf("the current default route already uses %s; pass both --interface and --gateway for the physical network", iface)
	}

	gateway6, iface6, _ := defaultRoute6(a.runner)
	if cfg.IPv6 && len(cfg.Applications) > 0 && iface6 == "" {
		cfg.IPv6 = false
		fmt.Fprintln(a.out, "warning: no usable IPv6 default route was found; IPv6 capture is disabled for this session")
	}
	automaticNetwork := cfg.Interface == "" && cfg.Gateway4 == ""
	initialPhysicalRoute := physicalRouteSnapshot{
		Gateway4: gateway4, Interface: iface,
		Gateway6: gateway6, Interface6: iface6,
	}
	if automaticNetwork {
		initialPhysicalRoute, err = samplePhysicalAddresses(a.runner, initialPhysicalRoute, cfg.IPv6 && iface6 != "")
		if err != nil {
			return fmt.Errorf("record initial physical interface addresses: %w", err)
		}
	}

	baseBypassValues := append([]string(nil), cfg.Bypass...)
	var autoPeers []string
	if !info.Loopback {
		// A remote proxy endpoint must never be routed back into its own TUN.
		baseBypassValues = append(baseBypassValues, info.Host)
	}
	discoverPeers := info.Loopback && (cfg.AutoBypass || len(cfg.Applications) > 0)
	if discoverPeers {
		autoPeers = discoverProxyPeers(a.runner, info.Port)
	}
	baseBypasses, err := resolveBypasses(baseBypassValues)
	if err != nil {
		return err
	}
	bypasses, err := mergeBypassPrefixes(baseBypasses, autoPeers)
	if err != nil {
		return err
	}
	if info.Loopback && len(cfg.Applications) == 0 && len(bypasses) == 0 {
		return fmt.Errorf("a loopback proxy requires at least one --bypass <remote-node-host-or-IP>; this prevents the proxy's own connection from looping into TUN")
	}
	if info.Loopback && len(cfg.Applications) > 0 && len(bypasses) == 0 {
		return fmt.Errorf("could not discover the local proxy's remote node; keep the proxy connected or provide --bypass <remote-node-host-or-IP>")
	}

	if _, err := net.InterfaceByName(cfg.Device); err == nil {
		return fmt.Errorf("device %s already exists; choose another --device or run mactun down", cfg.Device)
	}
	ownerIdentity, err := readProcessIdentity(os.Getpid())
	if err != nil {
		return fmt.Errorf("record mactun owner identity: %w", err)
	}
	ownerToken := currentLockToken()
	if ownerToken == "" {
		return fmt.Errorf("mactun lock has no owner token")
	}

	state := &State{
		Version:        stateVersion,
		Phase:          "starting",
		OwnerPID:       os.Getpid(),
		OwnerToken:     ownerToken,
		OwnerStartedAt: ownerIdentity.StartedAt,
		OwnerCommand:   ownerIdentity.Command,
		StartedAt:      time.Now(),
		Proxy:          redactProxy(cfg.Proxy),
		Device:         cfg.Device,
		Interface:      iface,
		Interface6:     iface6,
		PhysicalIPv4:   append([]string(nil), initialPhysicalRoute.IPv4...),
		PhysicalIPv6:   append([]string(nil), initialPhysicalRoute.IPv6...),
		Gateway4:       gateway4,
		Gateway6:       gateway6,
		AutoBypasses:   autoPeers,
		Applications:   append([]string(nil), configuredApplications...),
	}
	if err := saveState(state); err != nil {
		return err
	}
	restartMarkerActive = false
	if startupInterrupted() {
		return a.cleanup(state, nil)
	}

	engineRunConfig := cfg
	engineRunConfig.Proxy = engineProxy
	engineRunConfig.Applications = append([]string(nil), configuredApplications...)
	cmd, waitCh, engineControl, err := a.startEngine(
		engineRunConfig,
		info,
		iface,
		iface6,
		initialPhysicalRoute.Source4,
		capabilities,
	)
	if err != nil {
		if removeErr := os.Remove(statePath()); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return errors.Join(err, fmt.Errorf("remove failed startup state: %w", removeErr))
		}
		return err
	}
	defer engineControl.Close()
	state.EnginePID = cmd.Process.Pid
	cleanupDone := false
	defer func() {
		if !cleanupDone {
			if cleanupErr := a.cleanup(state, cmd.Process); cleanupErr != nil {
				returnErr = errors.Join(returnErr, fmt.Errorf("restore routes: %w", cleanupErr))
			}
		}
	}()
	engineIdentity, identityErr := readProcessIdentity(state.EnginePID)
	if identityErr != nil {
		state.Phase = "cleanup_failed"
		if saveErr := saveState(state); saveErr != nil {
			identityErr = errors.Join(identityErr, fmt.Errorf("save unidentified engine state: %w", saveErr))
		}
		return fmt.Errorf("record TUN engine birth identity: %w", identityErr)
	}
	state.EngineStartedAt = engineIdentity.StartedAt
	state.EngineCommand = engineIdentity.Command
	if err := saveState(state); err != nil {
		return err
	}
	if startupInterrupted() {
		return nil
	}

	if err := waitForInterface(cfg.Device, waitCh, 5*time.Second); err != nil {
		return err
	}
	if _, err := a.runner.Run("/sbin/ifconfig", cfg.Device, tunGateway4, tunGateway4, "mtu", fmt.Sprint(cfg.MTU), "up"); err != nil {
		return fmt.Errorf("configure %s: %w", cfg.Device, err)
	}
	if cfg.IPv6 {
		if _, err := a.runner.Run("/sbin/ifconfig", cfg.Device, "inet6", tunAddress6, tunAddress6, "prefixlen", "128", "alias"); err != nil {
			return fmt.Errorf("configure IPv6 on %s: %w", cfg.Device, err)
		}
	}

	for _, route := range routesWithPhysicalSources(
		bypassRoutes(bypasses, gateway4, gateway6, iface, iface6),
		initialPhysicalRoute,
	) {
		if err := a.addAndSaveRoute(state, route); err != nil {
			return fmt.Errorf("add bypass route for %s: %w", route.Target, err)
		}
	}
	if len(cfg.Applications) > 0 {
		for _, route := range routesWithPhysicalSources(
			directScopedRoutes(gateway4, gateway6, iface, iface6, cfg.IPv6),
			initialPhysicalRoute,
		) {
			if err := a.addAndSaveRoute(state, route); err != nil {
				return fmt.Errorf("add physical-interface scoped route %s: %w", route.Target, err)
			}
		}
	}
	for _, network := range ipv4TunNetworks {
		route := Route{Family: "inet", Kind: "net", Target: network, Gateway: tunGateway4, Purpose: "tun"}
		if err := a.addAndSaveRoute(state, route); err != nil {
			return fmt.Errorf("add TUN route %s: %w", network, err)
		}
	}
	if cfg.IPv6 {
		for _, network := range []string{"::/1", "8000::/1"} {
			route := Route{Family: "inet6", Kind: "net", Target: network, Interface: cfg.Device, Purpose: "tun"}
			if err := a.addAndSaveRoute(state, route); err != nil {
				return fmt.Errorf("add IPv6 TUN route %s: %w", network, err)
			}
		}
	}
	dnsServers, dnsErr := readSystemDNSServers(a.runner)
	if dnsErr != nil && len(cfg.Applications) > 0 {
		return fmt.Errorf("read system DNS configuration for direct routing: %w", dnsErr)
	}
	if len(dnsServers) == 0 && len(cfg.Applications) > 0 {
		return fmt.Errorf("system DNS configuration is empty; refusing per-app TUN because unselected applications would lose their direct resolver route")
	}
	if len(cfg.Applications) == 0 || strings.TrimSpace(cfg.TrustedDNS) == "" {
		for _, server := range routedSystemDNSServers(dnsServers, cfg.IPv6) {
			route := routeForSystemDNS(server, len(cfg.Applications) > 0, cfg.Device, gateway4, gateway6, iface6)
			route = routesWithPhysicalSources([]Route{route}, initialPhysicalRoute)[0]
			if err := a.addAndSaveRoute(state, route); err != nil {
				return fmt.Errorf("configure DNS route for %s: %w", server, err)
			}
		}
	}

	state.Phase = "active"
	if err := saveState(state); err != nil {
		return err
	}
	fmt.Fprintf(a.out, "TUN is active on %s via %s; press Ctrl-C to stop\n", cfg.Device, iface)
	if len(cfg.Applications) > 0 {
		fmt.Fprintf(a.out, "per-app mode is active for %d application(s); unknown owners stay direct while engine loops and ambiguous owners are blocked\n", len(cfg.Applications))
		if strings.TrimSpace(cfg.TrustedDNS) != "" {
			fmt.Fprintf(a.out, "system DNS is protected through SOCKS5 using trusted resolver %s; non-DNS traffic from unselected applications stays direct\n", cfg.TrustedDNS)
		} else {
			fmt.Fprintf(a.out, "shared system DNS keeps its configured path (loopback resolvers stay local; external resolvers stay direct via %s) so unselected applications keep their normal resolver path\n", iface)
		}
	}
	if len(autoPeers) > 0 {
		fmt.Fprintf(a.out, "auto-bypassed %d current proxy peer(s): %s\n", len(autoPeers), strings.Join(autoPeers, ", "))
	}

	var monitor *liveNetworkMonitor
	var networkTicker *time.Ticker
	var networkTicks <-chan time.Time
	// Explicit interface/gateway overrides are used when another VPN owns the
	// system default route. In that mode mactun cannot infer physical changes
	// safely, so automatic reconciliation remains disabled.
	if automaticNetwork {
		monitor = newLiveNetworkMonitor(
			initialPhysicalRoute,
			dnsServers,
			baseBypasses,
			autoPeers,
			cfg.IPv6 && iface6 != "",
			len(cfg.Applications) > 0,
			discoverPeers,
			info.Port,
		)
		networkTicker = time.NewTicker(networkPollInterval)
		networkTicks = networkTicker.C
		defer networkTicker.Stop()
	}

activeLoop:
	for {
		select {
		case sig := <-sigCh:
			fmt.Fprintf(a.out, "received %s, restoring routes...\n", sig)
			break activeLoop
		case engineErr := <-waitCh:
			if engineErr == nil {
				returnErr = fmt.Errorf("TUN engine stopped unexpectedly")
			} else {
				returnErr = fmt.Errorf("TUN engine stopped unexpectedly: %w", engineErr)
			}
			break activeLoop
		case <-networkTicks:
			updates, reconcileErr := monitor.poll(a, state, cfg)
			for _, update := range updates {
				fmt.Fprintf(a.out, "network update: %s\n", update)
			}
			if reconcileErr != nil {
				var networkChange *physicalNetworkChangeError
				if errors.As(reconcileErr, &networkChange) {
					fmt.Fprintf(a.out, "network update: %s; physical routes refreshed, rebinding old flows without dropping TUN capture\n", networkChange)
					closed, err := engineControl.RebindNetwork(networkChange.Source4, 3*time.Second)
					if err != nil {
						returnErr = fmt.Errorf("rebind TUN flows after physical network change: %w", err)
						fmt.Fprintln(a.errOut, returnErr)
						break activeLoop
					}
					fmt.Fprintf(a.out, "network update: engine acknowledged handoff; closed %d stale egress connection(s), TUN capture remained active\n", closed)
					continue
				}
				returnErr = fmt.Errorf("physical network reconciliation failed; stopping TUN to restore normal networking: %w", reconcileErr)
				fmt.Fprintln(a.errOut, returnErr)
				break activeLoop
			}
		}
	}

	cleanupErr := a.cleanup(state, cmd.Process)
	cleanupDone = true
	if cleanupErr != nil {
		return errors.Join(returnErr, fmt.Errorf("restore routes: %w", cleanupErr))
	}
	fmt.Fprintln(a.out, "TUN stopped; routes restored")
	return returnErr
}

func validateConfig(cfg Config) (proxyInfo, error) {
	info, err := validateProxy(cfg.Proxy)
	if err != nil {
		return proxyInfo{}, err
	}
	if !strings.HasPrefix(cfg.Device, "utun") || len(cfg.Device) <= len("utun") {
		return proxyInfo{}, fmt.Errorf("--device must look like utun123")
	}
	for _, r := range cfg.Device[len("utun"):] {
		if r < '0' || r > '9' {
			return proxyInfo{}, fmt.Errorf("--device must look like utun123")
		}
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
		if ip, err := netip.ParseAddr(cfg.Gateway4); err != nil || !ip.Is4() {
			return proxyInfo{}, fmt.Errorf("--gateway must be an IPv4 address")
		}
	}
	return info, nil
}

type engineController struct {
	mu           sync.Mutex
	commands     *os.File
	responseFile *os.File
	responses    <-chan EngineControlResponse
	responseErr  <-chan error
	generation   uint64
	closeOnce    sync.Once
}

func newEngineController(commands, responseFile *os.File) *engineController {
	responseCh := make(chan EngineControlResponse, 1)
	errorCh := make(chan error, 1)
	go func() {
		decoder := json.NewDecoder(responseFile)
		for {
			var response EngineControlResponse
			if err := decoder.Decode(&response); err != nil {
				errorCh <- err
				return
			}
			responseCh <- response
		}
	}()
	return &engineController{
		commands: commands, responseFile: responseFile,
		responses: responseCh, responseErr: errorCh,
	}
}

func (c *engineController) RebindNetwork(source4 string, timeout time.Duration) (int, error) {
	if c == nil {
		return 0, fmt.Errorf("engine control channel is unavailable")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.generation++
	command := NewEngineNetworkCommand(c.generation, source4)
	if err := json.NewEncoder(c.commands).Encode(command); err != nil {
		return 0, fmt.Errorf("send engine network generation %d: %w", command.Generation, err)
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case response := <-c.responses:
		if response.Action != command.Action || response.Generation != command.Generation {
			return 0, fmt.Errorf("unexpected engine acknowledgement: action=%q generation=%d", response.Action, response.Generation)
		}
		if response.Error != "" {
			return 0, fmt.Errorf("engine rejected network generation %d: %s", response.Generation, response.Error)
		}
		return response.Closed, nil
	case err := <-c.responseErr:
		return 0, fmt.Errorf("engine control channel closed: %w", err)
	case <-timer.C:
		return 0, fmt.Errorf("timed out waiting for engine network generation %d", command.Generation)
	}
}

func (c *engineController) Close() {
	if c == nil {
		return
	}
	c.closeOnce.Do(func() {
		_ = c.commands.Close()
		_ = c.responseFile.Close()
	})
}

func (a *App) startEngine(
	cfg Config,
	info proxyInfo,
	iface4, iface6, source4 string,
	capabilities proxyCapabilities,
) (*exec.Cmd, <-chan error, *engineController, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, nil, nil, err
	}
	configRead, configWrite, err := os.Pipe()
	if err != nil {
		return nil, nil, nil, err
	}
	commandRead, commandWrite, err := os.Pipe()
	if err != nil {
		configRead.Close()
		configWrite.Close()
		return nil, nil, nil, err
	}
	responseRead, responseWrite, err := os.Pipe()
	if err != nil {
		configRead.Close()
		configWrite.Close()
		commandRead.Close()
		commandWrite.Close()
		return nil, nil, nil, err
	}
	closeAll := func() {
		configRead.Close()
		configWrite.Close()
		commandRead.Close()
		commandWrite.Close()
		responseRead.Close()
		responseWrite.Close()
	}

	engineIface := iface4
	if info.Loopback {
		engineIface = ""
	}
	engineCfg := EngineConfig{
		Proxy:            cfg.Proxy,
		Device:           cfg.Device,
		Interface:        engineIface,
		DirectInterface:  iface4,
		DirectInterface6: iface6,
		DirectSource4:    source4,
		Applications:     append([]string(nil), cfg.Applications...),
		ProxyUDP:         capabilities.UDP,
		TrustedDNS:       cfg.TrustedDNS,
		MTU:              cfg.MTU,
		LogLevel:         cfg.LogLevel,
	}
	cmd := exec.Command(exe, "__engine")
	cmd.ExtraFiles = []*os.File{configRead, commandRead, responseWrite}
	cmd.Stdout = a.out
	cmd.Stderr = a.errOut
	if err := cmd.Start(); err != nil {
		closeAll()
		return nil, nil, nil, fmt.Errorf("start TUN engine: %w", err)
	}
	configRead.Close()
	commandRead.Close()
	responseWrite.Close()
	if err := json.NewEncoder(configWrite).Encode(engineCfg); err != nil {
		configWrite.Close()
		commandWrite.Close()
		responseRead.Close()
		_ = cmd.Process.Kill()
		return nil, nil, nil, err
	}
	if err := configWrite.Close(); err != nil {
		commandWrite.Close()
		responseRead.Close()
		_ = cmd.Process.Kill()
		return nil, nil, nil, err
	}
	control := newEngineController(commandWrite, responseRead)
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	return cmd, waitCh, control, nil
}

func waitForInterface(name string, child <-chan error, timeout time.Duration) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case err := <-child:
			if err == nil {
				return fmt.Errorf("TUN engine exited before %s appeared", name)
			}
			return fmt.Errorf("TUN engine failed: %w", err)
		case <-ticker.C:
			if _, err := net.InterfaceByName(name); err == nil {
				return nil
			}
		case <-deadline.C:
			return fmt.Errorf("timed out waiting for %s", name)
		}
	}
}

func waitForInterfaceGone(name string, sigCh <-chan os.Signal, timeout time.Duration) (os.Signal, error) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case sig := <-sigCh:
			return sig, nil
		default:
		}
		if _, err := net.InterfaceByName(name); err != nil {
			return nil, nil
		}
		select {
		case sig := <-sigCh:
			return sig, nil
		case <-ticker.C:
		case <-deadline.C:
			return nil, fmt.Errorf("timed out waiting for old TUN interface %s to disappear; automatic restart cancelled", name)
		}
	}
}

func bypassRoutes(prefixes []netip.Prefix, gateway4, gateway6, iface4, iface6 string) []Route {
	routes := make([]Route, 0, len(prefixes))
	for _, prefix := range prefixes {
		if prefix.Addr().Is4() {
			kind := "net"
			target := prefix.String()
			if prefix.Bits() == 32 {
				kind, target = "host", prefix.Addr().String()
			}
			routes = append(routes, Route{Family: "inet", Kind: kind, Target: target, Gateway: gateway4, Purpose: "bypass"})
			continue
		}
		kind := "net"
		target := prefix.String()
		if prefix.Bits() == 128 {
			kind, target = "host", prefix.Addr().String()
		}
		route := Route{Family: "inet6", Kind: kind, Target: target, Purpose: "bypass"}
		if gateway6 != "" {
			route.Gateway = gateway6
			route.Scope = iface6
		} else {
			route.Interface = iface6
		}
		routes = append(routes, route)
	}
	return routes
}

func directScopedRoutes(gateway4, gateway6, iface4, iface6 string, ipv6 bool) []Route {
	routes := make([]Route, 0, 4)
	for _, network := range ipv4TunNetworks {
		routes = append(routes, Route{
			Family:  "inet",
			Kind:    "net",
			Target:  network,
			Gateway: gateway4,
			Scope:   iface4,
			Purpose: "direct-scope",
		})
	}
	if ipv6 && gateway6 != "" && iface6 != "" {
		for _, network := range []string{"::/1", "8000::/1"} {
			routes = append(routes, Route{
				Family:  "inet6",
				Kind:    "net",
				Target:  network,
				Gateway: gateway6,
				Scope:   iface6,
				Purpose: "direct-scope",
			})
		}
	}
	return routes
}

func dnsRoute(server netip.Addr, device string) Route {
	if server.Is4() {
		return Route{Family: "inet", Kind: "host", Target: server.String(), Gateway: tunGateway4, Purpose: "dns"}
	}
	return Route{Family: "inet6", Kind: "host", Target: server.String(), Interface: device, Purpose: "dns"}
}

func (a *App) addAndSaveRoute(state *State, route Route) error {
	if err := addRoute(a.runner, route); err != nil {
		return err
	}
	state.Routes = append(state.Routes, route)
	return saveState(state)
}

var (
	engineTerminateGrace = 1500 * time.Millisecond
	engineKillGrace      = 500 * time.Millisecond
	engineExitPoll       = 25 * time.Millisecond
)

func (a *App) cleanup(state *State, engineProcess *os.Process) error {
	if state == nil {
		return fmt.Errorf("cleanup state is nil")
	}

	var cleanupErrs []error
	routesToClean := cleanupRoutesInSafeOrder(state)
	failedRoutes := make([]Route, 0, len(routesToClean))
	for _, route := range routesToClean {
		if err := deleteRoute(a.runner, route); err != nil && !routeAlreadyMissing(err) {
			cleanupErrs = append(cleanupErrs, fmt.Errorf("delete %s route %s: %w", route.Purpose, route.Target, err))
			failedRoutes = append(failedRoutes, route)
		}
	}
	for left, right := 0, len(failedRoutes)-1; left < right; left, right = left+1, right-1 {
		failedRoutes[left], failedRoutes[right] = failedRoutes[right], failedRoutes[left]
	}
	state.Routes = failedRoutes
	state.RouteReconcile = nil

	if err := stopEngine(state, engineProcess); err != nil {
		cleanupErrs = append(cleanupErrs, err)
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

func routeAlreadyMissing(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, syscall.ESRCH) {
		return true
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "not in table") ||
		strings.Contains(message, "no such route") ||
		strings.Contains(message, "route not found") ||
		strings.Contains(message, "no such process")
}

func routeAlreadyExists(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, syscall.EEXIST) {
		return true
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "file exists") ||
		strings.Contains(message, "already in table") ||
		strings.Contains(message, "route already exists")
}

func stopEngine(state *State, engineProcess *os.Process) error {
	pid := state.EnginePID
	process := engineProcess
	trustedHandle := process != nil
	if process != nil {
		pid = process.Pid
	}
	if pid <= 0 || !processAlive(pid) {
		state.EnginePID = 0
		return nil
	}
	exited, identityErr := engineProcessExited(pid, state, trustedHandle)
	if identityErr != nil {
		return fmt.Errorf("verify TUN engine PID %d before signaling: %w", pid, identityErr)
	}
	if exited {
		state.EnginePID = 0
		return nil
	}
	if process == nil {
		var err error
		process, err = os.FindProcess(pid)
		if err != nil {
			return fmt.Errorf("find TUN engine PID %d: %w", pid, err)
		}
	}

	termErr := process.Signal(syscall.SIGTERM)
	if termErr == nil {
		exited, waitErr := waitForProcessExit(pid, state, trustedHandle, engineTerminateGrace)
		if waitErr != nil {
			return fmt.Errorf("wait for TUN engine PID %d after SIGTERM: %w", pid, waitErr)
		}
		if exited {
			state.EnginePID = 0
			return nil
		}
	} else if exited, _ := engineProcessExited(pid, state, trustedHandle); exited {
		state.EnginePID = 0
		return nil
	}

	exited, identityErr = engineProcessExited(pid, state, trustedHandle)
	if identityErr != nil {
		return fmt.Errorf("reverify TUN engine PID %d before SIGKILL: %w", pid, identityErr)
	}
	if exited {
		state.EnginePID = 0
		return nil
	}
	killErr := process.Kill()
	exited, waitErr := waitForProcessExit(pid, state, trustedHandle, engineKillGrace)
	if waitErr != nil {
		return fmt.Errorf("wait for TUN engine PID %d after SIGKILL: %w", pid, waitErr)
	}
	if exited {
		state.EnginePID = 0
		return nil
	}
	if killErr != nil {
		return fmt.Errorf("stop TUN engine PID %d after SIGTERM failed (%v): kill: %w", pid, termErr, killErr)
	}
	return fmt.Errorf("TUN engine PID %d is still running after SIGTERM and SIGKILL", pid)
}

func engineProcessExited(pid int, state *State, trustedHandle bool) (bool, error) {
	if !processAlive(pid) {
		return true, nil
	}
	if state.EngineStartedAt.IsZero() || state.EngineCommand == "" {
		if trustedHandle {
			return false, nil
		}
		return false, fmt.Errorf("no persisted birth identity for PID %d; refusing to signal it", pid)
	}
	matches, err := matchesProcessIdentity(pid, state.EngineStartedAt, state.EngineCommand)
	if err != nil {
		if !processAlive(pid) {
			return true, nil
		}
		return false, err
	}
	return !matches, nil
}

func waitForProcessExit(pid int, state *State, trustedHandle bool, timeout time.Duration) (bool, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		exited, err := engineProcessExited(pid, state, trustedHandle)
		if exited || err != nil {
			return exited, err
		}
		time.Sleep(engineExitPoll)
	}
	return engineProcessExited(pid, state, trustedHandle)
}

func (a *App) recoverStale() error {
	if currentLockToken() == "" {
		return fmt.Errorf("recover stale state requires the mactun lock")
	}
	state, stateErr := loadState()
	if errors.Is(stateErr, os.ErrNotExist) {
		return nil
	}
	if stateErr != nil {
		return stateErr
	}
	if processAlive(state.OwnerPID) {
		matches, identityErr := matchesProcessIdentity(state.OwnerPID, state.OwnerStartedAt, state.OwnerCommand)
		if identityErr != nil {
			if processAlive(state.OwnerPID) {
				return fmt.Errorf("cannot prove stale owner PID %d is safe to replace: %w", state.OwnerPID, identityErr)
			}
		} else if matches {
			return fmt.Errorf("mactun state belongs to a still-running owner PID %d; refusing concurrent recovery", state.OwnerPID)
		}
	}
	fmt.Fprintln(a.errOut, "recovering stale TUN state from a previous interrupted run")
	if err := a.cleanup(state, nil); err != nil {
		return fmt.Errorf("recover stale TUN state: %w", err)
	}
	return nil
}

func (a *App) Down() error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("TUN setup is supported on macOS only")
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf("administrator privileges are required; run with sudo")
	}
	if err := acquireLock(); err == nil {
		defer releaseLock()
		return a.downWhileLocked(false)
	} else if !errors.Is(err, errLockHeld) {
		return err
	}

	state, err := a.stateForLockedOwner(500 * time.Millisecond)
	if err != nil {
		return err
	}
	if state.OwnerPID == os.Getpid() {
		return fmt.Errorf("refusing to signal the current process while it owns the mactun lock")
	}
	matches, identityErr := matchesProcessIdentity(state.OwnerPID, state.OwnerStartedAt, state.OwnerCommand)
	if identityErr != nil {
		if !processAlive(state.OwnerPID) {
			return a.takeLockAndFinishDown(4*time.Second, true)
		}
		return fmt.Errorf("verify mactun owner PID %d before signaling: %w", state.OwnerPID, identityErr)
	}
	if !matches {
		return fmt.Errorf("mactun owner PID %d birth identity changed; refusing to signal a reused PID", state.OwnerPID)
	}
	lockOwnerPID, lockToken := lockRecord()
	if lockOwnerPID != state.OwnerPID || lockToken == "" || lockToken != state.OwnerToken {
		return fmt.Errorf("mactun lock owner changed while preparing to stop; refusing to signal PID %d", state.OwnerPID)
	}
	process, findErr := os.FindProcess(state.OwnerPID)
	if findErr != nil {
		return fmt.Errorf("find mactun owner PID %d: %w", state.OwnerPID, findErr)
	}
	if signalErr := process.Signal(syscall.SIGTERM); signalErr != nil {
		if processAlive(state.OwnerPID) {
			return fmt.Errorf("signal mactun owner PID %d: %w", state.OwnerPID, signalErr)
		}
	}
	return a.takeLockAndFinishDown(4*time.Second, true)
}

func (a *App) stateForLockedOwner(timeout time.Duration) (*State, error) {
	deadline := time.Now().Add(timeout)
	for {
		state, stateErr := loadState()
		lockOwnerPID, lockToken := lockRecord()
		if stateErr == nil && lockOwnerPID > 0 && lockToken != "" &&
			state.OwnerPID == lockOwnerPID && state.OwnerToken == lockToken {
			return state, nil
		}
		if stateErr != nil && !errors.Is(stateErr, os.ErrNotExist) {
			return nil, stateErr
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("mactun lock/state owner identity is inconsistent; refusing to signal or clean up concurrently")
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func (a *App) takeLockAndFinishDown(timeout time.Duration, ownerWasSignaled bool) error {
	deadline := time.Now().Add(timeout)
	for {
		if err := acquireLock(); err == nil {
			defer releaseLock()
			return a.downWhileLocked(ownerWasSignaled)
		} else if !errors.Is(err, errLockHeld) {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("mactun owner did not release the cleanup lock; state was retained")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (a *App) downWhileLocked(ownerWasSignaled bool) error {
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
		if identityErr != nil {
			if processAlive(state.OwnerPID) {
				return fmt.Errorf("cannot prove owner PID %d is stale: %w", state.OwnerPID, identityErr)
			}
		} else if matches {
			return fmt.Errorf("owner PID %d is still running without the expected lock; refusing concurrent cleanup", state.OwnerPID)
		}
	}
	if err := a.cleanup(state, nil); err != nil {
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
	status := "stale"
	active, statusDetail := activeStateIdentity(state)
	if active {
		status = "active"
	}
	fmt.Fprintf(a.out, "status: %s\n", status)
	if statusDetail != "" {
		fmt.Fprintf(a.out, "status detail: %s\n", statusDetail)
	}
	fmt.Fprintf(a.out, "phase: %s\n", state.Phase)
	fmt.Fprintf(a.out, "proxy: %s\n", state.Proxy)
	fmt.Fprintf(a.out, "device: %s\n", state.Device)
	fmt.Fprintf(a.out, "physical interface: %s\n", state.Interface)
	fmt.Fprintf(a.out, "owner PID: %d\nengine PID: %d\n", state.OwnerPID, state.EnginePID)
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

func activeStateIdentity(state *State) (bool, string) {
	if state.Phase != "active" {
		return false, fmt.Sprintf("state phase is %s", state.Phase)
	}
	lockOwnerPID, lockToken := lockRecord()
	if state.OwnerToken == "" || lockToken == "" || lockOwnerPID != state.OwnerPID || lockToken != state.OwnerToken {
		return false, "lock owner does not match the persisted session"
	}
	ownerMatches, err := matchesProcessIdentity(state.OwnerPID, state.OwnerStartedAt, state.OwnerCommand)
	if err != nil {
		return false, fmt.Sprintf("owner identity check failed: %v", err)
	}
	if !ownerMatches {
		return false, "owner PID birth identity changed"
	}
	engineMatches, err := matchesProcessIdentity(state.EnginePID, state.EngineStartedAt, state.EngineCommand)
	if err != nil {
		return false, fmt.Sprintf("engine identity check failed: %v", err)
	}
	if !engineMatches {
		return false, "engine PID birth identity changed"
	}
	return true, ""
}

func (a *App) Doctor(proxy string) error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("mactun supports macOS only")
	}
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
		fmt.Fprintf(a.out, "OK: %s carries SOCKS5 TCP\n", redactProxy(proxy))
		fmt.Fprintf(a.out, "warning: UDP data failed; per-app mode will keep selected app-owned DNS available over TCP and block other selected-app UDP for TCP fallback: %s\n", capabilities.UDPWarning)
	}
	if info.Loopback {
		peers := discoverProxyPeers(a.runner, info.Port)
		if len(peers) == 0 {
			fmt.Fprintln(a.out, "note: no active remote peer was found; use --bypass with mactun up if needed")
		} else {
			fmt.Fprintf(a.out, "active remote peers available for auto-bypass: %s\n", strings.Join(peers, ", "))
		}
	}
	return nil
}
