//go:build !darwin && !windows

package tunscope

func discoverProxyPeers(commandRunner, int) []string { return nil }
