package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/le0xdon/le0xfarm/internal/controlleridentity"
)

func TestControllerSecureStartRequiresPKI(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "controller")
	t.Setenv("LE0X_CONTROLLER_DATA_DIR", dir)
	if _, err := controlleridentity.Initialize(dir); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := run([]string{"--listen", "127.0.0.1:0"}, &out, &errOut); code != 1 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(errOut.String(), "TLS_CREDENTIALS_REQUIRED") {
		t.Fatalf("stderr=%s", errOut.String())
	}
}

func TestControllerCLIInitializationFlag(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "controller")
	t.Setenv("LE0X_CONTROLLER_DATA_DIR", dir)
	var out, errOut bytes.Buffer
	if code := run([]string{"--listen", "127.0.0.1:0", "--insecure-dev"}, &out, &errOut); code != 1 || !strings.Contains(errOut.String(), "--init") {
		t.Fatalf("missing-init result: %d %s", code, errOut.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "identity.json")); !os.IsNotExist(err) {
		t.Fatal("identity created without --init")
	}
	if _, err := controlleridentity.Initialize(dir); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errOut.Reset()
	if code := run([]string{"--listen", "127.0.0.1:0", "--insecure-dev", "--init"}, &out, &errOut); code != 1 || !strings.Contains(errOut.String(), "already initialized") {
		t.Fatalf("repeat-init result: %d %s", code, errOut.String())
	}
}

func TestPairingTTLRequiresPairing(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run([]string{"--listen", "127.0.0.1:0", "--insecure-dev", "--pairing-ttl", "1m"}, &out, &errOut)
	if code != 2 || !strings.Contains(errOut.String(), "--pairing-ttl requires --pairing") {
		t.Fatalf("exit=%d stderr=%q", code, errOut.String())
	}
}

func TestBuildRuntimeCommand(t *testing.T) {
	id := "execution_0123456789abcdef0123456789abcdef"
	start, err := buildRuntimeCommand("start", id, "/bin/sleep", []string{"300"}, "", "ON_FAILURE")
	if err != nil {
		t.Fatal(err)
	}
	plan := start.GetStartExecution().GetPlan()
	if plan.ExecutionId != id || plan.Executable != "/bin/sleep" || len(plan.Args) != 1 || plan.Args[0] != "300" || plan.RestartPolicy != "ON_FAILURE" {
		t.Fatalf("start command=%v", start)
	}
	if command, err := buildRuntimeCommand("get", "", "", nil, "", "NEVER"); err != nil || command.GetGetExecutions() == nil {
		t.Fatalf("get command=%v error=%v", command, err)
	}
	if _, err := buildRuntimeCommand("start", "", "/bin/sleep", nil, "", "NEVER"); err == nil {
		t.Fatal("start without ExecutionID accepted")
	}
	if _, err := buildRuntimeCommand("", id, "", nil, "", "NEVER"); err == nil {
		t.Fatal("execution option without development action accepted")
	}
}
