//go:build darwin

package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

const detachedProcessProbeEnv = "TUNSCOPE_DETACHED_PROCESS_PROBE"

func TestConfigureDetachedProcessCreatesSession(t *testing.T) {
	if os.Getenv(detachedProcessProbeEnv) == "1" {
		pid := os.Getpid()
		pgid, pgidErr := unix.Getpgid(0)
		sid, sidErr := unix.Getsid(0)
		if pgidErr != nil || sidErr != nil {
			fmt.Fprintf(os.Stderr, "inspect detached process: pgid=%v sid=%v\n", pgidErr, sidErr)
			os.Exit(2)
		}
		fmt.Printf("%d %d %d\n", pid, pgid, sid)
		os.Exit(0)
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestConfigureDetachedProcessCreatesSession$")
	cmd.Env = append(os.Environ(), detachedProcessProbeEnv+"=1")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := configureDetachedProcess(cmd); err != nil {
		t.Fatalf("configure detached process: %v", err)
	}
	if err := cmd.Run(); err != nil {
		t.Fatalf("run detached process: %v; stderr: %s", err, stderr.String())
	}
	var pid, pgid, sid int
	if _, err := fmt.Fscan(strings.NewReader(stdout.String()), &pid, &pgid, &sid); err != nil {
		t.Fatalf("parse detached identity %q: %v", stdout.String(), err)
	}
	if pgid != pid || sid != pid {
		t.Fatalf("detached identity pid=%d pgid=%d sid=%d; want pid == pgid == sid", pid, pgid, sid)
	}
}
