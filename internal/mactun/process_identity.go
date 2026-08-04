package mactun

import (
	"fmt"
	"time"
)

type processIdentity struct {
	StartedAt time.Time
	Command   string
}

func matchesProcessIdentity(pid int, startedAt time.Time, command string) (bool, error) {
	if pid <= 0 {
		return false, nil
	}
	if startedAt.IsZero() || command == "" {
		return false, fmt.Errorf("no persisted birth identity for PID %d", pid)
	}
	identity, err := readProcessIdentity(pid)
	if err != nil {
		return false, err
	}
	return identity.StartedAt.Equal(startedAt) && identity.Command == command, nil
}
