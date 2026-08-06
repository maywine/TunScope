//go:build windows

package mactun

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/windows"
)

func readProcessIdentity(pid int) (processIdentity, error) {
	if pid <= 0 {
		return processIdentity{}, fmt.Errorf("invalid process PID %d", pid)
	}
	handle, err := windows.OpenProcess(
		windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE,
		false,
		uint32(pid),
	)
	if err != nil {
		return processIdentity{}, fmt.Errorf("open process PID %d: %w", pid, err)
	}
	defer windows.CloseHandle(handle)

	var created, exited, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(handle, &created, &exited, &kernel, &user); err != nil {
		return processIdentity{}, fmt.Errorf("read creation time for PID %d: %w", pid, err)
	}
	path, err := windowsProcessPathFromHandle(handle)
	if err != nil {
		return processIdentity{}, fmt.Errorf("read executable path for PID %d: %w", pid, err)
	}
	identity := processIdentity{
		StartedAt: time.Unix(0, created.Nanoseconds()).UTC(),
		Command:   normalizeWindowsPath(path),
	}
	if identity.StartedAt.IsZero() || identity.Command == "" {
		return processIdentity{}, fmt.Errorf("incomplete process identity for PID %d", pid)
	}
	return identity, nil
}

func windowsProcessPathFromHandle(handle windows.Handle) (string, error) {
	buffer := make([]uint16, 32768)
	size := uint32(len(buffer))
	if err := windows.QueryFullProcessImageName(handle, 0, &buffer[0], &size); err != nil {
		return "", err
	}
	if size == 0 || int(size) > len(buffer) {
		return "", fmt.Errorf("process image path has invalid length %d", size)
	}
	return windows.UTF16ToString(buffer[:size]), nil
}

func normalizeWindowsPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	return strings.ToLower(filepath.Clean(path))
}
