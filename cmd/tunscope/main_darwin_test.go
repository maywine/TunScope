//go:build darwin

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/maywine/TunScope/internal/tunscope"
)

func TestStatusJSONStopped(t *testing.T) {
	t.Setenv("TUNSCOPE_STATE_DIR", t.TempDir())
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if exitCode := run([]string{"status", "--json"}, &stdout, &stderr); exitCode != 0 {
		t.Fatalf("run status exit code = %d, stderr = %q", exitCode, stderr.String())
	}
	var report tunscope.RuntimeStatusReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("decode status JSON: %v; output = %q", err, stdout.String())
	}
	if report.Status != "stopped" {
		t.Fatalf("status = %q, want stopped", report.Status)
	}
}

func TestLoadConfigPreservesBypassTargets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	data := []byte(`{
  "proxy": "socks5://127.0.0.1:1080",
  "device": "utun123",
  "bypass": ["10.16.191.0/24"],
  "applications": ["/Applications/Example.app"],
  "mtu": 1500,
  "logLevel": "info",
  "autoBypass": true,
  "ipv6": true,
  "tcpOnly": true,
  "trustedDNS": "8.8.8.8:53",
  "icmpDirect": true
}`)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	cfg := tunscope.DefaultConfig()
	if err := loadConfig(path, &cfg); err != nil {
		t.Fatal(err)
	}
	if want := []string{"10.16.191.0/24"}; !reflect.DeepEqual(cfg.Bypass, want) {
		t.Fatalf("bypass = %v, want %v", cfg.Bypass, want)
	}
}
