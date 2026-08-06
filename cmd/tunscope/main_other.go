//go:build !darwin && !windows

package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "tunscope supports macOS and Windows only")
	os.Exit(1)
}
