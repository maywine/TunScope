//go:build (!darwin && !windows) || (darwin && !cgo)

package tunscope

import "fmt"

func newProcessMatcher([]string) (processMatcher, error) {
	return nil, fmt.Errorf("per-app process routing requires macOS with cgo enabled")
}
