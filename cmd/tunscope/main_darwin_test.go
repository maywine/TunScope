//go:build darwin

package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/maywine/TunScope/internal/tunscope"
)

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
