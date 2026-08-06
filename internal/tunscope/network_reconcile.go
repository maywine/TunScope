//go:build darwin

package tunscope

import (
	"errors"
	"fmt"
	"net/netip"
	"sort"
	"strings"
	"time"
)

const (
	networkStableSampleCount = 3
	addressStableSampleCount = 2
	proxyPeerPollEvery       = 4
	networkUnavailableGrace  = 30 * time.Second
)

var networkPollInterval = 750 * time.Millisecond

type physicalRouteSnapshot struct {
	Gateway4   string
	Interface  string
	Gateway6   string
	Interface6 string
	Source4    string
	IPv4       []string
	IPv6       []string
}

// physicalNetworkChangeError tells the owner that physical routes have already
// been reconciled and the engine's old TCP/UDP flows must now be rebound. The
// owner deliberately keeps the TUN interface and broad capture routes up.
type physicalNetworkChangeError struct {
	Description string
	Source4     string
}

func (e *physicalNetworkChangeError) Error() string {
	return e.Description
}

// physicalRouteUnavailableError marks sampling failures that are expected
// while macOS is disassociating from one Wi-Fi network and acquiring another.
// There is no useful route to restore during this interval, so the owner keeps
// the TUN data plane alive and retries until a complete physical snapshot is
// available again.
type physicalRouteUnavailableError struct {
	err error
}

func (e *physicalRouteUnavailableError) Error() string {
	return e.err.Error()
}

func (e *physicalRouteUnavailableError) Unwrap() error {
	return e.err
}

func physicalAddressesChanged(before, after physicalRouteSnapshot) bool {
	// Only the primary address XNU actually chooses for the IPv4 default route
	// is relevant. Secondary aliases and IPv6 privacy-address rotation must not
	// restart the whole data plane.
	return before.Source4 != "" && after.Source4 != "" && before.Source4 != after.Source4
}

func (s physicalRouteSnapshot) signature() string {
	return strings.Join([]string{
		s.Gateway4,
		s.Interface,
		s.Gateway6,
		s.Interface6,
		s.Source4,
	}, "\x00")
}

func samplePhysicalRoute(r commandRunner, includeIPv6 bool) (physicalRouteSnapshot, error) {
	gateway4, iface4, err := defaultRoute4(r)
	if err != nil {
		return physicalRouteSnapshot{}, &physicalRouteUnavailableError{
			err: fmt.Errorf("detect default IPv4 route: %w", err),
		}
	}
	if strings.HasPrefix(iface4, "utun") {
		return physicalRouteSnapshot{}, fmt.Errorf("default IPv4 route uses TUN interface %s", iface4)
	}
	if addr, parseErr := netip.ParseAddr(gateway4); parseErr != nil || !addr.Is4() {
		return physicalRouteSnapshot{}, &physicalRouteUnavailableError{
			err: fmt.Errorf("default IPv4 gateway %q is not a usable IPv4 address", gateway4),
		}
	}

	snapshot := physicalRouteSnapshot{Gateway4: gateway4, Interface: iface4}
	if !includeIPv6 {
		snapshot, err = samplePhysicalAddresses(r, snapshot, false)
		if err != nil {
			return physicalRouteSnapshot{}, &physicalRouteUnavailableError{err: err}
		}
		return snapshot, nil
	}
	gateway6, iface6, err := defaultRoute6(r)
	if err != nil {
		return physicalRouteSnapshot{}, &physicalRouteUnavailableError{
			err: fmt.Errorf("detect default IPv6 route: %w", err),
		}
	}
	if strings.HasPrefix(iface6, "utun") {
		return physicalRouteSnapshot{}, fmt.Errorf("default IPv6 route uses TUN interface %s", iface6)
	}
	snapshot.Gateway6 = gateway6
	snapshot.Interface6 = iface6
	snapshot, err = samplePhysicalAddresses(r, snapshot, true)
	if err != nil {
		return physicalRouteSnapshot{}, &physicalRouteUnavailableError{err: err}
	}
	return snapshot, nil
}

