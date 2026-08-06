//go:build !darwin && !windows

package mactun

import "fmt"

func readProcessIdentity(pid int) (processIdentity, error) {
	return processIdentity{}, fmt.Errorf("process birth identity is unavailable for PID %d on this platform", pid)
}
