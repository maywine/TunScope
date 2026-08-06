//go:build !darwin

package main

import (
	"fmt"
	"os/exec"
)

func configureDetachedProcess(_ *exec.Cmd) error {
	return fmt.Errorf("detached GUI launch is supported only on macOS")
}
