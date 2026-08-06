//go:build windows

package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/maywine/MacTun/internal/mactun"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"
	"golang.org/x/sys/windows/svc/mgr"
)

const (
	windowsServiceName        = "MacTun"
	windowsServiceDisplayName = "MacTun Per-Application Proxy"
	windowsServiceDescription = "Routes selected Windows applications through a local SOCKS5 proxy using Wintun."
	windowsServiceWaitTimeout = 45 * time.Second
)

func isWindowsServiceProcess() (bool, error) {
	return svc.IsWindowsService()
}

func runWindowsService() error {
	return svc.Run(windowsServiceName, &windowsServiceHandler{})
}

type windowsServiceHandler struct {
	run     func(stop <-chan struct{}, onActive func(), out, errOut io.Writer) error
	logFile func() (io.WriteCloser, error)
}

func (h *windowsServiceHandler) Execute(_ []string, requests <-chan svc.ChangeRequest, changes chan<- svc.Status) (bool, uint32) {
	logFile := h.logFile
	if logFile == nil {
		logFile = func() (io.WriteCloser, error) { return mactun.OpenWindowsServiceLog() }
	}
	writer, logErr := logFile()
	if logErr != nil {
		changes <- svc.Status{State: svc.StopPending, WaitHint: 1000}
		return true, 2
	}
	defer writer.Close()
	logger := log.New(writer, "service: ", log.LstdFlags|log.Lmicroseconds)
	eventWriter, _ := eventlog.Open(windowsServiceName)
	if eventWriter != nil {
		defer eventWriter.Close()
	}
	logInfo := func(message string) {
		logger.Print(message)
		if eventWriter != nil {
			_ = eventWriter.Info(1, message)
		}
	}
	logError := func(message string) {
		logger.Print("ERROR: " + message)
		if eventWriter != nil {
			_ = eventWriter.Error(100, message)
		}
	}

	run := h.run
	if run == nil {
		run = func(stop <-chan struct{}, onActive func(), out, errOut io.Writer) error {
			cfg, err := mactun.LoadWindowsServiceConfig()
			if err != nil {
				return fmt.Errorf("load %s: %w", mactun.WindowsServiceConfigPath(), err)
			}
			return mactun.New(out, errOut).UpAsWindowsService(cfg, stop, onActive)
		}
	}

	logInfo("starting Windows Service data plane")
	current := svc.Status{State: svc.StartPending, CheckPoint: 1, WaitHint: 30000}
	changes <- current
	stop := make(chan struct{})
	ready := make(chan struct{})
	result := make(chan error, 1)
	var readyOnce sync.Once
	go func() {
		result <- run(stop, func() { readyOnce.Do(func() { close(ready) }) }, writer, writer)
	}()

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	stopping := false
	stopOnce := sync.Once{}
	requestStop := func(reason string) {
		if stopping {
			return
		}
		stopping = true
		logInfo(reason)
		stopOnce.Do(func() { close(stop) })
		current = svc.Status{State: svc.StopPending, CheckPoint: 1, WaitHint: 20000}
		changes <- current
	}

	for {
		select {
		case <-ready:
			ready = nil
			if !stopping {
				current = svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
				changes <- current
				logInfo("TUN data plane is active")
			}
		case err := <-result:
			if err != nil {
				logError(err.Error())
				return true, 3
			}
			logInfo("Windows Service data plane stopped cleanly")
			return false, 0
		case request := <-requests:
			switch request.Cmd {
			case svc.Interrogate:
				changes <- current
			case svc.Stop:
				requestStop("received SCM stop request")
			case svc.Shutdown:
				requestStop("received system shutdown request")
			default:
				logger.Printf("ignored unsupported service control %d", request.Cmd)
			}
		case <-ticker.C:
			if current.State == svc.StartPending || current.State == svc.StopPending {
				current.CheckPoint++
				changes <- current
			}
		}
	}
}

