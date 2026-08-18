//go:build darwin && !cgo

package tunscope

import "fmt"

func newPhysicalNetworkEventWatcher(string) (physicalNetworkEventWatcher, error) {
	return nil, fmt.Errorf("macOS physical-network notifications require cgo")
}
