//go:build windows

package tunscope

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/sys/windows"
)

const windowsLockName = `Global\Maywine.TunScope.StateLock`

var (
	errLockHeld   = errors.New("tunscope lock is held")
	lockMu        sync.Mutex
	heldLock      windows.Handle
	heldLockToken string
)

func stateDir() string {
	if dir := os.Getenv("TUNSCOPE_STATE_DIR"); dir != "" {
		return dir
	}
	base := os.Getenv("ProgramData")
	if base == "" {
		drive := os.Getenv("SystemDrive")
		if drive == "" {
			drive = `C:`
		}
		base = filepath.Join(drive+string(filepath.Separator), "ProgramData")
	}
	return filepath.Join(base, "TunScope")
}

func statePath() string { return filepath.Join(stateDir(), "state.json") }
func lockPath() string  { return filepath.Join(stateDir(), "lock") }

func acquireLock() error {
	lockMu.Lock()
	defer lockMu.Unlock()
	if heldLock != 0 {
		return fmt.Errorf("%w by this process", errLockHeld)
	}
	if err := os.MkdirAll(stateDir(), 0755); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	name, err := windows.UTF16PtrFromString(windowsLockName)
	if err != nil {
		return err
	}
	handle, createErr := windows.CreateMutex(nil, false, name)
	if handle == 0 {
		return fmt.Errorf("create state mutex: %w", createErr)
	}
	if createErr != nil && !errors.Is(createErr, windows.ERROR_ALREADY_EXISTS) {
		_ = windows.CloseHandle(handle)
		return fmt.Errorf("create state mutex: %w", createErr)
	}
	waitResult, waitErr := windows.WaitForSingleObject(handle, 0)
	if waitErr != nil {
		_ = windows.CloseHandle(handle)
		return fmt.Errorf("wait for state mutex: %w", waitErr)
	}
	if waitResult != windows.WAIT_OBJECT_0 && waitResult != windows.WAIT_ABANDONED {
		_ = windows.CloseHandle(handle)
		if waitResult == uint32(windows.WAIT_TIMEOUT) {
			if pid := lockPID(); pid > 0 {
				return fmt.Errorf("%w by PID %d", errLockHeld, pid)
			}
			return fmt.Errorf("%w", errLockHeld)
		}
		return fmt.Errorf("unexpected state mutex wait result %#x", waitResult)
	}

	tokenBytes := make([]byte, 16)
	if _, err := rand.Read(tokenBytes); err != nil {
		_ = windows.ReleaseMutex(handle)
		_ = windows.CloseHandle(handle)
		return fmt.Errorf("generate lock token: %w", err)
	}
	token := hex.EncodeToString(tokenBytes)
	if err := os.WriteFile(lockPath(), []byte(fmt.Sprintf("%d %s\r\n", os.Getpid(), token)), 0644); err != nil {
		_ = windows.ReleaseMutex(handle)
		_ = windows.CloseHandle(handle)
		return fmt.Errorf("write lock metadata: %w", err)
	}
	heldLock = handle
	heldLockToken = token
	return nil
}

func lockPID() int {
	pid, _ := lockRecord()
	return pid
}

func lockRecord() (int, string) {
	data, err := os.ReadFile(lockPath())
	if err != nil {
		return 0, ""
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return 0, ""
	}
	pid, _ := strconv.Atoi(fields[0])
	if len(fields) < 2 {
		return pid, ""
	}
	return pid, fields[1]
}

func currentLockToken() string {
	lockMu.Lock()
	defer lockMu.Unlock()
	return heldLockToken
}

func releaseLock() {
	lockMu.Lock()
	defer lockMu.Unlock()
	if heldLock == 0 {
		return
	}
	_ = windows.ReleaseMutex(heldLock)
	_ = windows.CloseHandle(heldLock)
	heldLock = 0
	heldLockToken = ""
}

func saveState(state *State) error {
	if err := os.MkdirAll(stateDir(), 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.tmp-%d", statePath(), os.Getpid())
	if err := os.WriteFile(tmp, append(data, '\n'), 0644); err != nil {
		return err
	}
	from, err := windows.UTF16PtrFromString(tmp)
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	to, err := windows.UTF16PtrFromString(statePath())
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := windows.MoveFileEx(from, to, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("replace state file: %w", err)
	}
	return nil
}

func loadState() (*State, error) {
	data, err := os.ReadFile(statePath())
	if err != nil {
		return nil, err
	}
	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("invalid state file: %w", err)
	}
	if state.Version != stateVersion {
		return nil, fmt.Errorf("unsupported state version %d", state.Version)
	}
	return &state, nil
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return errors.Is(err, windows.ERROR_ACCESS_DENIED)
	}
	defer windows.CloseHandle(handle)
	result, err := windows.WaitForSingleObject(handle, 0)
	return err == nil && result == uint32(windows.WAIT_TIMEOUT)
}

func windowsStopEventName(ownerToken string) string {
	return `Global\Maywine.TunScope.Stop.` + ownerToken
}

func createWindowsStopEvent(ownerToken string) (windows.Handle, string, error) {
	name := windowsStopEventName(ownerToken)
	namePtr, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return 0, "", err
	}
	handle, err := windows.CreateEvent(nil, 1, 0, namePtr)
	if err != nil && !errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		if handle != 0 {
			_ = windows.CloseHandle(handle)
		}
		return 0, "", fmt.Errorf("create stop event: %w", err)
	}
	if handle == 0 {
		return 0, "", fmt.Errorf("create stop event returned an invalid handle")
	}
	return handle, name, nil
}

func signalWindowsStopEvent(state *State) error {
	expected := windowsStopEventName(state.OwnerToken)
	if state.StopEvent == "" || state.StopEvent != expected {
		return fmt.Errorf("persisted stop event does not match the session owner token")
	}
	name, err := windows.UTF16PtrFromString(state.StopEvent)
	if err != nil {
		return err
	}
	handle, err := windows.OpenEvent(windows.EVENT_MODIFY_STATE, false, name)
	if err != nil {
		return fmt.Errorf("open owner stop event: %w", err)
	}
	defer windows.CloseHandle(handle)
	if err := windows.SetEvent(handle); err != nil {
		return fmt.Errorf("signal owner stop event: %w", err)
	}
	return nil
}

func waitWindowsEvent(handle windows.Handle) <-chan error {
	result := make(chan error, 1)
	go func() {
		waitResult, err := windows.WaitForSingleObject(handle, windows.INFINITE)
		if err == nil && waitResult != windows.WAIT_OBJECT_0 {
			err = fmt.Errorf("unexpected stop event wait result %#x", waitResult)
		}
		result <- err
	}()
	return result
}
