package tunscope

// ipv4TunNetworks covers the public IPv4 space without replacing the system
// default route. Keeping the original default route makes exact physical
// bypass routes possible on both Darwin and Windows.
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
