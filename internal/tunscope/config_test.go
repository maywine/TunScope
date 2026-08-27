package tunscope

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestValidateApplicationPaths(t *testing.T) {
	app := filepath.Join(t.TempDir(), "Example.app")
	if err := os.Mkdir(app, 0755); err != nil {
		t.Fatal(err)
	}
	got, err := validateApplicationPaths([]string{app, app})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(app)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{resolved}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestRejectBroadApplicationDirectory(t *testing.T) {
	if _, err := validateApplicationPaths([]string{t.TempDir()}); err == nil {
		t.Fatal("expected non-app directory to be rejected")
	}
}

func TestValidatePackageFamilyNames(t *testing.T) {
	got, err := validatePackageFamilyNames([]string{
		" OpenAI.Codex_2p2nqsd0c76g0 ",
		"openai.codex_2P2NQSD0C76G0",
		"Microsoft.WindowsStore_8wekyb3d8bbwe",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"Microsoft.WindowsStore_8wekyb3d8bbwe",
		"OpenAI.Codex_2p2nqsd0c76g0",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestRejectInvalidPackageFamilyNames(t *testing.T) {
	for _, value := range []string{
		"OpenAI.Codex",
		"OpenAI_Codex_2p2nqsd0c76g0",
		"OpenAI.Codex_short",
		"OpenAI.Codex_2p2nqsd0c76g!",
	} {
		if _, err := validatePackageFamilyNames([]string{value}); err == nil {
			t.Fatalf("invalid package family %q was accepted", value)
		}
	}
}
