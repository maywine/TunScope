package tunscope

import "time"

const (
	stateVersion = 1
	tunGateway4  = "198.18.0.1"
	tunAddress6  = "fd7a:6d61:6374:756e::1"
)

type Config struct {
	Proxy        string   `json:"proxy"`
	Device       string   `json:"device"`
	Interface    string   `json:"interface,omitempty"`
	Gateway4     string   `json:"gateway4,omitempty"`
	Bypass       []string `json:"bypass,omitempty"`
	Applications []string `json:"applications,omitempty"`
	MTU          int      `json:"mtu"`
	LogLevel     string   `json:"logLevel"`
	AutoBypass   bool     `json:"autoBypass"`
	IPv6         bool     `json:"ipv6"`
	TCPOnly      bool     `json:"tcpOnly,omitempty"`
	TrustedDNS   string   `json:"trustedDNS,omitempty"`
}

func DefaultConfig() Config {
	return Config{
		Device:     defaultDeviceName(),
		MTU:        1500,
		LogLevel:   "info",
		AutoBypass: false,
		IPv6:       true,
	}
}

type EngineConfig struct {
	Proxy            string   `json:"proxy"`
	Device           string   `json:"device"`
	Interface        string   `json:"interface,omitempty"`
	DirectInterface  string   `json:"directInterface,omitempty"`
	DirectInterface6 string   `json:"directInterface6,omitempty"`
	DirectSource4    string   `json:"directSource4,omitempty"`
	Applications     []string `json:"applications,omitempty"`
	ProxyUDP         bool     `json:"proxyUDP"`
	TrustedDNS       string   `json:"trustedDNS,omitempty"`
	MTU              int      `json:"mtu"`
	LogLevel         string   `json:"logLevel"`
}

const engineActionRebindNetwork = "rebind-network"

// EngineControlCommand and EngineControlResponse are exchanged over private
// inherited pipes. Unlike an asynchronous signal, the response proves that
// stale egress flows were closed before the supervisor commits the handoff.
type EngineControlCommand struct {
	Action     string `json:"action"`
	Generation uint64 `json:"generation"`
	Source4    string `json:"source4,omitempty"`
}

type EngineControlResponse struct {
	Action     string `json:"action"`
	Generation uint64 `json:"generation"`
	Closed     int    `json:"closed"`
	Error      string `json:"error,omitempty"`
}

func NewEngineNetworkCommand(generation uint64, source4 string) EngineControlCommand {
	return EngineControlCommand{Action: engineActionRebindNetwork, Generation: generation, Source4: source4}
}

func (c EngineControlCommand) IsNetworkRebind() bool {
	return c.Action == engineActionRebindNetwork
}

type Route struct {
	Family    string `json:"family"`
	Kind      string `json:"kind"`
	Target    string `json:"target"`
	Gateway   string `json:"gateway,omitempty"`
	Interface string `json:"interface,omitempty"`
	Scope     string `json:"scope,omitempty"`
	Source    string `json:"source,omitempty"`
	Purpose   string `json:"purpose"`
}

// RouteReconcileJournal is a write-ahead cleanup ledger for a live physical
// network change. A route change may leave either the old or the new kernel
// route installed if the owner is interrupted between the routing command and
// the next state-file rename. Stale recovery therefore treats both sets as
// cleanup candidates until the reconciliation commits.
type RouteReconcileJournal struct {
	Before []Route `json:"before,omitempty"`
	After  []Route `json:"after,omitempty"`
}

type State struct {
	Version         int                    `json:"version"`
	Phase           string                 `json:"phase"`
	OwnerPID        int                    `json:"ownerPid"`
	OwnerToken      string                 `json:"ownerToken,omitempty"`
	OwnerStartedAt  time.Time              `json:"ownerStartedAt,omitempty"`
	OwnerCommand    string                 `json:"ownerCommand,omitempty"`
	StopEvent       string                 `json:"stopEvent,omitempty"`
	EnginePID       int                    `json:"enginePid,omitempty"`
	EngineStartedAt time.Time              `json:"engineStartedAt,omitempty"`
	EngineCommand   string                 `json:"engineCommand,omitempty"`
	StartedAt       time.Time              `json:"startedAt"`
	Proxy           string                 `json:"proxy"`
	Device          string                 `json:"device"`
	DeviceIndex     int                    `json:"deviceIndex,omitempty"`
	Interface       string                 `json:"interface"`
	Interface6      string                 `json:"interface6,omitempty"`
	PhysicalIPv4    []string               `json:"physicalIPv4,omitempty"`
	PhysicalIPv6    []string               `json:"physicalIPv6,omitempty"`
	Gateway4        string                 `json:"gateway4"`
	Gateway6        string                 `json:"gateway6,omitempty"`
	Routes          []Route                `json:"routes"`
	RouteReconcile  *RouteReconcileJournal `json:"routeReconcile,omitempty"`
	AutoBypasses    []string               `json:"autoBypasses,omitempty"`
	Applications    []string               `json:"applications,omitempty"`
}
