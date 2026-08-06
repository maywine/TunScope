//go:build windows

package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/maywine/MacTun/internal/mactun"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

type testWriteCloser struct{ io.Writer }

func (testWriteCloser) Close() error { return nil }

func TestWindowsServiceHandlerReportsReadyAndStopsCleanly(t *testing.T) {
	var output bytes.Buffer
	handler := &windowsServiceHandler{
		logFile: func() (io.WriteCloser, error) { return testWriteCloser{&output}, nil },
		run: func(stop <-chan struct{}, onActive func(), _, _ io.Writer) error {
			onActive()
			<-stop
			return nil
		},
	}
	requests := make(chan svc.ChangeRequest, 2)
	changes := make(chan svc.Status, 8)
	result := make(chan struct {
		specific bool
		code     uint32
	}, 1)
	go func() {
		specific, code := handler.Execute(nil, requests, changes)
		result <- struct {
			specific bool
			code     uint32
		}{specific, code}
	}()

	waitForServiceState(t, changes, svc.StartPending)
	waitForServiceState(t, changes, svc.Running)
	requests <- svc.ChangeRequest{Cmd: svc.Stop}
	waitForServiceState(t, changes, svc.StopPending)
	select {
	case got := <-result:
		if got.specific || got.code != 0 {
			t.Fatalf("Execute result = (%v, %d), want (false, 0)", got.specific, got.code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("service handler did not stop")
	}
}

func TestWindowsServiceHandlerReturnsRuntimeFailure(t *testing.T) {
	handler := &windowsServiceHandler{
		logFile: func() (io.WriteCloser, error) { return testWriteCloser{io.Discard}, nil },
		run: func(<-chan struct{}, func(), io.Writer, io.Writer) error {
			return errors.New("test startup failure")
		},
	}
	changes := make(chan svc.Status, 4)
	specific, code := handler.Execute(nil, make(chan svc.ChangeRequest), changes)
	if !specific || code != 3 {
		t.Fatalf("Execute result = (%v, %d), want (true, 3)", specific, code)
	}
	if status := <-changes; status.State != svc.StartPending {
		t.Fatalf("initial state = %v, want StartPending", status.State)
	}
}

func TestWindowsServiceStartup(t *testing.T) {
	tests := []struct {
		value   string
		start   uint32
		delayed bool
		ok      bool
	}{
		{"manual", mgr.StartManual, false, true},
		{"AUTOMATIC", mgr.StartAutomatic, true, true},
		{"auto", mgr.StartAutomatic, true, true},
		{"disabled", 0, false, false},
	}
	for _, test := range tests {
		start, delayed, err := windowsServiceStartup(test.value)
		if (err == nil) != test.ok || start != test.start || delayed != test.delayed {
			t.Errorf("windowsServiceStartup(%q) = (%d, %v, %v)", test.value, start, delayed, err)
		}
	}
}

func TestLoadConfigRejectsTrailingValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte("{} {}"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := mactun.DefaultConfig()
	if err := loadConfig(path, &cfg); err == nil {
		t.Fatal("loadConfig accepted multiple JSON values")
	}
}

func waitForServiceState(t *testing.T, changes <-chan svc.Status, want svc.State) {
	t.Helper()
	select {
	case status := <-changes:
		if status.State != want {
			t.Fatalf("service state = %v, want %v", status.State, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for service state %v", want)
	}
}
