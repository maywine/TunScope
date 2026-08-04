BINARY := bin/mactun

.PHONY: all build test clean install uninstall

all: build

build:
	mkdir -p bin
	go build -trimpath -ldflags "-s -w" -o $(BINARY) ./cmd/mactun

test:
	go test ./...

clean:
	rm -f $(BINARY)

install:
	test -x $(BINARY)
	install -d /usr/local/bin
	install -m 0755 $(BINARY) /usr/local/bin/mactun

uninstall:
	rm -f /usr/local/bin/mactun
