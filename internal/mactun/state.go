package mactun

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
	"syscall"
)

var (
	errLockHeld   = errors.New("mactun lock is held")
	lockMu        sync.Mutex
	heldLock      *os.File
	heldLockToken string
)

func stateDir() string {
	if dir := os.Getenv("MACTUN_STATE_DIR"); dir != "" {
		return dir
	}
	return "/var/run/mactun"
}

func statePath() string { return filepath.Join(stateDir(), "state.json") }
func lockPath() string  { return filepath.Join(stateDir(), "lock") }

func acquireLock() error {
	lockMu.Lock()
	defer lockMu.Unlock()
	if heldLock != nil {
		return fmt.Errorf("%w by this process", errLockHeld)
	}
	if err := os.MkdirAll(stateDir(), 0755); err != nil {
		return err
	}
	f, err := os.OpenFile(lockPath(), os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		return err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			if pid := lockPID(); pid > 0 {
				return fmt.Errorf("%w by PID %d", errLockHeld, pid)
			}
			return fmt.Errorf("%w at %s", errLockHeld, lockPath())
		}
		return fmt.Errorf("lock %s: %w", lockPath(), err)
	}
	releaseOnError := func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}
	if err := f.Chmod(0644); err != nil {
		releaseOnError()
		return fmt.Errorf("make lock metadata readable: %w", err)
	}
	tokenBytes := make([]byte, 16)
	if _, err := rand.Read(tokenBytes); err != nil {
		releaseOnError()
		return fmt.Errorf("generate lock token: %w", err)
	}
	token := hex.EncodeToString(tokenBytes)
	if err := f.Truncate(0); err != nil {
		releaseOnError()
		return err
	}
	if _, err := f.Seek(0, 0); err != nil {
		releaseOnError()
		return err
	}
	if _, err := fmt.Fprintf(f, "%d %s\n", os.Getpid(), token); err != nil {
		releaseOnError()
		return err
	}
	if err := f.Sync(); err != nil {
		releaseOnError()
		return err
	}
	heldLock = f
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
	if heldLock == nil {
		return
	}
	_ = syscall.Flock(int(heldLock.Fd()), syscall.LOCK_UN)
	_ = heldLock.Close()
	heldLock = nil
	heldLockToken = ""
}

func saveState(state *State) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	tmp := statePath() + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0644); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, statePath())
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
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
