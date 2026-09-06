package inventory

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/le0xdon/le0xfarm/internal/agentidentity"
	"github.com/le0xdon/le0xfarm/internal/identity"
)

func fixture() Source {
	cpu := ""
	for i := 0; i < 8; i++ {
		cpu += fmt.Sprintf("processor: %d\nvendor_id: TestVendor\nmodel name: TestCPU\nphysical id: %d\ncore id: %d\n\n", i, i/4, (i%4)/2)
	}
	return Source{Files: fstest.MapFS{
		"etc/os-release":            {Data: []byte("ID=ubuntu\nVERSION_ID=\"99.04\"\nPRETTY_NAME='Ubuntu future LTS'\n")},
		"proc/sys/kernel/osrelease": {Data: []byte("test-kernel\n")},
		"proc/cpuinfo":              {Data: []byte(cpu)},
		"proc/meminfo":              {Data: []byte("MemTotal: 2097152 kB\nMemFree: 1024 kB\n")},
	}, Hostname: func() (string, error) { return "same-hostname", nil }, Architecture: "amd64"}
}

func TestLinuxInventory(t *testing.T) {
	host, err := identity.NewHostID()
	if err != nil {
		t.Fatal(err)
	}
	got, warnings := fixture().Discover(host)
	if len(warnings) != 0 {
		t.Fatal(warnings)
	}
	if got.Host.HostID != host || got.Host.Hostname != "same-hostname" || got.Host.Architecture != "amd64" {
		t.Fatal(got.Host)
	}
	if got.Host.OS.ID != "ubuntu" || got.Host.OS.VersionID != "99.04" || got.Host.OS.PrettyName != "Ubuntu future LTS" || got.Host.OS.Kernel != "test-kernel" {
		t.Fatal(got.Host.OS)
	}
	if got.CPU.Vendor != "TestVendor" || got.CPU.Model != "TestCPU" || got.CPU.Sockets != 2 || got.CPU.Cores != 4 || got.CPU.Threads != 8 {
		t.Fatal(got.CPU)
	}
	if got.Memory.TotalBytes != 2<<30 || len(got.GPUs) != 0 {
		t.Fatal(got)
	}
}

func TestHostnameIndependentPersistence(t *testing.T) {
	source := fixture()
	dir := filepath.Join(t.TempDir(), "agent")
	first, err := agentidentity.LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	one, _ := source.Discover(first.HostID)
	second, err := agentidentity.LoadOrCreate(filepath.Join(t.TempDir(), "agent"))
	if err != nil {
		t.Fatal(err)
	}
	two, _ := source.Discover(second.HostID)
	if one.Host.Hostname != two.Host.Hostname || one.Host.HostID == two.Host.HostID || first.AgentID == second.AgentID {
		t.Fatal("hostname determined identity")
	}
	before, err := os.ReadFile(filepath.Join(dir, agentidentity.FileName))
	if err != nil {
		t.Fatal(err)
	}
	source.Hostname = func() (string, error) { return "renamed-host", nil }
	again, err := agentidentity.LoadOrCreate(dir)
	if err != nil || first != again {
		t.Fatalf("identity changed after rename: %v", err)
	}
	renamed, _ := source.Discover(again.HostID)
	if renamed.Host.Hostname != "renamed-host" || renamed.Host.HostID != first.HostID {
		t.Fatal("rename changed host identity")
	}
	after, err := os.ReadFile(filepath.Join(dir, agentidentity.FileName))
	if err != nil || string(before) != string(after) {
		t.Fatal("discovery modified identity file")
	}
}

func TestUnavailableFactsAndFallback(t *testing.T) {
	source := fixture()
	files := source.Files.(fstest.MapFS)
	files["usr/lib/os-release"] = files["etc/os-release"]
	delete(files, "etc/os-release")
	host, err := identity.NewHostID()
	if err != nil {
		t.Fatal(err)
	}
	got, warnings := source.Discover(host)
	if len(warnings) != 0 || got.Host.OS.ID != "ubuntu" {
		t.Fatalf("fallback failed: %v", warnings)
	}
	source.Files = fstest.MapFS{}
	source.Hostname = func() (string, error) { return "", errors.New("hostname unavailable") }
	got, warnings = source.Discover(host)
	if len(warnings) != 5 || got.Host.HostID != host || got.Memory.TotalBytes != 0 {
		t.Fatalf("partial inventory: %v %v", got, warnings)
	}
}

func TestCPUUnknownTopology(t *testing.T) {
	got, err := parseCPU("processor: 0\nmodel name: VM CPU\n\nprocessor: 1\nmodel name: VM CPU\n")
	if err == nil || got.Threads != 2 || got.Cores != 0 || got.Sockets != 0 {
		t.Fatalf("invented topology: %v %v", got, err)
	}
	for _, text := range []string{"", "processor: invalid", "processor: 0\n\nprocessor: 0"} {
		if _, err := parseCPU(text); err == nil {
			t.Fatalf("invalid CPU data accepted: %q", text)
		}
	}
}

func TestMemoryValidation(t *testing.T) {
	for _, text := range []string{"", "MemTotal: -1 kB", "MemTotal: 0 kB", "MemTotal: 10 MB", "MemTotal: 18446744073709551615 kB", "MemTotal: garbage kB"} {
		if _, err := parseMemory(text); err == nil {
			t.Fatalf("invalid memory accepted: %q", text)
		}
	}
}

func TestOSReleaseQuoting(t *testing.T) {
	values, err := parseOSRelease("# comment\nID=ubuntu\nVERSION_ID='24.04'\nPRETTY_NAME=\"Ubuntu \\\"LTS\\\" \\$HOME $(not-executed)\"\n")
	if err != nil || values["PRETTY_NAME"] != `Ubuntu "LTS" $HOME $(not-executed)` {
		t.Fatalf("quoting: %v %v", values, err)
	}
	values, err = parseOSRelease("ID=ubuntu\nPRETTY_NAME=\"unterminated\nVERSION_ID=26.04\n")
	if err == nil || values["ID"] != "ubuntu" || values["VERSION_ID"] != "26.04" {
		t.Fatal("partial OS facts lost")
	}
	for _, raw := range []string{`"broken`, `trailing\`, `unquoted space`} {
		if _, err := releaseValue(raw); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	if strings.Contains(values["PRETTY_NAME"], "unterminated") {
		t.Fatal("invalid field retained")
	}
}
