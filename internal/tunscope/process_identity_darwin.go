//go:build darwin

package tunscope

import (
	"bytes"
	"fmt"
	"time"

	"golang.org/x/sys/unix"
)

func readProcessIdentity(pid int) (processIdentity, error) {
	info, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return processIdentity{}, fmt.Errorf("read process identity for PID %d: %w", pid, err)
	}
	if info == nil || int(info.Proc.P_pid) != pid {
		return processIdentity{}, fmt.Errorf("process PID %d no longer exists", pid)
	}
	commandBytes := info.Proc.P_comm[:]
	if end := bytes.IndexByte(commandBytes, 0); end >= 0 {
		commandBytes = commandBytes[:end]
	}
	identity := processIdentity{
		StartedAt: time.Unix(int64(info.Proc.P_starttime.Sec), int64(info.Proc.P_starttime.Usec)*int64(time.Microsecond)),
		Command:   string(commandBytes),
	}
	if identity.StartedAt.IsZero() || identity.Command == "" {
		return processIdentity{}, fmt.Errorf("incomplete process identity for PID %d", pid)
	}
	return identity, nil
}