func samplePhysicalAddresses(r commandRunner, snapshot physicalRouteSnapshot, includeIPv6 bool) (physicalRouteSnapshot, error) {
	ipv4, ipv6OnV4Interface, err := interfaceAddresses(r, snapshot.Interface)
	if err != nil {
		return physicalRouteSnapshot{}, fmt.Errorf("read addresses on %s: %w", snapshot.Interface, err)
	}
	if len(ipv4) == 0 {
		return physicalRouteSnapshot{}, fmt.Errorf("physical interface %s has no usable IPv4 address", snapshot.Interface)
	}
	snapshot.IPv4 = ipv4
	source4, err := primaryInterfaceIPv4(r, snapshot.Interface, ipv4)
	if err != nil {
		return physicalRouteSnapshot{}, err
	}
	snapshot.Source4 = source4
	if !includeIPv6 {
		snapshot.IPv6 = nil
		return snapshot, nil
	}
	if snapshot.Interface6 == snapshot.Interface {
		snapshot.IPv6 = ipv6OnV4Interface
	} else {
		_, ipv6, err := interfaceAddresses(r, snapshot.Interface6)
		if err != nil {
			return physicalRouteSnapshot{}, fmt.Errorf("read addresses on %s: %w", snapshot.Interface6, err)
		}
		snapshot.IPv6 = ipv6
	}
	if len(snapshot.IPv6) == 0 {
		return physicalRouteSnapshot{}, fmt.Errorf("physical interface %s has no usable IPv6 address", snapshot.Interface6)
	}
	return snapshot, nil
}

func primaryInterfaceIPv4(r commandRunner, iface string, addresses []string) (string, error) {
	out, err := r.Run("/usr/sbin/ipconfig", "getifaddr", iface)
	if err != nil {
		return "", fmt.Errorf("read primary IPv4 address on %s: %w", iface, err)
	}
	value := strings.TrimSpace(out)
	addr, parseErr := netip.ParseAddr(value)
	if parseErr != nil || !addr.Is4() || addr.IsUnspecified() || addr.IsLoopback() || addr.IsLinkLocalUnicast() {
		return "", fmt.Errorf("primary IPv4 address on %s is not usable: %q", iface, value)
	}
	want := addr.Unmap().String()
	for _, candidate := range addresses {
		if candidate == want {
			return want, nil
		}
	}
	return "", fmt.Errorf("primary IPv4 address %s is not assigned to %s", want, iface)
}

func interfaceAddresses(r commandRunner, iface string) ([]string, []string, error) {
	out, err := r.Run("/sbin/ifconfig", iface)
	if err != nil {
		return nil, nil, err
	}
	seen4 := make(map[string]struct{})
	seen6 := make(map[string]struct{})
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || (fields[0] != "inet" && fields[0] != "inet6") {
			continue
		}
		addr, parseErr := netip.ParseAddr(fields[1])
		if parseErr != nil || addr.IsUnspecified() || addr.IsLoopback() || addr.IsMulticast() {
			continue
		}
		if addr.Is4() {
			seen4[addr.Unmap().String()] = struct{}{}
		} else {
			seen6[addr.String()] = struct{}{}
		}
	}
	ipv4 := make([]string, 0, len(seen4))
	for addr := range seen4 {
		ipv4 = append(ipv4, addr)
	}
	ipv6 := make([]string, 0, len(seen6))
	for addr := range seen6 {
		ipv6 = append(ipv6, addr)
	}
	sort.Strings(ipv4)
	sort.Strings(ipv6)
	return ipv4, ipv6, nil
}

// stableObservation keeps transient route/DNS/peer snapshots from causing
// routing mutations. Its applied signature is advanced only after the caller
// commits the corresponding route/state transaction.
type stableObservation struct {
	threshold int
	applied   string
	candidate string
	count     int
}

func newStableObservation(threshold int, applied string) stableObservation {
	if threshold < 1 {
		threshold = 1
	}
	return stableObservation{threshold: threshold, applied: applied}
}

func (s *stableObservation) observe(signature string) bool {
	if signature == s.applied {
		s.candidate = ""
		s.count = 0
		return false
	}
	if signature != s.candidate {
		s.candidate = signature
		s.count = 1
	} else {
		s.count++
	}
	return s.count >= s.threshold
}

func (s *stableObservation) commit(signature string) {
	s.applied = signature
	s.candidate = ""
	s.count = 0
}

func (s *stableObservation) resetCandidate() {
	s.candidate = ""
	s.count = 0
}

