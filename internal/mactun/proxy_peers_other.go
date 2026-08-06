//go:build !darwin && !windows

package mactun

func discoverProxyPeers(commandRunner, int) []string { return nil }
