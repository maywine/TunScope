//go:build windows

package tunscope

import (
	"fmt"
	"net"
	"strings"
	"testing"
)

type windowsNetworkScriptTestRunner struct {
	setup string
}

func (r windowsNetworkScriptTestRunner) Run(name string, args ...string) (string, error) {
	args = append([]string(nil), args...)
	args[len(args)-1] = r.setup + "\n" + args[len(args)-1]
	return (execRunner{}).Run(name, args...)
}

const windowsNetworkTestSetup = `
$testInterfaces=@(
    [pscustomobject]@{InterfaceIndex=12;ConnectionState='Connected'},
    [pscustomobject]@{InterfaceIndex=17;ConnectionState='Connected'}
)
$testRoutes=@(
    [pscustomobject]@{InterfaceIndex=12;InterfaceAlias='Ethernet';NextHop='192.0.2.1';RouteMetric=0;InterfaceMetric=5},
    [pscustomobject]@{InterfaceIndex=17;InterfaceAlias='WLAN';NextHop='198.51.100.1';RouteMetric=0;InterfaceMetric=35}
)
$testAddresses=@(
    [pscustomobject]@{InterfaceIndex=12;IPAddress='192.0.2.10';PrefixOrigin='Dhcp';AddressState='Preferred';SkipAsSource=$false},
    [pscustomobject]@{InterfaceIndex=17;IPAddress='198.51.100.20';PrefixOrigin='Dhcp';AddressState='Preferred';SkipAsSource=$false}
)
function Get-NetIPInterface {
    param($AddressFamily, $ConnectionState)
    $testInterfaces | Where-Object { -not $ConnectionState -or $_.ConnectionState -eq $ConnectionState }
}
function Get-NetRoute {
    param($PolicyStore, $AddressFamily, $DestinationPrefix)
    $testRoutes
}
function Get-NetIPAddress {
    param($AddressFamily, $InterfaceIndex)
    $testAddresses | Where-Object { -not $InterfaceIndex -or $_.InterfaceIndex -eq $InterfaceIndex }
}
function Get-DnsClientServerAddress {
    param($InterfaceIndex, $AddressFamily)
    [pscustomobject]@{ServerAddresses=@('203.0.113.53')}
}
`

func TestReadWindowsPhysicalNetworkSelectsUsableRoute(t *testing.T) {
	tests := []struct {
		name      string
		setup     string
		wantIndex int
	}{
		{name: "lowest metric", wantIndex: 12},
		{name: "disconnected lower metric", setup: "$testInterfaces[0].ConnectionState='Disconnected'", wantIndex: 17},
		{name: "missing address", setup: "$testAddresses=@($testAddresses[1])", wantIndex: 17},
		{name: "tentative address", setup: "$testAddresses[0].AddressState='Tentative'", wantIndex: 17},
		{name: "deprecated address", setup: "$testAddresses[0].AddressState='Deprecated'", wantIndex: 17},
		{name: "skip as source", setup: "$testAddresses[0].SkipAsSource=$true", wantIndex: 17},
		{name: "unspecified address", setup: "$testAddresses[0].IPAddress='0.0.0.0'", wantIndex: 17},
		{name: "loopback address", setup: "$testAddresses[0].IPAddress='127.0.0.2'", wantIndex: 17},
		{name: "link local address", setup: "$testAddresses[0].IPAddress='169.254.1.2'", wantIndex: 17},
		{name: "well known address", setup: "$testAddresses[0].PrefixOrigin='WellKnown'", wantIndex: 17},
		{name: "combined metric", setup: "$testRoutes[0].RouteMetric=40", wantIndex: 17},
		{name: "no usable addresses", setup: "$testAddresses=@()"},
		{name: "no connected interfaces", setup: "$testInterfaces=@()"},
		{name: "no default routes", setup: "$testRoutes=@()"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runner := windowsNetworkScriptTestRunner{setup: windowsNetworkTestSetup + "\n" + tt.setup}
			got, err := readWindowsPhysicalNetwork(runner, "", false)
			if tt.wantIndex == 0 {
				if err == nil || !strings.Contains(err.Error(), "no connected physical IPv4 default route with a usable source address found") {
					t.Fatalf("snapshot=%+v error=%v, want no usable route", got, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			wantAlias, wantGateway, wantSource := "Ethernet", "192.0.2.1", "192.0.2.10"
			if tt.wantIndex == 17 {
				wantAlias, wantGateway, wantSource = "WLAN", "198.51.100.1", "198.51.100.20"
			}
			if got.InterfaceIndex != tt.wantIndex || got.InterfaceAlias != wantAlias || got.Gateway4 != wantGateway || got.Source4 != wantSource {
				t.Fatalf("snapshot=%+v, want %s index=%d gateway=%s source=%s", got, wantAlias, tt.wantIndex, wantGateway, wantSource)
			}
		})
	}
}

func TestReadWindowsPhysicalNetworkHonorsRequestedInterface(t *testing.T) {
	interfaces, err := net.Interfaces()
	if err != nil {
		t.Fatal(err)
	}
	if len(interfaces) == 0 {
		t.Fatal("no interface available to test explicit selection")
	}
	wanted := interfaces[0]
	setup := windowsNetworkTestSetup + fmt.Sprintf(`
$testInterfaces[1].InterfaceIndex=%d
$testRoutes[1].InterfaceIndex=%d
$testAddresses[1].InterfaceIndex=%d
$testInterfaces[0].InterfaceIndex=%d
$testRoutes[0].InterfaceIndex=%d
$testAddresses[0].InterfaceIndex=%d
`, wanted.Index, wanted.Index, wanted.Index, wanted.Index+1, wanted.Index+1, wanted.Index+1)
	for _, disconnected := range []bool{false, true} {
		t.Run(fmt.Sprintf("disconnected=%t", disconnected), func(t *testing.T) {
			runner := windowsNetworkScriptTestRunner{setup: setup}
			if disconnected {
				runner.setup += "\n$testInterfaces[1].ConnectionState='Disconnected'"
			}
			got, err := readWindowsPhysicalNetwork(runner, wanted.Name, false)
			if disconnected {
				if err == nil {
					t.Fatalf("selected another route for disconnected requested interface: %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.InterfaceIndex != wanted.Index || got.Source4 != "198.51.100.20" {
				t.Fatalf("snapshot=%+v, want requested interface index=%d", got, wanted.Index)
			}
		})
	}
}