func observationSignature(value string, err error) string {
	if err != nil {
		return "error\x00" + err.Error()
	}
	return "value\x00" + value
}

func addrSetSignature(addrs []netip.Addr) string {
	values := make([]string, 0, len(addrs))
	for _, addr := range addrs {
		values = append(values, addr.String())
	}
	sort.Strings(values)
	return strings.Join(values, "\x00")
}

func stringSetSignature(values []string) string {
	copyValues := append([]string(nil), values...)
	sort.Strings(copyValues)
	return strings.Join(copyValues, "\x00")
}

func mergeBypassPrefixes(base []netip.Prefix, peers []string) ([]netip.Prefix, error) {
	values := make([]string, 0, len(base)+len(peers))
	for _, prefix := range base {
		values = append(values, prefix.String())
	}
	values = append(values, peers...)
	return resolveBypasses(values)
}

func managedPhysicalPurpose(purpose string) bool {
	switch purpose {
	case "bypass", "direct-scope", "dns-direct":
		return true
	default:
		return false
	}
}

func physicalRoutesForSnapshot(cfg Config, snapshot physicalRouteSnapshot, bypasses []netip.Prefix, dnsServers []netip.Addr) []Route {
	routes := bypassRoutes(bypasses, snapshot.Gateway4, snapshot.Gateway6, snapshot.Interface, snapshot.Interface6)
	if len(cfg.Applications) == 0 {
		return routesWithPhysicalSources(routes, snapshot)
	}
	routes = append(routes, directScopedRoutes(
		snapshot.Gateway4,
		snapshot.Gateway6,
		snapshot.Interface,
		snapshot.Interface6,
		cfg.IPv6,
	)...)
	if strings.TrimSpace(cfg.TrustedDNS) != "" {
		return routesWithPhysicalSources(routes, snapshot)
	}
	for _, server := range routedSystemDNSServers(dnsServers, cfg.IPv6) {
		routes = append(routes, directDNSRoute(
			server,
			snapshot.Gateway4,
			snapshot.Gateway6,
			snapshot.Interface6,
		))
	}
	return routesWithPhysicalSources(routes, snapshot)
}

func routesWithPhysicalSources(routes []Route, snapshot physicalRouteSnapshot) []Route {
	result := make([]Route, len(routes))
	copy(result, routes)
	for i := range result {
		if result[i].Gateway == "" {
			continue
		}
		switch result[i].Family {
		case "inet":
			if snapshot.Source4 != "" {
				result[i].Source = snapshot.Source4
			}
		}
	}
	return result
}

type routeChange struct {
	before Route
	after  Route
}

type routeReconcilePlan struct {
	changes []routeChange
	adds    []Route
	deletes []Route
}

func routeLogicalKey(route Route) string {
	return strings.Join([]string{route.Purpose, route.Family, route.Kind, route.Target}, "\x00")
}

func routeExactKey(route Route) string {
	return strings.Join([]string{
		route.Purpose,
		route.Family,
		route.Kind,
		route.Target,
		route.Gateway,
		route.Interface,
		route.Scope,
		route.Source,
	}, "\x00")
}

func routesChangeCompatible(before, after Route) bool {
	return before.Family == after.Family &&
		before.Kind == after.Kind &&
		before.Target == after.Target &&
		before.Purpose == after.Purpose &&
		before.Interface == after.Interface &&
		before.Scope == after.Scope
}

func planRouteReconcile(before, after []Route) (routeReconcilePlan, error) {
	beforeByKey := make(map[string]Route, len(before))
	for _, route := range before {
		key := routeLogicalKey(route)
		if _, exists := beforeByKey[key]; exists {
			return routeReconcilePlan{}, fmt.Errorf("duplicate existing managed route %s", route.Target)
		}
		beforeByKey[key] = route
	}
	afterByKey := make(map[string]Route, len(after))
	for _, route := range after {
		key := routeLogicalKey(route)
		if _, exists := afterByKey[key]; exists {
			return routeReconcilePlan{}, fmt.Errorf("duplicate desired managed route %s", route.Target)
		}
		afterByKey[key] = route
	}

	var plan routeReconcilePlan
	for key, oldRoute := range beforeByKey {
		newRoute, remains := afterByKey[key]
		if !remains {
			plan.deletes = append(plan.deletes, oldRoute)
			continue
		}
		if routeExactKey(oldRoute) == routeExactKey(newRoute) {
			continue
		}
		if !routesChangeCompatible(oldRoute, newRoute) {
			return routeReconcilePlan{}, fmt.Errorf("route %s cannot be changed safely in place", oldRoute.Target)
		}
		plan.changes = append(plan.changes, routeChange{before: oldRoute, after: newRoute})
	}
	for key, newRoute := range afterByKey {
		if _, existed := beforeByKey[key]; !existed {
			plan.adds = append(plan.adds, newRoute)
		}
	}

	sort.Slice(plan.changes, func(i, j int) bool {
		return routeMutationLess(plan.changes[i].after, plan.changes[j].after)
	})
	sort.Slice(plan.adds, func(i, j int) bool { return routeMutationLess(plan.adds[i], plan.adds[j]) })
	sort.Slice(plan.deletes, func(i, j int) bool { return routeMutationLess(plan.deletes[i], plan.deletes[j]) })
	return plan, nil
}