func runWindowsServiceCommand(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		printWindowsServiceUsage(stderr)
		return fmt.Errorf("a service command is required")
	}
	if args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		printWindowsServiceUsage(stdout)
		return nil
	}
	if !windows.GetCurrentProcessToken().IsElevated() {
		return fmt.Errorf("administrator privileges are required for Windows Service commands")
	}

	switch args[0] {
	case "install":
		fs := flag.NewFlagSet("service install", flag.ContinueOnError)
		fs.SetOutput(stderr)
		startup := fs.String("startup", "manual", "service startup: manual or automatic")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
		}
		return installWindowsService(*startup, stdout, stderr)
	case "configure":
		return configureWindowsService(args[1:], stdout, stderr)
	case "start", "stop", "restart", "uninstall", "status":
		if len(args) != 1 && args[0] != "status" {
			return fmt.Errorf("unexpected arguments: %s", strings.Join(args[1:], " "))
		}
		return controlWindowsService(args[0], args[1:], stdout, stderr)
	default:
		printWindowsServiceUsage(stderr)
		return fmt.Errorf("unknown service command %q", args[0])
	}
}

func printWindowsServiceUsage(w io.Writer) {
	fmt.Fprint(w, `Usage:
  mactun service install [--startup manual|automatic]
  mactun service configure --stdin
  mactun service configure --config C:\path\to\config.json
  mactun service start
  mactun service stop
  mactun service restart
  mactun service status [--json]
  mactun service uninstall

The service runs as LocalSystem. Manual startup is recommended when SOCKS5 belongs to a logged-in desktop user.
`)
}

func configureWindowsService(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("service configure", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fromStdin := fs.Bool("stdin", false, "read JSON config from standard input")
	configPath := fs.String("config", "", "read JSON config from a file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	if *fromStdin == (*configPath != "") {
		return fmt.Errorf("specify exactly one of --stdin or --config")
	}
	var cfg mactun.Config
	var err error
	if *fromStdin {
		cfg, err = mactun.DecodeWindowsServiceConfig(os.Stdin)
	} else {
		cfg = mactun.DefaultConfig()
		err = loadConfig(*configPath, &cfg)
		if err == nil {
			cfg, err = mactun.NormalizeWindowsServiceConfig(cfg)
		}
	}
	if err != nil {
		return err
	}
	if err := mactun.SaveWindowsServiceConfig(cfg); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "service config saved securely: %s\n", mactun.WindowsServiceConfigPath())
	status, statusErr := queryWindowsServiceStatus()
	if statusErr == nil && status.Installed && status.State != "stopped" {
		fmt.Fprintln(stdout, "service is running; restart it to apply the new config")
	}
	return nil
}

func installWindowsService(startup string, stdout, stderr io.Writer) error {
	startType, delayed, err := windowsServiceStartup(startup)
	if err != nil {
		return err
	}
	executable, err := secureWindowsServiceExecutable()
	if err != nil {
		return err
	}
	if err := mactun.EnsureWindowsServiceStorage(); err != nil {
		return err
	}
	manager, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect to Service Control Manager: %w", err)
	}
	defer manager.Disconnect()
	configuration := mgr.Config{
		ServiceType:      windows.SERVICE_WIN32_OWN_PROCESS,
		BinaryPathName:   executable,
		StartType:        startType,
		ErrorControl:     mgr.ErrorNormal,
		ServiceStartName: "LocalSystem",
		DisplayName:      windowsServiceDisplayName,
		Description:      windowsServiceDescription,
		DelayedAutoStart: delayed,
	}
	service, openErr := manager.OpenService(windowsServiceName)
	if openErr == nil {
		defer service.Close()
		if err := service.UpdateConfig(configuration); err != nil {
			return fmt.Errorf("update Windows Service: %w", err)
		}
		fmt.Fprintf(stdout, "Windows Service updated: %s (%s startup)\n", windowsServiceName, startup)
	} else {
		if !errors.Is(openErr, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
			return fmt.Errorf("open Windows Service: %w", openErr)
		}
		service, err = manager.CreateService(windowsServiceName, executable, configuration)
		if err != nil {
			return fmt.Errorf("install Windows Service: %w", err)
		}
		defer service.Close()
		fmt.Fprintf(stdout, "Windows Service installed: %s (%s startup)\n", windowsServiceName, startup)
	}
	if err := eventlog.InstallAsEventCreate(windowsServiceName, eventlog.Error|eventlog.Warning|eventlog.Info); err != nil && !strings.Contains(err.Error(), "registry key already exists") {
		fmt.Fprintf(stderr, "warning: install Windows Event Log source: %v\n", err)
	}
	fmt.Fprintln(stdout, "the service was not started; save a config, then run 'mactun service start'")
	return nil
}

