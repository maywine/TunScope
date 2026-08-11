package main

import "errors"

type networkLifecycle interface {
	InvalidateNetwork() (int, error)
	RebindNetwork(string) (int, error)
}

func invalidateNetwork(components ...networkLifecycle) (int, error) {
	closed := 0
	var result error
	for _, component := range components {
		count, err := component.InvalidateNetwork()
		closed += count
		result = errors.Join(result, err)
	}
	return closed, result
}

func rebindNetwork(source4 string, components ...networkLifecycle) (int, error) {
	closed := 0
	var result error
	for _, component := range components {
		count, err := component.RebindNetwork(source4)
		closed += count
		result = errors.Join(result, err)
	}
	return closed, result
}