func forceRefreshMatchingRoutes(plan *routeReconcilePlan, before, after []Route) error {
	afterByKey := make(map[string]Route, len(after))
	for _, route := range after {
		afterByKey[routeLogicalKey(route)] = route
	}
	alreadyChanged := make(map[string]struct{}, len(plan.changes))
	for _, change := range plan.changes {
		alreadyChanged[routeLogicalKey(change.after)] = struct{}{}
	}
	for _, oldRoute := range before {
		key := routeLogicalKey(oldRoute)
		if _, exists := alreadyChanged[key]; exists {
			continue
		}
		newRoute, remains := afterByKey[key]
		if !remains {
			continue
		}
		if !routesChangeCompatible(oldRoute, newRoute) {
			return fmt.Errorf("route %s cannot be refreshed safely in place", oldRoute.Target)
		}
		plan.changes = append(plan.changes, routeChange{before: oldRoute, after: newRoute})
	}
	sort.Slice(plan.changes, func(i, j int) bool {
		return routeMutationLess(plan.changes[i].after, plan.changes[j].after)
	})
	return nil
}

func routeMutationLess(left, right Route) bool {
	priority := func(purpose string) int {
		switch purpose {
		case "direct-scope":
			return 0
		case "bypass":
			return 1
		case "dns-direct":
			return 2
		default:
			return 3
		}
	}
	leftPriority, rightPriority := priority(left.Purpose), priority(right.Purpose)
	if leftPriority != rightPriority {
		return leftPriority < rightPriority
	}
	return routeLogicalKey(left) < routeLogicalKey(right)
}

func managedPhysicalRoutes(routes []Route) []Route {
	managed := make([]Route, 0)
	for _, route := range routes {
		if managedPhysicalPurpose(route.Purpose) {
			managed = append(managed, route)
		}
	}
	return managed
}

func replaceManagedPhysicalRoutes(routes, desired []Route) []Route {
	result := make([]Route, 0, len(routes)+len(desired))
	for _, route := range routes {
		if !managedPhysicalPurpose(route.Purpose) {
			result = append(result, route)
		}
	}
	return append(result, desired...)
}

// reconcilePhysicalRoutes updates only routes that depend on the current
// physical network. The write-ahead journal is persisted before the first
// routing command. Any error is fatal to the active session; the caller's
// normal cleanup then removes both journal sides and stops the engine.
func (a *App) reconcilePhysicalRoutes(
	state *State,
	cfg Config,
	snapshot physicalRouteSnapshot,
	bypasses []netip.Prefix,
	dnsServers []netip.Addr,
	autoPeers []string,
) error {
	return a.reconcilePhysicalRoutesWithRefresh(state, cfg, snapshot, bypasses, dnsServers, autoPeers, false)
}

