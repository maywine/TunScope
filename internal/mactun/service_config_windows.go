//go:build windows

package mactun

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/windows"
)

const (
	windowsServiceConfigLimit = 64 << 10
	windowsServiceLogLimit    = 4 << 20
)

// WindowsRuntimeStatus is the machine-readable subset of the TUN state used
// by the Windows service controller and GUI. Proxy credentials are never
// present because the persisted runtime state already stores a redacted URL.
type WindowsRuntimeStatus struct {
	Status       string `json:"status"`
	Detail       string `json:"detail,omitempty"`
	Phase        string `json:"phase,omitempty"`
	Proxy        string `json:"proxy,omitempty"`
	Device       string `json:"device,omitempty"`
	Interface    string `json:"interface,omitempty"`
	OwnerPID     int    `json:"ownerPid,omitempty"`
	EnginePID    int    `json:"enginePid,omitempty"`
	Applications int    `json:"applications,omitempty"`
}

func WindowsServiceDirectory() string {
	return filepath.Join(stateDir(), "service")
}

func WindowsServiceConfigPath() string {
	return filepath.Join(WindowsServiceDirectory(), "config.json")
}

func WindowsServiceLogPath() string {
	return filepath.Join(WindowsServiceDirectory(), "service.log")
}

// NormalizeWindowsServiceConfig performs static validation without contacting
// the proxy or changing routes. Runtime probes are still performed by Up.
func NormalizeWindowsServiceConfig(cfg Config) (Config, error) {
	info, err := validateWindowsConfig(cfg)
	if err != nil {
		return Config{}, err
	}
	configuredApplications, _, err := validateApplicationTargets(cfg.Applications)
	if err != nil {
		return Config{}, err
	}
	cfg.Applications = configuredApplications
	if cfg.TCPOnly && len(cfg.Applications) == 0 {
		return Config{}, fmt.Errorf("TCP-only compatibility mode requires at least one application")
	}
	if len(cfg.Bypass) > 256 {
		return Config{}, fmt.Errorf("at most 256 bypass targets may be configured")
	}
	seenBypass := make(map[string]struct{}, len(cfg.Bypass))
	normalizedBypass := make([]string, 0, len(cfg.Bypass))
	for _, raw := range cfg.Bypass {
		value := strings.TrimSpace(raw)
		if value == "" || len(value) > 512 || strings.ContainsAny(value, "\x00\r\n\t ") {
			return Config{}, fmt.Errorf("invalid bypass target %q", raw)
		}
		key := strings.ToLower(value)
		if _, exists := seenBypass[key]; exists {
			continue
		}
		seenBypass[key] = struct{}{}
		normalizedBypass = append(normalizedBypass, value)
	}
	cfg.Bypass = normalizedBypass
	if info.Loopback && len(cfg.Applications) == 0 && len(cfg.Bypass) == 0 && !cfg.AutoBypass {
		return Config{}, fmt.Errorf("a loopback proxy in global mode requires a bypass target or auto-bypass")
	}
	return cfg, nil
}

func DecodeWindowsServiceConfig(reader io.Reader) (Config, error) {
	data, err := io.ReadAll(io.LimitReader(reader, windowsServiceConfigLimit+1))
	if err != nil {
		return Config{}, fmt.Errorf("read service config: %w", err)
	}
	if len(data) > windowsServiceConfigLimit {
		return Config{}, fmt.Errorf("service config must be no larger than 64 KiB")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	cfg := DefaultConfig()
	if err := decoder.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("decode service config: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return Config{}, fmt.Errorf("decode service config: multiple JSON values are not allowed")
		}
		return Config{}, fmt.Errorf("decode service config: %w", err)
	}
	return NormalizeWindowsServiceConfig(cfg)
}

func LoadWindowsServiceConfig() (Config, error) {
	path := WindowsServiceConfigPath()
	info, err := os.Lstat(path)
	if err != nil {
		return Config{}, err
	}
	if !info.Mode().IsRegular() || info.Size() > windowsServiceConfigLimit {
		return Config{}, fmt.Errorf("service config must be a regular file no larger than 64 KiB")
	}
	file, err := os.Open(path)
	if err != nil {
		return Config{}, err
	}
	defer file.Close()
	return DecodeWindowsServiceConfig(file)
}

