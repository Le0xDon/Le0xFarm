package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/le0xdon/le0xfarm/internal/agentidentity"
	"github.com/le0xdon/le0xfarm/internal/identity"
	"github.com/le0xdon/le0xfarm/internal/model"
)

func TestPresentationJSON(t *testing.T) {
	host, err := identity.NewHostID()
	if err != nil {
		t.Fatal(err)
	}
	agent, err := identity.NewAgentID()
	if err != nil {
		t.Fatal(err)
	}
	original := report{Identity: agentidentity.Identity{HostID: host, AgentID: agent}, Inventory: model.Inventory{
		Host:   model.Host{HostID: host, Hostname: "host\"name", OS: model.OSInfo{ID: "ubuntu", PrettyName: "Ubuntu test"}, Architecture: "amd64"},
		Memory: model.Memory{TotalBytes: 2 << 30}, GPUs: []model.GPU{},
	}, Warnings: []string{"test warning"}}
	var output bytes.Buffer
	if err := present(&output, original, true); err != nil {
		t.Fatal(err)
	}
	var decoded report
	if err := json.Unmarshal(output.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Identity != original.Identity || decoded.Inventory.Host != original.Inventory.Host || decoded.Inventory.Memory != original.Inventory.Memory || len(decoded.Warnings) != 1 {
		t.Fatal("JSON fields changed")
	}
	if strings.Contains(output.String(), "Le0xAgent\n") {
		t.Fatal("human output mixed into JSON")
	}
	output.Reset()
	if err := present(&output, original, false); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Le0xAgent", host.String(), agent.String(), "Ubuntu test", "2147483648 bytes", "Warning: test warning"} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("missing %s", want)
		}
	}
}

func TestCLIErrorDoesNotReplaceIdentity(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LE0X_DATA_DIR", dir)
	path := filepath.Join(dir, agentidentity.FileName)
	if err := os.WriteFile(path, []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--json"}, &stdout, &stderr); code != 1 {
		t.Fatalf("exit %d", code)
	}
	if stdout.Len() != 0 {
		t.Fatal("success output on failure")
	}
	var failure struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(stderr.Bytes(), &failure); err != nil || failure.Code != "CONFIG_CONFLICT" {
		t.Fatalf("invalid error JSON: %s %v", stderr.String(), err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "corrupt" {
		t.Fatal("identity replaced")
	}
}

func TestCLIHelpAndBadFlagsDoNotBootstrap(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "not-created")
	t.Setenv("LE0X_DATA_DIR", dir)
	for _, tc := range []struct {
		args []string
		code int
	}{{[]string{"--help"}, 0}, {[]string{"--unknown"}, 2}, {[]string{"unexpected"}, 2}} {
		var stdout, stderr bytes.Buffer
		if got := run(tc.args, &stdout, &stderr); got != tc.code {
			t.Fatalf("exit %d, want %d", got, tc.code)
		}
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("argument handling created identity")
	}
}
