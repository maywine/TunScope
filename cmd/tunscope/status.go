package main

import (
	"flag"
	"fmt"
	"io"

	"github.com/maywine/TunScope/internal/tunscope"
)

func runStatus(app *tunscope.App, args []string, stderr io.Writer) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "print machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected status arguments: %v", fs.Args())
	}
	if *asJSON {
		return app.StatusJSON()
	}
	return app.Status()
}
