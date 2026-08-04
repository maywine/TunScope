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
	"strings"
	"syscall"
	"time"

	_ "github.com/xjasonlyu/tun2socks/v2/dns"
	"github.com/xjasonlyu/tun2socks/v2/engine"
	"github.com/xjasonlyu/tun2socks/v2/tunnel"

	"github.com/maywine/MacTun/internal/mactun"
)

const version = "0.3.11"

type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error {
	if strings.TrimSpace(v) == "" {
		return errors.New("value cannot be empty")
	}
	*s = append(*s, v)
	return nil
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "__engine" {
		os.Exit(runEngineChild())
	}
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printUsage(stderr)
		return 2
	}

	app := mactun.New(stdout, stderr)
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
	case "version", "--version", "-v":
		fmt.Fprintf(stdout, "mactun %s\n", version)
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
		fmt.Fprintf(stderr, "mactun: %v\n", err)
		return 1
	}
	return 0
}

func runUp(app *mactun.App, args []string, stderr io.Writer) error {
	cfg := mactun.DefaultConfig()
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
	fs.Var(&applications, "app", "application bundle or executable to proxy (repeatable)")
	fs.StringVar(&cfg.Interface, "interface", cfg.Interface, "physical interface (auto-detected by default)")
	fs.StringVar(&cfg.Gateway4, "gateway", cfg.Gateway4, "physical IPv4 gateway (auto-detected by default)")
	fs.StringVar(&cfg.Device, "device", cfg.Device, "utun device name")
	fs.IntVar(&cfg.MTU, "mtu", cfg.MTU, "TUN MTU")
	fs.StringVar(&cfg.LogLevel, "log-level", cfg.LogLevel, "engine log level: debug, info, warn, error, silent")
	fs.BoolVar(&cfg.AutoBypass, "auto-bypass", cfg.AutoBypass, "discover current remote peers of a loopback proxy (best effort)")
	fs.BoolVar(&cfg.IPv6, "ipv6", cfg.IPv6, "capture IPv6 traffic as well as IPv4")
	fs.BoolVar(&cfg.TCPOnly, "tcp-only", cfg.TCPOnly, "block selected-app non-DNS UDP so supported applications use TCP")
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

func loadConfig(path string, cfg *mactun.Config) error {
	clean := filepath.Clean(path)
	info, err := os.Lstat(clean)
	if err != nil {
		return fmt.Errorf("inspect config: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() > 64<<10 {
		return fmt.Errorf("config must be a regular file no larger than 64 KiB")
	}
	f, err := os.Open(clean)
	if err != nil {
		return fmt.Errorf("open config: %w", err)
	}
	defer f.Close()
	decoder := json.NewDecoder(io.LimitReader(f, 64<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(cfg); err != nil {
		return fmt.Errorf("decode config: %w", err)
	}
	return nil
}

func runDoctor(app *mactun.App, args []string, stderr io.Writer) error {
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
	fmt.Fprint(w, `mactun - lightweight TUN mode for macOS

Usage:
  mactun up --proxy socks5://127.0.0.1:7890 --app /Applications/Example.app
  mactun up --config /path/to/config.json
  mactun down
  mactun status
  mactun doctor --proxy socks5://127.0.0.1:7890
  mactun version

Run "mactun up" and "mactun down" with sudo. Press Ctrl-C to stop and restore routes.
`)
}

// runEngineChild hosts the tun2socks data plane inside the same binary. The
// configuration arrives through fd 3 so proxy credentials are not exposed in
// the process list.
func runEngineChild() int {
	f := os.NewFile(3, "mactun-engine-config")
	if f == nil {
		fmt.Fprintln(os.Stderr, "engine: missing configuration fd")
		return 2
	}
	defer f.Close()

	var cfg mactun.EngineConfig
	if err := json.NewDecoder(io.LimitReader(f, 64<<10)).Decode(&cfg); err != nil {
		fmt.Fprintf(os.Stderr, "engine: invalid configuration: %v\n", err)
		return 2
	}
	if cfg.Proxy == "" || cfg.Device == "" {
		fmt.Fprintln(os.Stderr, "engine: incomplete configuration")
		return 2
	}
	commandFile := os.NewFile(4, "mactun-engine-commands")
	responseFile := os.NewFile(5, "mactun-engine-responses")
	if commandFile == nil || responseFile == nil {
		fmt.Fprintln(os.Stderr, "engine: missing control pipes")
		return 2
	}
	defer commandFile.Close()
	defer responseFile.Close()

	stopCh := make(chan os.Signal, 1)
	signal.Notify(stopCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(stopCh)

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
	var perAppDialer *mactun.PerAppDialer
	if len(cfg.Applications) > 0 {
		var err error
		perAppDialer, err = mactun.NewPerAppDialer(
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
		tunnel.T().SetDialer(perAppDialer)
		activeDialer = perAppDialer
	} else {
		trackedProxy, err := mactun.NewTrackedProxyDialer(cfg.Proxy)
		if err != nil {
			fmt.Fprintf(os.Stderr, "engine: configure tracked proxy routing: %v\n", err)
			engine.Stop()
			return 1
		}
		tunnel.T().SetDialer(trackedProxy)
		activeDialer = trackedProxy
	}
	defer activeDialer.Close()

	commandCh := make(chan mactun.EngineControlCommand)
	controlErrCh := make(chan error, 1)
	go func() {
		decoder := json.NewDecoder(commandFile)
		for {
			var command mactun.EngineControlCommand
			if err := decoder.Decode(&command); err != nil {
				controlErrCh <- err
				return
			}
			commandCh <- command
		}
	}()
	responseEncoder := json.NewEncoder(responseFile)
	for {
		select {
		case <-stopCh:
			engine.Stop()
			return 0
		case command := <-commandCh:
			response := mactun.EngineControlResponse{
				Action: command.Action, Generation: command.Generation,
			}
			switch {
			case !command.IsNetworkRebind():
				response.Error = "unsupported engine control action"
			default:
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
			fmt.Fprintf(os.Stderr, "engine: control channel failed: %v\n", err)
			engine.Stop()
			return 1
		}
	}
}