func (a *App) reconcilePhysicalRoutesWithRefresh(
	state *State,
	cfg Config,
	snapshot physicalRouteSnapshot,
	bypasses []netip.Prefix,
	dnsServers []netip.Addr,
	autoPeers []string,
	forceRefresh bool,
) error {
	before := managedPhysicalRoutes(state.Routes)
	after := physicalRoutesForSnapshot(cfg, snapshot, bypasses, dnsServers)
	plan, err := planRouteReconcile(before, after)
	if err != nil {
		return err
	}
	if forceRefresh {
		if err := forceRefreshMatchingRoutes(&plan, before, after); err != nil {
			return err
		}
	}
	if len(plan.changes) == 0 && len(plan.adds) == 0 && len(plan.deletes) == 0 {
		state.Interface = snapshot.Interface
		state.Interface6 = snapshot.Interface6
		state.PhysicalIPv4 = append([]string(nil), snapshot.IPv4...)
		state.PhysicalIPv6 = append([]string(nil), snapshot.IPv6...)
		state.Gateway4 = snapshot.Gateway4
		state.Gateway6 = snapshot.Gateway6
		state.AutoBypasses = append([]string(nil), autoPeers...)
		return saveState(state)
	}

	state.Phase = "reconciling"
	state.RouteReconcile = &RouteReconcileJournal{
		Before: append([]Route(nil), before...),
		After:  append([]Route(nil), after...),
	}
	if err := saveState(state); err != nil {
		state.Phase = "active"
		state.RouteReconcile = nil
		return fmt.Errorf("save route reconciliation journal: %w", err)
	}

	for _, change := range plan.changes {
		var err error
		if change.before.Family == "inet" && change.before.Source != change.after.Source {
			err = replaceOwnedRoute(a.runner, change.before, change.after)
		} else {
			err = changeOrRestoreRoute(a.runner, change.after)
		}
		if err != nil {
			return fmt.Errorf("change %s route %s: %w", change.after.Purpose, change.after.Target, err)
		}
	}
	// New DNS and peer routes are installed before obsolete ones are removed,
	// so a stable update does not create an avoidable direct-path gap.
	for _, route := range plan.adds {
		if err := addRoute(a.runner, route); err != nil {
			return fmt.Errorf("add %s route %s: %w", route.Purpose, route.Target, err)
		}
	}
	for _, route := range plan.deletes {
		if err := deleteRoute(a.runner, route); err != nil && !routeAlreadyMissing(err) {
			return fmt.Errorf("delete obsolete %s route %s: %w", route.Purpose, route.Target, err)
		}
	}

	state.Routes = replaceManagedPhysicalRoutes(state.Routes, after)
	state.Interface = snapshot.Interface
	state.Interface6 = snapshot.Interface6
	state.PhysicalIPv4 = append([]string(nil), snapshot.IPv4...)
	state.PhysicalIPv6 = append([]string(nil), snapshot.IPv6...)
	state.Gateway4 = snapshot.Gateway4
	state.Gateway6 = snapshot.Gateway6
	state.AutoBypasses = append([]string(nil), autoPeers...)
	state.Phase = "active"
	state.RouteReconcile = nil
	if err := saveState(state); err != nil {
		return fmt.Errorf("commit reconciled routes: %w", err)
	}
	return nil
}

// changeOrRestoreRoute handles the normal macOS Wi-Fi handoff behavior where
// the kernel removes an interface-scoped route before the stable replacement
// gateway is available. Since this logical route is already in our ownership
// ledger, restoring it with add is safe. If it races with the kernel and add
// reports EEXIST, retrying change is likewise limited to an already-owned key.
func changeOrRestoreRoute(r commandRunner, desired Route) error {
	if err := changeRoute(r, desired); err != nil {
		if !routeAlreadyMissing(err) {
			return err
		}
		if addErr := addRoute(r, desired); addErr != nil {
			if routeAlreadyExists(addErr) {
				return changeRoute(r, desired)
			}
			return addErr
		}
	}
	return nil
}

// replaceOwnedRoute removes an owned physical route before adding its new
// source-address form. RTM_CHANGE can leave XNU cloned routes attached to the
// removed DHCP address; delete/add invalidates those clones. Broad TUN capture
// routes never enter the managed physical route plan and therefore stay up.
func replaceOwnedRoute(r commandRunner, before, after Route) error {
	if err := deleteRoute(r, before); err != nil && !routeAlreadyMissing(err) {
		return err
	}
	if err := addRoute(r, after); err != nil {
		if routeAlreadyExists(err) {
			return changeRoute(r, after)
		}
		return err
	}
	return nil
}

func cleanupRouteCandidates(state *State) []Route {
	candidates := append([]Route(nil), state.Routes...)
	if state.RouteReconcile != nil {
		candidates = append(candidates, state.RouteReconcile.Before...)
		candidates = append(candidates, state.RouteReconcile.After...)
	}
	seen := make(map[string]struct{}, len(candidates))
	unique := make([]Route, 0, len(candidates))
	for _, route := range candidates {
		key := routeExactKey(route)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		unique = append(unique, route)
	}
	return unique
}

