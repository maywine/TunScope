//go:build darwin

package mactun

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

type commandRunner interface {
	Run(name string, args ...string) (string, error)
}

type execRunner struct{}

func (execRunner) Run(name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return out.String(), fmt.Errorf("%s timed out", name)
	}
	if err != nil {
		msg := strings.TrimSpace(out.String())
		if msg == "" {
			return out.String(), fmt.Errorf("%s: %w", name, err)
		}
		return out.String(), fmt.Errorf("%s: %w: %s", name, err, msg)
	}
	return out.String(), nil
}

func parseRouteGet(output string) (gateway, iface string, err error) {
	for _, line := range strings.Split(output, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok {
			continue
		}
		switch strings.TrimSpace(key) {
		case "gateway":
			gateway = strings.TrimSpace(value)
		case "interface":
			iface = strings.TrimSpace(value)
		}
	}
	if gateway == "" || iface == "" {
		return "", "", fmt.Errorf("could not parse the default route")
	}
	return gateway, iface, nil
}

func defaultRoute4(r commandRunner) (gateway, iface string, err error) {
	out, err := r.Run("/sbin/route", "-n", "get", "default")
	if err != nil {
		return "", "", err
	}
	return parseRouteGet(out)
}

func defaultRoute6(r commandRunner) (gateway, iface string, err error) {
	out, err := r.Run("/sbin/route", "-n", "get", "-inet6", "default")
	if err != nil {
		return "", "", err
	}
	return parseRouteGet(out)
}

func routeArgs(action string, route Route) []string {
	args := []string{"-n", action}
	if route.Family == "inet6" {
		args = append(args, "-inet6")
	}
	args = append(args, "-"+route.Kind)
	if route.Scope != "" {
		args = append(args, "-ifscope", route.Scope)
	}
	// RTM_CHANGE does not reliably refresh the source address cached by an
	// interface-scoped route after DHCP replaces an address. Tell XNU which
	// current interface address to attach to every add/change. A delete must
	// not carry -ifa: the recorded address may already have been removed.
	if action != "delete" && route.Source != "" {
		args = append(args, "-ifa", route.Source)
	}
	args = append(args, route.Target)
	if route.Interface != "" {
		args = append(args, "-interface", route.Interface)
	} else {
		args = append(args, route.Gateway)
	}
	return args
}

func addRoute(r commandRunner, route Route) error {
	_, err := r.Run("/sbin/route", routeArgs("add", route)...)
	return err
}

func deleteRoute(r commandRunner, route Route) error {
	_, err := r.Run("/sbin/route", routeArgs("delete", route)...)
	return err
}

func changeRoute(r commandRunner, route Route) error {
	_, err := r.Run("/sbin/route", routeArgs("change", route)...)
	return err
}
