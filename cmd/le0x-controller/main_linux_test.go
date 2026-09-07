package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/le0xdon/le0xfarm/internal/controlleridentity"
)

func TestControllerCLIRefusesPlaintextByDefault(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"--listen", "127.0.0.1:0"}, &out, &errOut); code != 1 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(errOut.String(), "--insecure-dev") {
		t.Fatalf("missing explicit development warning: %s", errOut.String())
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
