package mactun

import "io"

// App exposes the platform-specific TUN lifecycle through a shared CLI-facing
// type. Darwin and Windows provide separate implementations of Up, Down,
// Status, and Doctor while sharing the proxy and data-plane packages.
type App struct {
	runner commandRunner
	out    io.Writer
	errOut io.Writer
}

func New(out, errOut io.Writer) *App {
	return &App{runner: execRunner{}, out: out, errOut: errOut}
}