// cleanupRoutesInSafeOrder removes broad TUN capture before any physical-path
// bookkeeping. If the routing socket starts failing during cleanup, this
// ordering gives unselected applications their normal default route as early
// as possible instead of leaving capture active while journal candidates are
// retried first.
func cleanupRoutesInSafeOrder(state *State) []Route {
	candidates := cleanupRouteCandidates(state)
	for left, right := 0, len(candidates)-1; left < right; left, right = left+1, right-1 {
		candidates[left], candidates[right] = candidates[right], candidates[left]
	}
	priority := func(route Route) int {
		switch route.Purpose {
		case "tun":
			return 0
		case "dns":
			return 1
		case "dns-direct":
			return 2
		case "direct-scope":
			return 3
		case "bypass":
			return 4
		default:
			return 5
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		return priority(candidates[i]) < priority(candidates[j])
	})
	return candidates
}

type liveNetworkMonitor struct {
	route               physicalRouteSnapshot
	dnsServers          []netip.Addr
	baseBypasses        []netip.Prefix
	autoPeers           []string
	includeIPv6         bool
	perApp              bool
	discoverPeers       bool
	proxyPort           int
	peerPollCount       int
	routeUnavailable    bool
	routeUnavailableAt  time.Time
	recoveryPending     bool
	dnsUnavailable      bool
	now                 func() time.Time
	routeObservation    stableObservation
	addressObservation  stableObservation
	recoveryObservation stableObservation
	dnsObservation      stableObservation
	peerObservation     stableObservation
}

func newLiveNetworkMonitor(
	route physicalRouteSnapshot,
	dnsServers []netip.Addr,
	baseBypasses []netip.Prefix,
	autoPeers []string,
	includeIPv6 bool,
	perApp bool,
	discoverPeers bool,
	proxyPort int,
) *liveNetworkMonitor {
	return &liveNetworkMonitor{
		route:         route,
		dnsServers:    append([]netip.Addr(nil), dnsServers...),
		baseBypasses:  append([]netip.Prefix(nil), baseBypasses...),
		autoPeers:     append([]string(nil), autoPeers...),
		includeIPv6:   includeIPv6,
		perApp:        perApp,
		discoverPeers: discoverPeers,
		proxyPort:     proxyPort,
		now:           time.Now,
		routeObservation: newStableObservation(
			networkStableSampleCount,
			observationSignature(route.signature(), nil),
		),
		addressObservation: newStableObservation(
			addressStableSampleCount,
			observationSignature(route.signature(), nil),
		),
		recoveryObservation: newStableObservation(addressStableSampleCount, ""),
		dnsObservation: newStableObservation(
			networkStableSampleCount,
			observationSignature(addrSetSignature(dnsServers), nil),
		),
		peerObservation: newStableObservation(
			networkStableSampleCount,
			observationSignature(stringSetSignature(autoPeers), nil),
		),
	}
}

func (m *liveNetworkMonitor) currentBypasses() ([]netip.Prefix, error) {
	return mergeBypassPrefixes(m.baseBypasses, m.autoPeers)
}