func secureWindowsServiceExecutable() (string, error) {
	executable, err := os.Executable()
	if err != nil {
		return "", err
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return "", fmt.Errorf("resolve service executable: %w", err)
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		return "", err
	}
	programFiles := os.Getenv("ProgramFiles")
	if programFiles == "" {
		return "", fmt.Errorf("ProgramFiles is unavailable")
	}
	programFiles, err = filepath.Abs(programFiles)
	if err != nil {
		return "", err
	}
	relative, err := filepath.Rel(programFiles, executable)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("refusing to install a LocalSystem service from a user-writable path; run install.ps1 so mactun.exe is under %s", programFiles)
	}
	if info, err := os.Stat(filepath.Join(filepath.Dir(executable), "wintun.dll")); err != nil || !info.Mode().IsRegular() {
		return "", fmt.Errorf("wintun.dll must be next to the service executable")
	}
	return executable, nil
}

func windowsServiceStartup(value string) (uint32, bool, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "manual":
		return mgr.StartManual, false, nil
	case "automatic", "auto":
		return mgr.StartAutomatic, true, nil
	default:
		return 0, false, fmt.Errorf("--startup must be manual or automatic")
	}
}

type windowsServiceStatus struct {
	Installed   bool                        `json:"installed"`
	State       string                      `json:"state"`
	Startup     string                      `json:"startup,omitempty"`
	ProcessID   uint32                      `json:"processId,omitempty"`
	ExitCode    uint32                      `json:"exitCode,omitempty"`
	ConfigPath  string                      `json:"configPath"`
	ConfigReady bool                        `json:"configReady"`
	ConfigError string                      `json:"configError,omitempty"`
	LogPath     string                      `json:"logPath"`
	Runtime     mactun.WindowsRuntimeStatus `json:"runtime"`
}

func queryWindowsServiceStatus() (windowsServiceStatus, error) {
	result := windowsServiceStatus{
		State:      "not-installed",
		ConfigPath: mactun.WindowsServiceConfigPath(),
		LogPath:    mactun.WindowsServiceLogPath(),
		Runtime:    mactun.ReadWindowsRuntimeStatus(),
	}
	if _, err := mactun.LoadWindowsServiceConfig(); err == nil {
		result.ConfigReady = true
	} else if !errors.Is(err, os.ErrNotExist) {
		result.ConfigError = err.Error()
	}
	manager, err := mgr.Connect()
	if err != nil {
		return result, fmt.Errorf("connect to Service Control Manager: %w", err)
	}
	defer manager.Disconnect()
	service, err := manager.OpenService(windowsServiceName)
	if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return result, nil
	}
	if err != nil {
		return result, err
	}
	defer service.Close()
	status, err := service.Query()
	if err != nil {
		return result, err
	}
	configuration, err := service.Config()
	if err != nil {
		return result, err
	}
	result.Installed = true
	result.State = windowsServiceStateName(status.State)
	result.Startup = windowsServiceStartTypeName(configuration.StartType, configuration.DelayedAutoStart)
	result.ProcessID = status.ProcessId
	if status.ServiceSpecificExitCode != 0 {
		result.ExitCode = status.ServiceSpecificExitCode
	} else {
		result.ExitCode = status.Win32ExitCode
	}
	return result, nil
}