func SaveWindowsServiceConfig(cfg Config) error {
	if !windows.GetCurrentProcessToken().IsElevated() {
		return fmt.Errorf("administrator privileges are required to save the service config")
	}
	normalized, err := NormalizeWindowsServiceConfig(cfg)
	if err != nil {
		return err
	}
	if err := ensureWindowsServiceDirectory(); err != nil {
		return err
	}
	path := WindowsServiceConfigPath()
	temporary := filepath.Join(WindowsServiceDirectory(), fmt.Sprintf(".config-%d-%d.tmp", os.Getpid(), time.Now().UnixNano()))
	file, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return fmt.Errorf("create temporary service config: %w", err)
	}
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	writeErr := encoder.Encode(normalized)
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		_ = os.Remove(temporary)
		return errors.Join(writeErr, closeErr)
	}
	if err := protectWindowsServicePath(temporary, windows.NO_INHERITANCE); err != nil {
		_ = os.Remove(temporary)
		return fmt.Errorf("protect temporary service config: %w", err)
	}
	from, err := windows.UTF16PtrFromString(temporary)
	if err != nil {
		_ = os.Remove(temporary)
		return err
	}
	to, err := windows.UTF16PtrFromString(path)
	if err != nil {
		_ = os.Remove(temporary)
		return err
	}
	if err := windows.MoveFileEx(from, to, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH); err != nil {
		_ = os.Remove(temporary)
		return fmt.Errorf("replace service config: %w", err)
	}
	return nil
}

func EnsureWindowsServiceStorage() error {
	if !windows.GetCurrentProcessToken().IsElevated() {
		return fmt.Errorf("administrator privileges are required to create service storage")
	}
	return ensureWindowsServiceDirectory()
}

func ensureWindowsServiceDirectory() error {
	directory := WindowsServiceDirectory()
	if err := os.MkdirAll(directory, 0700); err != nil {
		return fmt.Errorf("create Windows service directory: %w", err)
	}
	if err := protectWindowsServicePath(directory, windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT); err != nil {
		return fmt.Errorf("protect Windows service directory: %w", err)
	}
	return nil
}

func protectWindowsServicePath(path string, inheritance uint32) error {
	adminSID, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return err
	}
	systemSID, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return err
	}
	entries := []windows.EXPLICIT_ACCESS{
		{
			AccessPermissions: windows.GENERIC_ALL,
			AccessMode:        windows.SET_ACCESS,
			Inheritance:       inheritance,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  windows.TRUSTEE_IS_USER,
				TrusteeValue: windows.TrusteeValueFromSID(systemSID),
			},
		},
		{
			AccessPermissions: windows.GENERIC_ALL,
			AccessMode:        windows.SET_ACCESS,
			Inheritance:       inheritance,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  windows.TRUSTEE_IS_GROUP,
				TrusteeValue: windows.TrusteeValueFromSID(adminSID),
			},
		},
	}
	acl, err := windows.ACLFromEntries(entries, nil)
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil,
		nil,
		acl,
		nil,
	)
}

func OpenWindowsServiceLog() (*os.File, error) {
	if err := ensureWindowsServiceDirectory(); err != nil {
		return nil, err
	}
	path := WindowsServiceLogPath()
	if info, err := os.Stat(path); err == nil && info.Size() >= windowsServiceLogLimit {
		previous := path + ".1"
		_ = os.Remove(previous)
		if err := os.Rename(path, previous); err != nil {
			return nil, fmt.Errorf("rotate Windows service log: %w", err)
		}
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0600)
	if err != nil {
		return nil, err
	}
	if err := protectWindowsServicePath(path, windows.NO_INHERITANCE); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func ReadWindowsRuntimeStatus() WindowsRuntimeStatus {
	state, err := loadState()
	if errors.Is(err, os.ErrNotExist) {
		return WindowsRuntimeStatus{Status: "stopped"}
	}
	if err != nil {
		return WindowsRuntimeStatus{Status: "stale", Detail: err.Error()}
	}
	active, detail := activeWindowsStateIdentity(state)
	status := "stale"
	if active {
		status = "active"
	}
	return WindowsRuntimeStatus{
		Status:       status,
		Detail:       detail,
		Phase:        state.Phase,
		Proxy:        state.Proxy,
		Device:       state.Device,
		Interface:    state.Interface,
		OwnerPID:     state.OwnerPID,
		EnginePID:    state.EnginePID,
		Applications: len(state.Applications),
	}
}
