package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/le0xdon/le0xfarm/internal/controlleridentity"
	"github.com/le0xdon/le0xfarm/internal/controllerpki"
	"github.com/le0xdon/le0xfarm/internal/farmerr"
	"github.com/le0xdon/le0xfarm/internal/identity"
	le0xv1 "github.com/le0xdon/le0xfarm/proto/le0x/v1"
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

func TestControllerOpensFarmDatabase(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "controller")
	t.Setenv("LE0X_CONTROLLER_DATA_DIR", dir)
	if _, err := controlleridentity.Initialize(dir); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	// An invalid listen address lets startup reach database initialization and exit.
	if code := run([]string{"--listen", "invalid-address", "--insecure-dev"}, &out, &errOut); code != 1 {
		t.Fatalf("exit=%d stderr=%s", code, errOut.String())
	}
	info, err := os.Stat(filepath.Join(dir, "farm.db"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("farm.db permissions %o", info.Mode().Perm())
	}
}

func TestControllerLocalBackupCreateListAndVerify(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "controller")
	t.Setenv("LE0X_CONTROLLER_DATA_DIR", dir)
	controllerIdentity, err := controlleridentity.Initialize(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controllerpki.Initialize(dir, controllerIdentity.ControllerID, controllerIdentity.FarmID); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := run([]string{"--backup-action", "create"}, &out, &errOut); code != 0 {
		t.Fatalf("create exit=%d stderr=%s", code, errOut.String())
	}
	var backupID string
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.HasPrefix(line, "BackupID: ") {
			backupID = strings.TrimPrefix(line, "BackupID: ")
		}
	}
	if backupID == "" {
		t.Fatalf("create output=%s", out.String())
	}
	out.Reset()
	errOut.Reset()
	if code := run([]string{"--backup-action", "list"}, &out, &errOut); code != 0 || !strings.Contains(out.String(), backupID) {
		t.Fatalf("list exit=%d stdout=%s stderr=%s", code, out.String(), errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if code := run([]string{"--backup-action", "verify", "--backup-id", backupID}, &out, &errOut); code != 0 || !strings.Contains(out.String(), "Code: VERIFIED") {
		t.Fatalf("verify exit=%d stdout=%s stderr=%s", code, out.String(), errOut.String())
	}
	if err := os.Rename(filepath.Join(dir, controllerpki.DirName), filepath.Join(dir, "pki-missing")); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errOut.Reset()
	if code := run([]string{"--backup-action", "restore", "--backup-id", backupID}, &out, &errOut); code != 1 || !strings.Contains(errOut.String(), string(farmerr.RESTORE_IDENTITY_MISMATCH)) {
		t.Fatalf("restore without trust context exit=%d stdout=%s stderr=%s", code, out.String(), errOut.String())
	}
}

func TestPairingTTLRequiresPairing(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run([]string{"--listen", "127.0.0.1:0", "--insecure-dev", "--pairing-ttl", "1m"}, &out, &errOut)
	if code != 2 || !strings.Contains(errOut.String(), "--pairing-ttl requires --pairing") {
		t.Fatalf("exit=%d stderr=%q", code, errOut.String())
	}
}

func TestPairingRequiresDedicatedCredentialFile(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"--listen", "127.0.0.1:0", "--insecure-dev", "--pairing"}, &out, &errOut); code != 2 || !strings.Contains(errOut.String(), "--pairing-token-file") {
		t.Fatalf("exit=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
}

func TestEnrollmentCredentialFileIsExclusiveAndSecretSafe(t *testing.T) {
	token := "synthetic-one-time-token"
	path := filepath.Join(t.TempDir(), "enrollment.token")
	if err := writeEnrollmentCredential(path, token); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != token+"\n" || info.Mode().Perm() != 0600 {
		t.Fatalf("credential file mode=%o content=%q", info.Mode().Perm(), data)
	}
	if err := writeEnrollmentCredential(path, "replacement"); err == nil {
		t.Fatal("credential file was overwritten")
	}
	var operational bytes.Buffer
	fmt.Fprintf(&operational, "Pairing: ENABLED\nEnrollment credential file: %s\n", path)
	if strings.Contains(operational.String(), token) {
		t.Fatal("enrollment token entered operational output")
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

func TestDevelopmentStartGetsProtocolV3Ownership(t *testing.T) {
	command, err := buildRuntimeCommand("start", "execution_0123456789abcdef0123456789abcdef", "/bin/sleep", []string{"1"}, "", "NEVER")
	if err != nil {
		t.Fatal(err)
	}
	hostID, _ := identity.ParseHostID("host_0123456789abcdef0123456789abcdef")
	if err := attachDevOwnership(command, hostID); err != nil {
		t.Fatal(err)
	}
	owner := command.GetStartExecution().GetPlan().GetOwnership()
	if owner.GetHostId() != hostID.String() || owner.GetDesiredGeneration() != 1 || !owner.GetResourceClaim().GetCpu() || owner.GetWorkloadId() == "" || len(owner.GetResolvedHash()) != 71 {
		t.Fatalf("invalid development ownership: %+v", owner)
	}
}

func TestBuildGenericMinerRuntimeCommand(t *testing.T) {
	id := "execution_0123456789abcdef0123456789abcdef"
	command, err := buildMinerRuntimeCommand(id, "test-no-http", "package_0123456789abcdef0123456789abcdef", "1.0", "STRESS", "", "test-algorithm", 2, "ON_FAILURE", nil)
	if err != nil {
		t.Fatal(err)
	}
	plan := command.GetStartExecution().GetPlan()
	if plan.GetExecutable() != "" || plan.GetMiner().GetAdapterId() != "test-no-http" || plan.GetMiner().GetPackageId() == "" || plan.GetMiner().GetCpuThreads() != 2 || plan.GetMiner().GetMode() != "STRESS" {
		t.Fatalf("miner command=%v", command)
	}
	if _, err := buildMinerRuntimeCommand(id, "", "package_0123456789abcdef0123456789abcdef", "6.26.0", "STRESS", "", "", 2, "NEVER", nil); err == nil {
		t.Fatal("missing adapter accepted")
	}
}

func TestBuildMiningCommandPreservesResolvedEndpoint(t *testing.T) {
	id := "execution_0123456789abcdef0123456789abcdef"
	endpoint := &le0xv1.MiningEndpoint{Address: "pool.example:443", Tls: true, User: "public-login", Password: "credential-value", Worker: "worker-1"}
	command, err := buildMinerRuntimeCommand(id, "test-no-http", "package_0123456789abcdef0123456789abcdef", "1.0", "MINING", "ZEPH", "rx/0", 2, "ON_FAILURE", endpoint)
	if err != nil {
		t.Fatal(err)
	}
	got := command.GetStartExecution().GetPlan().GetMiner().GetEndpoint()
	if got.GetAddress() != endpoint.Address || !got.GetTls() || got.GetUser() != endpoint.User || got.GetPassword() != endpoint.Password || got.GetWorker() != endpoint.Worker {
		t.Fatalf("endpoint was not preserved: %+v", got)
	}
	t.Setenv("LE0X_CONTROLLER_DATA_DIR", filepath.Join(t.TempDir(), "missing"))
	var out, errOut bytes.Buffer
	if code := runWithInput([]string{"--dev-runtime-action", "start-miner", "--execution-id", id, "--miner-adapter", "test-no-http", "--package-id", "package_0123456789abcdef0123456789abcdef", "--package-version", "1.0", "--miner-mode", "MINING", "--pool-address", "pool.example:443", "--pool-user", "public-login", "--pool-password-stdin", "--target-agent", "agent_0123456789abcdef0123456789abcdef", "--insecure-dev"}, strings.NewReader("credential-value\n"), &out, &errOut); code != 1 {
		t.Fatalf("expected missing identity failure, got %d", code)
	}
	if strings.Contains(out.String()+errOut.String(), "credential-value") {
		t.Fatal("pool credential leaked through CLI output")
	}
}
