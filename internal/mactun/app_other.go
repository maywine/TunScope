//go:build !darwin && !windows

package mactun

import "fmt"

func unsupportedPlatformError() error {
	return fmt.Errorf("mactun supports macOS and Windows only")
}

func (a *App) Up(Config) error     { return unsupportedPlatformError() }
func (a *App) Down() error         { return unsupportedPlatformError() }
func (a *App) Status() error       { return unsupportedPlatformError() }
func (a *App) Doctor(string) error { return unsupportedPlatformError() }
