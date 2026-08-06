//go:build darwin

package mactun

import "net/netip"

func routeForSystemDNS(server netip.Addr, perApp bool, device, gateway4, gateway6, iface6 string) Route {
	if perApp {
		return directDNSRoute(server, gateway4, gateway6, iface6)
	}
	return dnsRoute(server, device)
}

// directDNSRoute keeps the shared macOS resolver on the physical network in
// per-app mode. mDNSResponder does not retain the identity of the application
// that requested a lookup, so proxying it would also change DNS for every
// unselected application.
func directDNSRoute(server netip.Addr, gateway4, gateway6, iface6 string) Route {
	if server.Is4() {
		return Route{
			Family:  "inet",
			Kind:    "host",
			Target:  server.String(),
			Gateway: gateway4,
			Purpose: "dns-direct",
		}
	}
	route := Route{
		Family:  "inet6",
		Kind:    "host",
		Target:  server.String(),
		Purpose: "dns-direct",
	}
	if gateway6 != "" {
		route.Gateway = gateway6
		route.Scope = iface6
	} else {
		route.Interface = iface6
	}
	return route
}