func controlWindowsService(command string, args []string, stdout, stderr io.Writer) error {
	if command == "status" {
		fs := flag.NewFlagSet("service status", flag.ContinueOnError)
		fs.SetOutput(stderr)
		asJSON := fs.Bool("json", false, "print machine-readable JSON")
		if err := fs.Parse(args); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
		}
		status, err := queryWindowsServiceStatus()
		if err != nil {
			return err
		}
		if *asJSON {
			encoder := json.NewEncoder(stdout)
			encoder.SetIndent("", "  ")
			return encoder.Encode(status)
		}
		fmt.Fprintf(stdout, "service: %s\nstartup: %s\nconfig: %t (%s)\nruntime: %s\nlog: %s\n", status.State, status.Startup, status.ConfigReady, status.ConfigPath, status.Runtime.Status, status.LogPath)
		if status.ConfigError != "" {
			fmt.Fprintf(stdout, "config error: %s\n", status.ConfigError)
		}
		if status.Runtime.Detail != "" {
			fmt.Fprintf(stdout, "runtime detail: %s\n", status.Runtime.Detail)
		}
		return nil
	}

	manager, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect to Service Control Manager: %w", err)
	}
	defer manager.Disconnect()
	service, err := manager.OpenService(windowsServiceName)
	if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		if command == "uninstall" {
			if err := cleanStaleWindowsServiceRuntime(stdout, stderr); err != nil {
				return err
			}
			fmt.Fprintln(stdout, "Windows Service is not installed")
			return nil
		}
		return fmt.Errorf("Windows Service is not installed")
	}
	if err != nil {
		return err
	}
	defer service.Close()

	switch command {
	case "start":
		if _, err := mactun.LoadWindowsServiceConfig(); err != nil {
			return fmt.Errorf("service config is not ready: %w", err)
		}
		if err := startWindowsService(service, windowsServiceWaitTimeout); err != nil {
			return err
		}
		fmt.Fprintln(stdout, "Windows Service started; TUN is active")
	case "stop":
		if err := stopWindowsService(service, windowsServiceWaitTimeout); err != nil {
			return err
		}
		if err := cleanStaleWindowsServiceRuntime(stdout, stderr); err != nil {
			return err
		}
		fmt.Fprintln(stdout, "Windows Service stopped; routes restored")
	case "restart":
		if _, err := mactun.LoadWindowsServiceConfig(); err != nil {
			return fmt.Errorf("service config is not ready: %w", err)
		}
		if err := stopWindowsService(service, windowsServiceWaitTimeout); err != nil {
			return err
		}
		if err := cleanStaleWindowsServiceRuntime(stdout, stderr); err != nil {
			return err
		}
		if err := startWindowsService(service, windowsServiceWaitTimeout); err != nil {
			return err
		}
		fmt.Fprintln(stdout, "Windows Service restarted; TUN is active")
	case "uninstall":
		if err := stopWindowsService(service, windowsServiceWaitTimeout); err != nil {
			return err
		}
		if err := cleanStaleWindowsServiceRuntime(stdout, stderr); err != nil {
			return err
		}
		if err := service.Delete(); err != nil {
			return fmt.Errorf("delete Windows Service: %w", err)
		}
		if err := eventlog.Remove(windowsServiceName); err != nil && !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
			fmt.Fprintf(stderr, "warning: remove Windows Event Log source: %v\n", err)
		}
		fmt.Fprintln(stdout, "Windows Service uninstalled; saved config and logs were retained")
	default:
		return fmt.Errorf("unsupported service command %q", command)
	}
	return nil
}

