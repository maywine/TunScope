BINARY := bin/mactun
WINDOWS_BINARY := bin/mactun-windows-amd64.exe
VERSION ?= 0.3.12
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: all build windows-amd64 test clean install uninstall

all: build

build:
	mkdir -p bin
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/mactun

windows-amd64:
	mkdir -p bin
	GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(WINDOWS_BINARY) ./cmd/mactun

test:
	go test ./...

clean:
	rm -f $(BINARY) $(WINDOWS_BINARY)

install:
	test -x $(BINARY)
	install -d /usr/local/bin
	install -m 0755 $(BINARY) /usr/local/bin/mactun

uninstall:
	rm -f /usr/local/bin/mactun
