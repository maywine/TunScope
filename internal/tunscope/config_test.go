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
