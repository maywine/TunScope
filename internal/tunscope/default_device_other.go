//go:build !darwin && !windows

package tunscope

func defaultDeviceName() string { return "tun0" }
