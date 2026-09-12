//go:build linux

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStagedInstallAndUninstallCLI(t *testing.T) {
	root := t.TempDir()
	controller := cliBinary(t, "controller")
	agent := cliBinary(t, "agent")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--root", root, "--role", "both", "--controller-binary", controller, "--agent-binary", agent}, &stdout, &stderr); code != 0 {
		t.Fatalf("install exit=%d stderr=%s", code, stderr.String())
	}
	for _, path := range []string{"usr/local/bin/le0x-controller", "usr/local/bin/le0x-agent", "etc/systemd/system/le0x-controller.service", "etc/systemd/system/le0x-agent.service"} {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(path))); err != nil {
			t.Fatalf("missing staged path %s: %v", path, err)
		}
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"--action", "uninstall", "--root", root, "--role", "both"}, &stdout, &stderr); code != 0 {
		t.Fatalf("uninstall exit=%d stderr=%s", code, stderr.String())
	}
	if _, err := os.Stat(filepath.Join(root, "var/lib/le0xfarm/controller")); err != nil {
		t.Fatalf("persistent state directory removed: %v", err)
	}
}

func TestCLIRejectsUnsafeArgumentsWithoutWriting(t *testing.T) {
	root := t.TempDir()
	for _, args := range [][]string{
		{"--root", root, "--role", "unknown"},
		{"--root", "relative", "--role", "controller"},
		{"--root", root, "--role", "controller", "--start"},
		{"--action", "uninstall", "--root", root, "--role", "agent", "--agent-binary", "unexpected"},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(args, &stdout, &stderr); code == 0 || strings.TrimSpace(stderr.String()) == "" {
			t.Fatalf("unsafe args accepted: %v stdout=%q stderr=%q", args, stdout.String(), stderr.String())
		}
	}
}

func cliBinary(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "binary")
	if err := os.WriteFile(path, []byte(contents), 0755); err != nil {
		t.Fatal(err)
	}
	return path
}
