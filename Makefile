BINARY := bin/tunscope
WINDOWS_BINARY := bin/tunscope-windows-amd64.exe
WINDOWS_GUI_DIR := bin/windows-gui
MACOS_RID ?= $(if $(filter arm64,$(shell uname -m)),osx-arm64,osx-x64)
VERSION ?= 0.3.22
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: all build macos-gui windows-amd64 windows-gui test gui-test clean install uninstall

all: build

build:
	mkdir -p bin
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/tunscope

macos-gui:
	./gui/package-macos.sh $(VERSION) $(MACOS_RID) dist

windows-amd64:
	mkdir -p bin
	GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(WINDOWS_BINARY) ./cmd/tunscope

windows-gui:
	dotnet publish gui/TunScope.GUI.csproj -c Release -r win-x64 --self-contained true -p:Version=$(VERSION) -o $(WINDOWS_GUI_DIR)

test:
	go test ./...

gui-test:
	dotnet run --project gui.tests/TunScope.GUI.Tests.csproj -c Release

clean:
	rm -f $(BINARY) $(WINDOWS_BINARY)

install:
	test -x $(BINARY)
	install -d /usr/local/bin
	install -m 0755 $(BINARY) /usr/local/bin/tunscope

uninstall:
	rm -f /usr/local/bin/tunscope
