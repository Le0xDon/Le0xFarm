//go:build linux

// le0x-install installs the Ubuntu Technical MVP services without using a shell.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/le0xdon/le0xfarm/internal/linuxinstall"
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("le0x-install", flag.ContinueOnError)
	flags.SetOutput(stderr)
	action := flags.String("action", "install", "install or uninstall")
	roleValue := flags.String("role", "", "controller, agent, or both")
	root := flags.String("root", "/", "destination root; non-/ roots stage files without host mutation")
	controllerBinary := flags.String("controller-binary", "", "built le0x-controller source path")
	agentBinary := flags.String("agent-binary", "", "built le0x-agent source path")
	enable := flags.Bool("enable", false, "enable selected services")
	start := flags.Bool("start", false, "start selected services; requires --enable")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() != 0 || (*action != "install" && *action != "uninstall") {
		fmt.Fprintln(stderr, "expected --action install|uninstall and no positional arguments")
		return 2
	}
	roles, err := parseRoles(*roleValue)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	cleanRoot, err := filepath.Abs(*root)
	if err != nil || cleanRoot != filepath.Clean(*root) {
		fmt.Fprintln(stderr, "--root must be an absolute clean path")
		return 2
	}
	var host linuxinstall.Host = linuxinstall.LocalHost{}
	if cleanRoot != "/" {
		host = &linuxinstall.StagingHost{}
	} else if os.Geteuid() != 0 {
		fmt.Fprintln(stderr, "installation under / requires root")
		return 1
	}
	installer, err := linuxinstall.New(linuxinstall.Config{
		Root: cleanRoot, Roles: roles, ControllerBinary: *controllerBinary, AgentBinary: *agentBinary,
		Enable: *enable, Start: *start, Host: host,
	})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	ctx := context.Background()
	if *action == "install" {
		err = installer.Install(ctx)
	} else {
		if *enable || *start || *controllerBinary != "" || *agentBinary != "" {
			fmt.Fprintln(stderr, "uninstall does not accept install/binary options")
			return 2
		}
		err = installer.Uninstall(ctx)
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintf(stdout, "%s complete for %s; persistent data and configuration preserved\n", *action, *roleValue)
	return 0
}

func parseRoles(value string) ([]linuxinstall.Role, error) {
	switch strings.TrimSpace(value) {
	case "controller":
		return []linuxinstall.Role{linuxinstall.RoleController}, nil
	case "agent":
		return []linuxinstall.Role{linuxinstall.RoleAgent}, nil
	case "both":
		return []linuxinstall.Role{linuxinstall.RoleController, linuxinstall.RoleAgent}, nil
	default:
		return nil, errors.New("--role must be controller, agent, or both")
	}
}
