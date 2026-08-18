//go:build darwin

package tunscope

type physicalNetworkEvent struct {
	Description string
}

type physicalNetworkEventWatcher interface {
	Events() <-chan physicalNetworkEvent
	Close() error
}
