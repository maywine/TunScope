//go:build darwin && cgo

package tunscope

import "testing"

func TestPhysicalNetworkEventWatcherStartsAndCloses(t *testing.T) {
	watcher, err := newPhysicalNetworkEventWatcher("lo0")
	if err != nil {
		t.Fatal(err)
	}
	if watcher.Events() == nil {
		t.Fatal("physical-network watcher returned a nil event channel")
	}
	if err := watcher.Close(); err != nil {
		t.Fatal(err)
	}
	if err := watcher.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
}
