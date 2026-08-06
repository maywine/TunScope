package main

// version is a variable so release builds can inject the Git tag with
// -ldflags "-X main.version=<version>". Local builds retain this default.
var version = "0.3.12"