func cleanStaleWindowsServiceRuntime(stdout, stderr io.Writer) error {
	runtime := mactun.ReadWindowsRuntimeStatus()
	switch runtime.Status {
	case "stopped":
		return nil
	case "stale":
		fmt.Fprintln(stderr, "recovering stale TUN state before completing the service operation")
		if err := mactun.New(stdout, stderr).Down(); err != nil {
			return fmt.Errorf("clean stale Windows Service runtime: %w", err)
		}
		if remaining := mactun.ReadWindowsRuntimeStatus(); remaining.Status != "stopped" {
			return fmt.Errorf("TUN runtime is still %s after cleanup: %s", remaining.Status, remaining.Detail)
		}
		return nil
	case "active":
		return fmt.Errorf("SCM reports the Windows Service stopped, but another MacTun owner is active; refusing to stop or delete it")
	default:
		return fmt.Errorf("unexpected TUN runtime status %q", runtime.Status)
	}
}

func startWindowsService(service *mgr.Service, timeout time.Duration) error {
	status, err := service.Query()
	if err != nil {
		return err
	}
	if status.State == svc.Running {
		return nil
	}
	startIssued := false
	if status.State == svc.Stopped {
		if err := service.Start(); err != nil && !errors.Is(err, windows.ERROR_SERVICE_ALREADY_RUNNING) {
			return fmt.Errorf("start Windows Service: %w", err)
		}
		startIssued = true
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		status, err = service.Query()
		if err != nil {
			return err
		}
		if status.State == svc.Running {
			return nil
		}
		if status.State == svc.Stopped {
			if !startIssued {
				if err := service.Start(); err != nil && !errors.Is(err, windows.ERROR_SERVICE_ALREADY_RUNNING) {
					return fmt.Errorf("start Windows Service: %w", err)
				}
				startIssued = true
				time.Sleep(250 * time.Millisecond)
				continue
			}
			exitCode := status.ServiceSpecificExitCode
			if exitCode == 0 {
				exitCode = status.Win32ExitCode
			}
			return fmt.Errorf("Windows Service stopped during startup with exit code %d; see %s", exitCode, mactun.WindowsServiceLogPath())
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for Windows Service to start")
}

func stopWindowsService(service *mgr.Service, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	stopSent := false
	for time.Now().Before(deadline) {
		status, err := service.Query()
		if err != nil {
			return err
		}
		switch status.State {
		case svc.Stopped:
			if stopSent {
				exitCode := status.ServiceSpecificExitCode
				if exitCode == 0 {
					exitCode = status.Win32ExitCode
				}
				if exitCode != 0 {
					return fmt.Errorf("Windows Service stopped with exit code %d; cleanup may be incomplete, see %s", exitCode, mactun.WindowsServiceLogPath())
				}
			}
			return nil
		case svc.Running, svc.Paused:
			if !stopSent {
				if _, err := service.Control(svc.Stop); err != nil && !errors.Is(err, windows.ERROR_SERVICE_NOT_ACTIVE) {
					return fmt.Errorf("stop Windows Service: %w", err)
				}
				stopSent = true
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for Windows Service to stop; cleanup state was retained")
}

func windowsServiceStateName(state svc.State) string {
	switch state {
	case svc.Stopped:
		return "stopped"
	case svc.StartPending:
		return "starting"
	case svc.StopPending:
		return "stopping"
	case svc.Running:
		return "running"
	case svc.Paused:
		return "paused"
	default:
		return fmt.Sprintf("unknown-%d", state)
	}
}

func windowsServiceStartTypeName(startType uint32, delayed bool) string {
	switch startType {
	case mgr.StartAutomatic:
		if delayed {
			return "automatic-delayed"
		}
		return "automatic"
	case mgr.StartManual:
		return "manual"
	case mgr.StartDisabled:
		return "disabled"
	default:
		return fmt.Sprintf("unknown-%d", startType)
	}
}
