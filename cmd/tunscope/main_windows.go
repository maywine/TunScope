//go:build windows

package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "github.com/xjasonlyu/tun2socks/v2/dns"
	"github.com/xjasonlyu/tun2socks/v2/engine"
	"github.com/xjasonlyu/tun2socks/v2/tunnel"

	"github.com/maywine/TunScope/internal/tunscope"
)

type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }
func (s *stringList) Set(value string) error {
	if strings.TrimSpace(value) == "" {
		return errors.New("value cannot be empty")
	}
	*s = append(*s, value)
	return nil
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "__engine" {
		os.Exit(runWindowsEngineChild(os.Args[2:]))
	}
	isService, err := isWindowsServiceProcess()
	if err != nil {
		fmt.Fprintf(os.Stderr, "tunscope: detect Windows Service context: %v\n", err)
		os.Exit(1)
	}
	if isService {
		if err := runWindowsService(); err != nil {
			fmt.Fprintf(os.Stderr, "tunscope: Windows Service failed: %v\n", err)
			os.Exit(1)
		}
		return
	}
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printUsage(stderr)
		return 2
	}
	app := tunscope.New(stdout, stderr)
	var err error
	switch args[0] {
	case "up":
		err = runUp(app, args[1:], stderr)
	case "down":
		err = app.Down()
	case "status":
		err = app.Status()
	case "doctor":
		err = runDoctor(app, args[1:], stderr)
	case "service":
		err = runWindowsServiceCommand(args[1:], stdout, stderr)
	case "version", "--version", "-v":
		fmt.Fprintf(stdout, "tunscope %s\n", version)
		return 0
	case "help", "--help", "-h":
		printUsage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n", args[0])
		printUsage(stderr)
		return 2
	}
	if err != nil {
		fmt.Fprintf(stderr, "tunscope: %v\n", err)
		return 1
	}
	return 0
}

