package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/le0xdon/le0xfarm/internal/agentidentity"
	"github.com/le0xdon/le0xfarm/internal/identity"
	"github.com/le0xdon/le0xfarm/internal/model"
	"github.com/le0xdon/le0xfarm/internal/packages"
)

func TestPackageImportOperatorPath(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("LE0X_DATA_DIR", filepath.Join(dir, "agent"))
	archive := filepath.Join(dir, "package.tar.gz")
	var buffer bytes.Buffer
	gz := gzip.NewWriter(&buffer)
	tw := tar.NewWriter(gz)
	data := []byte("#!/bin/sh\nexit 0\n")
	if err := tw.WriteHeader(&tar.Header{Name: "xmrig", Mode: 0755, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(archive, buffer.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(buffer.Bytes())
	id, _ := identity.ParsePackageID("package_11111111111111111111111111111111")
	manifest := packages.Manifest{PackageID: id, Name: "fixture", Version: "1", OS: "linux", Architecture: "amd64", ArchiveSHA256: fmt.Sprintf("%x", sum[:]), ExecutableRelativePath: "xmrig", SourceRepository: "https://example.invalid/source", SourceURL: "https://example.invalid/archive"}
	original := approvedPackageManifest
	approvedPackageManifest = func() packages.Manifest { return manifest }
	defer func() { approvedPackageManifest = original }()
	for _, args := range [][]string{{"--package-action", "import", "--package-archive", archive}, {"--package-action", "verify"}, {"--package-action", "list"}} {
		var stdout, stderr bytes.Buffer
		if code := run(args, strings.NewReader(""), &stdout, &stderr); code != 0 {
			t.Fatalf("args=%v code=%d stderr=%s", args, code, stderr.String())
		}
		if args[1] == "list" {
			var installed []packages.Installed
			if err := json.Unmarshal(stdout.Bytes(), &installed); err != nil || len(installed) != 1 || installed[0].Manifest.PackageID != id {
				t.Fatalf("output=%s err=%v", stdout.String(), err)
			}
		} else {
			var installed packages.Installed
			if err := json.Unmarshal(stdout.Bytes(), &installed); err != nil || installed.Manifest.PackageID != id {
				t.Fatalf("output=%s err=%v", stdout.String(), err)
			}
		}
	}
}

func TestPackageListIsEmptyBeforeImport(t *testing.T) {
	t.Setenv("LE0X_DATA_DIR", filepath.Join(t.TempDir(), "agent"))
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--package-action", "list"}, strings.NewReader(""), &stdout, &stderr); code != 0 || strings.TrimSpace(stdout.String()) != "[]" {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

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
	if code := run([]string{"--json"}, bytes.NewReader(nil), &stdout, &stderr); code != 1 {
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
		if got := run(tc.args, bytes.NewReader(nil), &stdout, &stderr); got != tc.code {
			t.Fatalf("exit %d, want %d", got, tc.code)
		}
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("argument handling created identity")
	}
}

func TestSecurePairingCLIRejectsArgvTokenAndRequiresFingerprint(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "agent")
	t.Setenv("LE0X_DATA_DIR", dir)
	var out, errOut bytes.Buffer
	if code := run([]string{"--controller", "127.0.0.1:1", "--pair", "secret"}, bytes.NewReader(nil), &out, &errOut); code != 1 || !strings.Contains(errOut.String(), "--pair-stdin") {
		t.Fatalf("argv token: code=%d err=%q", code, errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if code := run([]string{"--controller", "127.0.0.1:1", "--pair-stdin"}, strings.NewReader("secret\n"), &out, &errOut); code != 1 || !strings.Contains(errOut.String(), "--tls-fingerprint") {
		t.Fatalf("fingerprint: code=%d err=%q", code, errOut.String())
	}
}
