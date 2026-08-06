//go:build !darwin && !windows

package mactun

func defaultDeviceName() string { return "tun0" }