func runUp(app *tunscope.App, args []string, stderr io.Writer) error {
	cfg := tunscope.DefaultConfig()
	configPath := configPathFromArgs(args)
	if configPath != "" {
		if err := loadConfig(configPath, &cfg); err != nil {
			return err
		}
	}
	fs := flag.NewFlagSet("up", flag.ContinueOnError)
	fs.SetOutput(stderr)
	bypass := stringList(append([]string(nil), cfg.Bypass...))
	applications := stringList(append([]string(nil), cfg.Applications...))
	deleteConfig := false
	proxyDefault := cfg.Proxy
	fs.StringVar(&configPath, "config", configPath, "read settings from a JSON file")
	fs.BoolVar(&deleteConfig, "delete-config", false, "delete the JSON config immediately after reading it")
	fs.StringVar(&cfg.Proxy, "proxy", proxyDefault, "local SOCKS5 URL, for example socks5://127.0.0.1:7890")
	fs.StringVar(&cfg.Proxy, "p", proxyDefault, "short form of --proxy")
	fs.Var(&bypass, "bypass", "proxy server IP, CIDR, or hostname to keep outside TUN (repeatable)")
	fs.Var(&applications, "app", "application executable to proxy (repeatable)")
	fs.StringVar(&cfg.Interface, "interface", cfg.Interface, "physical adapter name (auto-detected by default)")
	fs.StringVar(&cfg.Gateway4, "gateway", cfg.Gateway4, "physical IPv4 gateway (auto-detected by default)")
	fs.StringVar(&cfg.Device, "device", cfg.Device, "Wintun adapter name")
	fs.IntVar(&cfg.MTU, "mtu", cfg.MTU, "TUN MTU")
	fs.StringVar(&cfg.LogLevel, "log-level", cfg.LogLevel, "engine log level: debug, info, warn, error, silent")
	fs.BoolVar(&cfg.AutoBypass, "auto-bypass", cfg.AutoBypass, "discover current remote peers of a loopback proxy (best effort)")
	fs.BoolVar(&cfg.IPv6, "ipv6", cfg.IPv6, "capture IPv6 traffic as well as IPv4")
	fs.BoolVar(&cfg.TCPOnly, "tcp-only", cfg.TCPOnly, "block selected-application non-DNS UDP for TCP fallback")
	fs.StringVar(&cfg.TrustedDNS, "trusted-dns", cfg.TrustedDNS, "DNS resolver reached through SOCKS5 in per-app mode (empty keeps system DNS direct)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	cfg.Bypass = bypass
	cfg.Applications = applications
	if deleteConfig && configPath != "" {
		if err := os.Remove(configPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("delete temporary config: %w", err)
		}
	}
	return app.Up(cfg)
}

func configPathFromArgs(args []string) string {
	for i, arg := range args {
		if arg == "--config" && i+1 < len(args) {
			return args[i+1]
		}
		if value, ok := strings.CutPrefix(arg, "--config="); ok {
			return value
		}
	}
	return ""
}

func loadConfig(path string, cfg *tunscope.Config) error {
	clean := filepath.Clean(path)
	info, err := os.Lstat(clean)
	if err != nil {
		return fmt.Errorf("inspect config: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() > 64<<10 {
		return fmt.Errorf("config must be a regular file no larger than 64 KiB")
	}
	file, err := os.Open(clean)
	if err != nil {
		return fmt.Errorf("open config: %w", err)
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, 64<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(cfg); err != nil {
		return fmt.Errorf("decode config: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("decode config: multiple JSON values are not allowed")
		}
		return fmt.Errorf("decode config: %w", err)
	}
	return nil
}

func runDoctor(app *tunscope.App, args []string, stderr io.Writer) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	proxy := fs.String("proxy", "", "SOCKS5 URL to test")
	fs.StringVar(proxy, "p", "", "short form of --proxy")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	return app.Doctor(*proxy)
}

func printUsage(w io.Writer) {
	fmt.Fprint(w, `tunscope - lightweight per-application TUN proxy for Windows

Usage:
  tunscope up --proxy socks5://127.0.0.1:7890 --app "C:\Path\Example.exe"
  tunscope up --config C:\path\to\config.json
  tunscope down
  tunscope status
  tunscope doctor --proxy socks5://127.0.0.1:7890
  tunscope service install --startup manual
  tunscope service configure --stdin
  tunscope service start|stop|restart|status|uninstall
  tunscope version

Run route and service mutations in an elevated Terminal. Foreground "up" still uses Ctrl-C; the service is controlled through SCM.
`)
}

func runWindowsEngineChild(args []string) int {
	fs := flag.NewFlagSet("__engine", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	commandHandleText := fs.String("command-handle", "", "internal command pipe")
	responseHandleText := fs.String("response-handle", "", "internal response pipe")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return 2
	}
	commandHandle, err := strconv.ParseUint(*commandHandleText, 10, 64)
	if err != nil || commandHandle == 0 {
		fmt.Fprintln(os.Stderr, "engine: invalid command handle")
		return 2
	}
	responseHandle, err := strconv.ParseUint(*responseHandleText, 10, 64)
	if err != nil || responseHandle == 0 {
		fmt.Fprintln(os.Stderr, "engine: invalid response handle")
		return 2
	}
	commandFile := os.NewFile(uintptr(commandHandle), "tunscope-engine-commands")
	responseFile := os.NewFile(uintptr(responseHandle), "tunscope-engine-responses")
	if commandFile == nil || responseFile == nil {
		fmt.Fprintln(os.Stderr, "engine: inherited control pipes are unavailable")
		return 2
	}
	defer commandFile.Close()
	defer responseFile.Close()

	var cfg tunscope.EngineConfig
	decoder := json.NewDecoder(io.LimitReader(os.Stdin, 64<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		fmt.Fprintf(os.Stderr, "engine: invalid configuration: %v\n", err)
		return 2
	}
	if cfg.Proxy == "" || cfg.Device == "" {
		fmt.Fprintln(os.Stderr, "engine: incomplete configuration")
		return 2
	}

	key := &engine.Key{
		Device:     cfg.Device,
		Proxy:      cfg.Proxy,
		Interface:  cfg.Interface,
		MTU:        cfg.MTU,
		LogLevel:   cfg.LogLevel,
		UDPTimeout: 2 * time.Minute,
	}
	engine.Insert(key)
	engine.Start()
	type networkDialer interface {
		RebindNetwork(string) (int, error)
		Close() error
	}
	var activeDialer networkDialer
	if len(cfg.Applications) > 0 {
		perApp, err := tunscope.NewPerAppDialer(
			cfg.Proxy,
			cfg.Applications,
			cfg.ProxyUDP,
			cfg.TrustedDNS,
			cfg.DirectInterface,
			cfg.DirectInterface6,
			cfg.DirectSource4,
		)
		if err != nil {
			fmt.Fprintf(os.Stderr, "engine: configure per-app routing: %v\n", err)
			engine.Stop()
			return 1
		}
		tunnel.T().SetDialer(perApp)
		activeDialer = perApp
	} else {
		trackedProxy, err := tunscope.NewTrackedProxyDialer(cfg.Proxy)
		if err != nil {
			fmt.Fprintf(os.Stderr, "engine: configure tracked proxy routing: %v\n", err)
			engine.Stop()
			return 1
		}
		tunnel.T().SetDialer(trackedProxy)
		activeDialer = trackedProxy
	}
	defer activeDialer.Close()
	responseEncoder := json.NewEncoder(responseFile)
	if err := responseEncoder.Encode(tunscope.EngineControlResponse{Action: "ready"}); err != nil {
		fmt.Fprintf(os.Stderr, "engine: send startup response: %v\n", err)
		engine.Stop()
		return 1
	}

	stopCh := make(chan os.Signal, 1)
	signal.Notify(stopCh, os.Interrupt)
	defer signal.Stop(stopCh)
	commandCh := make(chan tunscope.EngineControlCommand)
	controlErrCh := make(chan error, 1)
	go func() {
		decoder := json.NewDecoder(commandFile)
		for {
			var command tunscope.EngineControlCommand
			if err := decoder.Decode(&command); err != nil {
				controlErrCh <- err
				return
			}
			commandCh <- command
		}
	}()
	for {
		select {
		case <-stopCh:
			engine.Stop()
			return 0
		case command := <-commandCh:
			response := tunscope.EngineControlResponse{Action: command.Action, Generation: command.Generation}
			if !command.IsNetworkRebind() {
				response.Error = "unsupported engine control action"
			} else {
				closed, err := activeDialer.RebindNetwork(command.Source4)
				response.Closed = closed
				if err != nil {
					response.Error = err.Error()
				}
			}
			if err := responseEncoder.Encode(response); err != nil {
				fmt.Fprintf(os.Stderr, "engine: send control response: %v\n", err)
				engine.Stop()
				return 1
			}
		case err := <-controlErrCh:
			if !errors.Is(err, io.EOF) {
				fmt.Fprintf(os.Stderr, "engine: control channel failed: %v\n", err)
			}
			engine.Stop()
			return 0
		}
	}
}
