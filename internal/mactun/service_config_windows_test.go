//go:build windows

package mactun

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func TestNormalizeWindowsServiceConfig(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.Proxy = "socks5://127.0.0.1:7890"
	cfg.Applications = []string{executable, executable}
	cfg.Bypass = []string{" node.example.com ", "NODE.EXAMPLE.COM"}
	normalized, err := NormalizeWindowsServiceConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(normalized.Applications) != 1 || len(normalized.Bypass) != 1 || normalized.Bypass[0] != "node.example.com" {
		t.Fatalf("normalized config = %#v", normalized)
	}
}

func TestNormalizeWindowsServiceConfigRejectsUnsafeGlobalLoopback(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Proxy = "socks5://127.0.0.1:7890"
	if _, err := NormalizeWindowsServiceConfig(cfg); err == nil || !strings.Contains(err.Error(), "bypass") {
		t.Fatalf("error = %v, want missing bypass error", err)
	}
}

func TestDecodeWindowsServiceConfigRejectsTrailingAndOversizedInput(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Proxy = "socks5://127.0.0.1:7890"
	cfg.AutoBypass = true
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeWindowsServiceConfig(strings.NewReader(string(data) + " {}")); err == nil {
		t.Fatal("multiple JSON values were accepted")
	}
	if _, err := DecodeWindowsServiceConfig(strings.NewReader(strings.Repeat(" ", windowsServiceConfigLimit+1))); err == nil {
		t.Fatal("oversized config was accepted")
	}
}

func TestDecodeWindowsServiceConfigAppliesDefaults(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(map[string]any{
		"proxy":        "socks5://127.0.0.1:7890",
		"applications": []string{executable},
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := DecodeWindowsServiceConfig(strings.NewReader(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	defaults := DefaultConfig()
	if cfg.Device != defaults.Device || cfg.MTU != defaults.MTU || cfg.LogLevel != defaults.LogLevel || cfg.IPv6 != defaults.IPv6 || cfg.TrustedDNS != defaults.TrustedDNS {
		t.Fatalf("decoded defaults = %#v, want values from %#v", cfg, defaults)
	}
}

func TestSaveAndLoadWindowsServiceConfig(t *testing.T) {
	if !windows.GetCurrentProcessToken().IsElevated() {
		t.Skip("test requires an elevated Windows token to verify the protected service config")
	}
	t.Setenv("MACTUN_STATE_DIR", t.TempDir())
	cfg := DefaultConfig()
	cfg.Proxy = "socks5://127.0.0.1:7890"
	cfg.AutoBypass = true
	if err := SaveWindowsServiceConfig(cfg); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadWindowsServiceConfig()
	if err != nil {
		t.Fatal(err)
	}
	want, err := NormalizeWindowsServiceConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(loaded, want) {
		t.Fatalf("loaded config = %#v, want %#v", loaded, want)
	}
	descriptor, err := windows.GetNamedSecurityInfo(
		WindowsServiceConfigPath(),
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
	)
	if err != nil {
		t.Fatal(err)
	}
	sddl := descriptor.String()
	if !strings.Contains(sddl, ";;;SY)") || !strings.Contains(sddl, ";;;BA)") {
		t.Fatalf("service config ACL = %q, want SYSTEM and Administrators", sddl)
	}
}