// poll samples the route, DNS, and proxy peers independently. In particular,
// an unstable peer set can never postpone a gateway repair. Each stable change
// is committed before sampling the next dependency, which restores the
// direct-scope routes first after a Wi-Fi gateway change.
func (m *liveNetworkMonitor) poll(a *App, state *State, cfg Config) ([]string, error) {
	var updates []string

	route, routeErr := samplePhysicalRoute(a.runner, m.includeIPv6)
	if routeErr != nil {
		var unavailable *physicalRouteUnavailableError
		if errors.As(routeErr, &unavailable) {
			// A Wi-Fi handoff temporarily removes the default route and often the
			// interface address as well. Do not let samples on opposite sides of
			// that gap satisfy a stability threshold, and do not tear down TUN:
			// there is no working physical route to restore yet.
			m.addressObservation.resetCandidate()
			m.routeObservation.resetCandidate()
			m.recoveryObservation.commit("")
			now := m.now()
			if !m.routeUnavailable {
				m.routeUnavailableAt = now
				updates = append(updates, fmt.Sprintf(
					"physical network temporarily unavailable; keeping TUN capture active while waiting: %v",
					unavailable,
				))
			}
			m.routeUnavailable = true
			m.recoveryPending = true
			if !m.routeUnavailableAt.IsZero() && now.Sub(m.routeUnavailableAt) >= networkUnavailableGrace {
				return updates, fmt.Errorf(
					"physical network remained unavailable for %s: %w",
					networkUnavailableGrace,
					unavailable,
				)
			}
			return updates, nil
		}
	} else if m.routeUnavailable {
		m.routeUnavailable = false
		m.routeUnavailableAt = time.Time{}
		updates = append(updates, "physical network is available again; validating the replacement route")
	}
	if routeErr == nil && m.recoveryPending {
		recoverySignature := observationSignature(route.signature(), nil)
		if !m.recoveryObservation.observe(recoverySignature) {
			return updates, nil
		}
		if route.Interface != m.route.Interface {
			return updates, fmt.Errorf("physical IPv4 interface changed from %s to %s", m.route.Interface, route.Interface)
		}
		if m.includeIPv6 && route.Interface6 != m.route.Interface6 {
			return updates, fmt.Errorf("physical IPv6 interface changed from %s to %s", m.route.Interface6, route.Interface6)
		}
		bypasses, err := m.currentBypasses()
		if err != nil {
			return updates, fmt.Errorf("resolve current bypass routes: %w", err)
		}
		if err := a.reconcilePhysicalRoutesWithRefresh(
			state,
			cfg,
			route,
			bypasses,
			m.dnsServers,
			m.autoPeers,
			true,
		); err != nil {
			return updates, err
		}
		change := &physicalNetworkChangeError{
			Description: fmt.Sprintf(
				"physical network recovered on %s: IPv4 %s -> %s, gateway %s -> %s",
				route.Interface,
				m.route.Source4,
				route.Source4,
				m.route.Gateway4,
				route.Gateway4,
			),
			Source4: route.Source4,
		}
		m.route = route
		m.recoveryPending = false
		m.recoveryObservation.commit(recoverySignature)
		m.addressObservation.commit(observationSignature(route.signature(), nil))
		m.routeObservation.commit(observationSignature(route.signature(), nil))
		return updates, change
	}
	// Source and gateway must belong to the same stable snapshot. Debouncing on
	// Source4 alone can accidentally combine a newly acquired address with a
	// gateway observed during a different DHCP transition sample.
	addressSignature := observationSignature(route.signature(), routeErr)
	if m.addressObservation.observe(addressSignature) && routeErr == nil && physicalAddressesChanged(m.route, route) {
		if route.Interface != m.route.Interface {
			return updates, fmt.Errorf("physical IPv4 interface changed from %s to %s", m.route.Interface, route.Interface)
		}
		if m.includeIPv6 && route.Interface6 != m.route.Interface6 {
			return updates, fmt.Errorf("physical IPv6 interface changed from %s to %s", m.route.Interface6, route.Interface6)
		}
		bypasses, err := m.currentBypasses()
		if err != nil {
			return updates, fmt.Errorf("resolve current bypass routes: %w", err)
		}
		if err := a.reconcilePhysicalRoutes(state, cfg, route, bypasses, m.dnsServers, m.autoPeers); err != nil {
			return updates, err
		}
		change := &physicalNetworkChangeError{
			Description: fmt.Sprintf(
				"physical addresses changed on %s: IPv4 %s -> %s, IPv6 %s -> %s",
				route.Interface,
				strings.Join(m.route.IPv4, ","),
				strings.Join(route.IPv4, ","),
				strings.Join(m.route.IPv6, ","),
				strings.Join(route.IPv6, ","),
			),
			Source4: route.Source4,
		}
		m.route = route
		m.addressObservation.commit(addressSignature)
		m.routeObservation.commit(observationSignature(route.signature(), nil))
		return updates, change
	}
	routeSignature := observationSignature(route.signature(), routeErr)
	if m.routeObservation.observe(routeSignature) {
		if routeErr != nil {
			return updates, routeErr
		}
		if route.Interface != m.route.Interface {
			return updates, fmt.Errorf("physical IPv4 interface changed from %s to %s", m.route.Interface, route.Interface)
		}
		if m.includeIPv6 && route.Interface6 != m.route.Interface6 {
			return updates, fmt.Errorf("physical IPv6 interface changed from %s to %s", m.route.Interface6, route.Interface6)
		}
		bypasses, err := m.currentBypasses()
		if err != nil {
			return updates, fmt.Errorf("resolve current bypass routes: %w", err)
		}
		if err := a.reconcilePhysicalRoutes(state, cfg, route, bypasses, m.dnsServers, m.autoPeers); err != nil {
			return updates, err
		}
		oldGateway4, oldGateway6 := m.route.Gateway4, m.route.Gateway6
		m.route = route
		m.routeObservation.commit(routeSignature)
		update := fmt.Sprintf(
			"physical network changed on %s: IPv4 gateway %s -> %s, IPv6 gateway %s -> %s",
			route.Interface,
			oldGateway4,
			route.Gateway4,
			oldGateway6,
			route.Gateway6,
		)
		updates = append(updates, update)
		return updates, &physicalNetworkChangeError{Description: update, Source4: route.Source4}
	}

	if m.perApp {
		dnsServers, dnsErr := readSystemDNSServers(a.runner)
		if dnsErr == nil && len(dnsServers) == 0 && len(m.dnsServers) > 0 {
			dnsErr = fmt.Errorf("system DNS configuration is empty")
		}
		if dnsErr != nil {
			// Dynamic-store DNS keys also disappear briefly during a Wi-Fi
			// handoff. Keep the last direct resolver routes until a complete new
			// set is available instead of stopping the entire TUN session.
			m.dnsObservation.resetCandidate()
			if !m.dnsUnavailable {
				updates = append(updates, fmt.Sprintf(
					"system DNS temporarily unavailable; retaining %d existing direct resolver route(s): %v",
					len(routedSystemDNSServers(m.dnsServers, cfg.IPv6)),
					dnsErr,
				))
			}
			m.dnsUnavailable = true
		} else {
			if m.dnsUnavailable {
				m.dnsUnavailable = false
				updates = append(updates, "system DNS is available again; validating the replacement resolver set")
			}
			dnsSignature := observationSignature(addrSetSignature(dnsServers), nil)
			if m.dnsObservation.observe(dnsSignature) {
				bypasses, err := m.currentBypasses()
				if err != nil {
					return updates, fmt.Errorf("resolve current bypass routes: %w", err)
				}
				if err := a.reconcilePhysicalRoutes(state, cfg, m.route, bypasses, dnsServers, m.autoPeers); err != nil {
					return updates, err
				}
				m.dnsServers = append([]netip.Addr(nil), dnsServers...)
				m.dnsObservation.commit(dnsSignature)
				updates = append(updates, fmt.Sprintf(
					"system DNS configuration updated for %d resolver(s); managing %d external route(s)",
					len(dnsServers),
					len(routedSystemDNSServers(dnsServers, cfg.IPv6)),
				))
			}
		}
	}

	if m.discoverPeers {
		m.peerPollCount++
		if m.peerPollCount >= proxyPeerPollEvery {
			m.peerPollCount = 0
			peers := discoverProxyPeers(a.runner, m.proxyPort)
			if len(peers) == 0 && len(m.autoPeers) > 0 {
				// lsof commonly observes an empty interval while a local proxy is
				// reconnecting. Retain the last known escape route so that the
				// reconnect itself cannot be captured by TUN.
				m.peerObservation.observe(observationSignature(stringSetSignature(m.autoPeers), nil))
				return updates, nil
			}
			peerSignature := observationSignature(stringSetSignature(peers), nil)
			if m.peerObservation.observe(peerSignature) {
				candidateBypasses, err := mergeBypassPrefixes(m.baseBypasses, peers)
				if err != nil {
					return updates, fmt.Errorf("resolve discovered proxy peers: %w", err)
				}
				if err := a.reconcilePhysicalRoutes(state, cfg, m.route, candidateBypasses, m.dnsServers, peers); err != nil {
					return updates, err
				}
				m.autoPeers = append([]string(nil), peers...)
				m.peerObservation.commit(peerSignature)
				updates = append(updates, fmt.Sprintf("proxy bypass routes updated for %d peer(s)", len(peers)))
			}
		}
	}

	return updates, nil
}
