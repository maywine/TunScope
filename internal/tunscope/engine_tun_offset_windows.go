//go:build windows

package tunscope

import "golang.zx2c4.com/wireguard/tun"

const engineTUNOffset = 0

func configureNativeTUNPlatform() {
	// Preserve the adapter tunnel type created by tun2socks/engine so an
	// existing TunScope Wintun adapter is reused instead of duplicated.
	tun.WintunTunnelType = "tun2socks"
}
