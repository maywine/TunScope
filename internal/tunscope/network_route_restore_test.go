//go:build darwin

package tunscope

import (
	"bytes"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Unlike liveMonitorRunner, this runner remembers route deletion. It models
// XNU's RTM_CHANGE fallback to the system default when an owned key is absent.
// It never executes a command or changes the machine's routing table.
type ownedRouteTableRunner struct {
	liveMonitorRunner
	routes         map[string]Route
	defaultChanges int
}

func testRouteTableKey(route Route) string {
	return strings.Join([]string{route.Family, route.Kind, route.Scope, route.Target}, "\x00")
}

func newOwnedRouteTableRunner(snapshot physicalRouteSnapshot, routes []Route) *ownedRouteTableRunner {
	r := &ownedRouteTableRunner{
		liveMonitorRunner: liveMonitorRunner{
			routeGateway: snapshot.Gateway4,
			routeIface:   snapshot.Interface,
			ipv4Address:  snapshot.Source4,
			dnsServer:    "1.1.1.1",
		},
		routes: make(map[string]Route),
	}
	for _, route := range routes {
		r.routes[testRouteTableKey(route)] = route
	}
	return r
}

func (r *ownedRouteTableRunner) Run(name string, args ...string) (string, error) {
	if name == "/sbin/route" && len(args) >= 2 {
		action := args[1]
		if action == "get" && len(args) == 6 && args[2] == "-net" && args[3] == "-ifscope" {
			r.calls = append(r.calls, strings.Join(append([]string{name}, args...), " "))
			key := testRouteTableKey(Route{Family: "inet", Kind: "net", Scope: args[4], Target: args[5]})
			lookup := r.liveMonitorRunner
			if route, exists := r.routes[key]; exists {
				lookup.scopedRouteGateway = route.Gateway
				lookup.scopedRouteIface = route.Scope
			} else {
				lookup.scopedRouteTarget = "default"
				lookup.scopedRouteMask = "default"
			}
			return lookup.Run(name, args...)
		}
		if action == "add" || action == "delete" || action == "change" {
			r.calls = append(r.calls, strings.Join(append([]string{name}, args...), " "))
			route := Route{Family: "inet"}
			for i := 2; i < len(args); i++ {
				switch args[i] {
				case "-inet6":
					route.Family = "inet6"
				case "-net", "-host":
					route.Kind = strings.TrimPrefix(args[i], "-")
				case "-ifscope", "-ifa", "-interface":
					option := args[i]
					i++
					if i == len(args) {
						return "", fmt.Errorf("missing value for %s", option)
					}
					switch option {
					case "-ifscope":
						route.Scope = args[i]
					case "-ifa":
						route.Source = args[i]
					case "-interface":
						route.Interface = args[i]
					}
				default:
					if route.Target == "" {
						route.Target = args[i]
					} else {
						route.Gateway = args[i]
					}
				}
			}
			key := testRouteTableKey(route)
			_, exists := r.routes[key]
			switch action {
			case "delete":
				if !exists {
					return "", syscall.ESRCH
				}
				delete(r.routes, key)
			case "add":
				if exists {
					return "", syscall.EEXIST
				}
				r.routes[key] = route
			case "change":
				if !exists {
					r.defaultChanges++
				} else {
					r.routes[key] = route
				}
			}
			return "", nil
		}
	}
	return r.liveMonitorRunner.Run(name, args...)
}

func TestDHCPRenewalRestoresDeletedRoutesWithoutChangingDefault(t *testing.T) {
	t.Setenv("TUNSCOPE_STATE_DIR", t.TempDir())
	cfg := reconcileTestConfig()
	snapshot := physicalRouteSnapshot{
		Gateway4: "192.168.50.1", Interface: "en0", Source4: "192.168.50.20", IPv4: []string{"192.168.50.20"},
	}
	bypasses := []netip.Prefix{netip.MustParsePrefix("203.0.113.9/32")}
	dns := []netip.Addr{netip.MustParseAddr("1.1.1.1")}
	state := reconcileTestState(cfg, snapshot, bypasses, dns)
	runner := newOwnedRouteTableRunner(snapshot, state.Routes)
	app := &App{runner: runner, out: &bytes.Buffer{}, errOut: &bytes.Buffer{}}
	monitor := newLiveNetworkMonitor(snapshot, dns, bypasses, nil, false, true, false, 0)
	now := time.Unix(1_700_000_000, 0)
	monitor.now = func() time.Time { return now }

	// Repeat a same-address lease renewal: this must recover in place every
	// time, without a clean engine restart or touching the system default.
	for renewal := 0; renewal < 2; renewal++ {
		_, err := monitor.handlePhysicalNetworkEvent("DHCP lease renewed on en0")
		requireNetworkUnavailableSignal(t, err)
		if _, err := app.suspendOwnedRoutes(state); err != nil {
			t.Fatal(err)
		}
		if len(runner.routes) != 0 {
			t.Fatalf("owned routes remain after suspension: %#v", runner.routes)
		}
		var recovered *physicalNetworkChangeError
		for poll := 0; poll < networkRecoverySampleCount+1; poll++ {
			now = now.Add(networkPollInterval)
			_, err = monitor.poll(app, state, cfg)
			if runner.defaultChanges != 0 {
				t.Fatalf("renewal %d modified the system default %d times", renewal, runner.defaultChanges)
			}
			if err != nil && !errors.As(err, &recovered) {
				t.Fatalf("renewal %d failed to recover: %v", renewal, err)
			}
			if poll < networkRecoverySampleCount && recovered != nil {
				t.Fatal("recovered before physical routes survived the verification poll")
			}
			for _, route := range state.Routes {
				if isCaptureRoute(route) {
					if _, exists := runner.routes[testRouteTableKey(route)]; exists {
						t.Fatal("capture resumed before the engine acknowledged the handoff")
					}
				}
			}
		}
		if recovered == nil || recovered.Source4 != snapshot.Source4 {
			t.Fatalf("renewal %d did not recover with the original source: %#v", renewal, recovered)
		}
		if _, err := app.resumeCaptureRoutes(state); err != nil {
			t.Fatal(err)
		}
		for _, want := range state.Routes {
			got, exists := runner.routes[testRouteTableKey(want)]
			if !exists || got.Gateway != want.Gateway || got.Source != want.Source {
				t.Fatalf("route %s was not restored: got %#v, want %#v", want.Target, got, want)
			}
		}
		if state.RoutesSuspended || state.RouteReconcile != nil {
			t.Fatalf("renewal did not commit recovery: %#v", state)
		}
	}
}

func TestGatewayChangeRestoresMissingOwnedRoutesWithoutChangingDefault(t *testing.T) {
	t.Setenv("TUNSCOPE_STATE_DIR", t.TempDir())
	cfg := reconcileTestConfig()
	cfg.IPv6 = true
	before := physicalRouteSnapshot{
		Gateway4: "192.168.50.1", Interface: "en0", Source4: "192.168.50.20", IPv4: []string{"192.168.50.20"},
		Gateway6: "fe80::1%en0", Interface6: "en0", IPv6: []string{"2001:db8:50::20"},
	}
	after := before
	after.Gateway4 = "192.168.50.254"
	after.Gateway6 = "fe80::2%en0"
	bypasses := []netip.Prefix{netip.MustParsePrefix("203.0.113.9/32"), netip.MustParsePrefix("2001:db8:123::9/128")}
	dns := []netip.Addr{netip.MustParseAddr("1.1.1.1"), netip.MustParseAddr("2001:db8:53::1")}
	state := reconcileTestState(cfg, before, bypasses, dns)
	// macOS removed physical routes without the owner suspending capture.
	var capture []Route
	for _, route := range state.Routes {
		if isCaptureRoute(route) {
			capture = append(capture, route)
		}
	}
	runner := newOwnedRouteTableRunner(after, capture)
	app := &App{runner: runner, out: &bytes.Buffer{}, errOut: &bytes.Buffer{}}
	if err := app.reconcilePhysicalRoutes(state, cfg, after, bypasses, dns, nil); err != nil {
		t.Fatal(err)
	}
	if runner.defaultChanges != 0 {
		t.Fatalf("gateway change modified the system default %d times", runner.defaultChanges)
	}
	for _, want := range state.Routes {
		got, exists := runner.routes[testRouteTableKey(want)]
		if !exists || got.Gateway != want.Gateway || got.Source != want.Source {
			t.Fatalf("route %s was not restored: got %#v, want %#v", want.Target, got, want)
		}
	}
}
