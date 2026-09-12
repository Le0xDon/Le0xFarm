// Le0xController owns persistent desired state and authenticated Agent reconciliation.
package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/le0xdon/le0xfarm/internal/controllerbackup"
	"github.com/le0xdon/le0xfarm/internal/controllerdb"
	"github.com/le0xdon/le0xfarm/internal/controlleridentity"
	"github.com/le0xdon/le0xfarm/internal/controllerlock"
	"github.com/le0xdon/le0xfarm/internal/controllernet"
	"github.com/le0xdon/le0xfarm/internal/controllerpki"
	"github.com/le0xdon/le0xfarm/internal/controllerstate"
	"github.com/le0xdon/le0xfarm/internal/controllertrust"
	"github.com/le0xdon/le0xfarm/internal/farmconfig"
	"github.com/le0xdon/le0xfarm/internal/farmerr"
	"github.com/le0xdon/le0xfarm/internal/identity"
	"github.com/le0xdon/le0xfarm/internal/packagecatalog"
	"github.com/le0xdon/le0xfarm/internal/reconcile"
	le0xv1 "github.com/le0xdon/le0xfarm/proto/le0x/v1"
)

type stringList []string

func (values *stringList) String() string { return fmt.Sprint([]string(*values)) }
func (values *stringList) Set(value string) error {
	*values = append(*values, value)
	return nil
}

func main() { os.Exit(runWithInput(os.Args[1:], os.Stdin, os.Stdout, os.Stderr)) }

func run(args []string, stdout, stderr io.Writer) int {
	return runWithInput(args, strings.NewReader(""), stdout, stderr)
}

func runWithInput(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("le0x-controller", flag.ContinueOnError)
	flags.SetOutput(stderr)
	listen := flags.String("listen", "127.0.0.1:50051", "TCP listen address")
	insecureDev := flags.Bool("insecure-dev", false, "Allow plaintext gRPC for development/test only")
	initIdentity := flags.Bool("init", false, "Initialize a new Controller/Farm identity")
	initPKI := flags.Bool("init-pki", false, "Initialize PKI for an existing Controller identity")
	pairing := flags.Bool("pairing", false, "Enable temporary development enrollment pairing")
	pairingTTL := flags.Duration("pairing-ttl", 15*time.Minute, "Enrollment token lifetime")
	pairingTokenFile := flags.String("pairing-token-file", "", "Create a mode-0600 file containing the one-time enrollment token")
	backupAction := flags.String("backup-action", "", "Local backup action: create, list, verify, or restore")
	backupID := flags.String("backup-id", "", "BackupID for verify or restore")
	backupDir := flags.String("backup-dir", "", "Optional local Controller backup directory")
	runtimeAction := flags.String("dev-runtime-action", "", "Development/test runtime action: start, start-miner, stop, restart, or get")
	runtimeTarget := flags.String("target-agent", "", "AgentID targeted by a development/test runtime action")
	executionID := flags.String("execution-id", "", "ExecutionID for a development/test runtime action")
	executable := flags.String("executable", "", "Absolute executable path for a development/test start")
	workingDirectory := flags.String("working-directory", "", "Working directory for a development/test start")
	restartPolicy := flags.String("restart-policy", "NEVER", "Restart policy for a development/test start: NEVER or ON_FAILURE")
	minerAdapter := flags.String("miner-adapter", "", "Miner adapter ID for start-miner")
	packageID := flags.String("package-id", "", "Installed PackageID for start-miner")
	packageVersion := flags.String("package-version", "", "Installed package version for start-miner")
	minerMode := flags.String("miner-mode", "", "Miner mode for start-miner")
	coin := flags.String("coin", "", "Optional resolved coin for start-miner")
	algorithm := flags.String("algorithm", "", "Optional miner algorithm")
	cpuThreads := flags.Uint("cpu-threads", 0, "CPU threads for start-miner")
	poolAddress := flags.String("pool-address", "", "Resolved pool host:port for start-miner")
	poolTLS := flags.Bool("pool-tls", false, "Require TLS for the resolved pool connection")
	poolUser := flags.String("pool-user", "", "Exact public pool login for start-miner")
	poolWorker := flags.String("pool-worker", "", "Optional separate pool worker identity")
	poolPasswordStdin := flags.Bool("pool-password-stdin", false, "Read a potentially sensitive pool password from one stdin line")
	var executionArgs stringList
	flags.Var(&executionArgs, "execution-arg", "Argument for a development/test start; repeat for multiple argv entries")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "Unexpected positional arguments")
		return 2
	}
	setTTL := false
	flags.Visit(func(f *flag.Flag) {
		if f.Name == "pairing-ttl" {
			setTTL = true
		}
	})
	if setTTL && !*pairing {
		fmt.Fprintln(stderr, "--pairing-ttl requires --pairing")
		return 2
	}
	if *pairing && *pairingTokenFile == "" {
		fmt.Fprintln(stderr, "--pairing requires --pairing-token-file; credentials are never written to operational output")
		return 2
	}
	if !*pairing && *pairingTokenFile != "" {
		fmt.Fprintln(stderr, "--pairing-token-file requires --pairing")
		return 2
	}
	if *initIdentity && *initPKI {
		fmt.Fprintln(stderr, "--init and --init-pki cannot be combined")
		return 2
	}
	if *backupAction == "" && (*backupID != "" || *backupDir != "") {
		fmt.Fprintln(stderr, "--backup-id and --backup-dir require --backup-action")
		return 2
	}
	if *backupAction != "" && (*initIdentity || *initPKI || *pairing || *runtimeAction != "" || *runtimeTarget != "") {
		fmt.Fprintln(stderr, "backup actions cannot be combined with initialization, pairing, or runtime actions")
		return 2
	}
	var runtimeCommand *le0xv1.CommandEnvelope
	var err error
	if *runtimeAction == "start-miner" {
		if uint64(*cpuThreads) > math.MaxUint32 {
			fmt.Fprintln(stderr, "--cpu-threads exceeds uint32")
			return 2
		}
		poolPassword := ""
		if *poolPasswordStdin {
			poolPassword, err = readOneLine(stdin)
			if err != nil {
				fmt.Fprintln(stderr, "cannot read pool password from stdin")
				return 2
			}
		}
		runtimeCommand, err = buildMinerRuntimeCommand(*executionID, *minerAdapter, *packageID, *packageVersion, *minerMode, *coin, *algorithm, uint32(*cpuThreads), *restartPolicy, endpointFromFlags(*poolAddress, *poolTLS, *poolUser, poolPassword, *poolWorker))
	} else {
		if *minerAdapter != "" || *packageID != "" || *packageVersion != "" || *minerMode != "" || *coin != "" || *algorithm != "" || *cpuThreads != 0 || *poolAddress != "" || *poolTLS || *poolUser != "" || *poolWorker != "" || *poolPasswordStdin {
			fmt.Fprintln(stderr, "miner options require --dev-runtime-action start-miner")
			return 2
		}
		runtimeCommand, err = buildRuntimeCommand(*runtimeAction, *executionID, *executable, []string(executionArgs), *workingDirectory, *restartPolicy)
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	var targetAgent identity.AgentID
	if runtimeCommand != nil {
		targetAgent, err = identity.ParseAgentID(*runtimeTarget)
		if err != nil {
			fmt.Fprintln(stderr, "runtime action requires a valid --target-agent")
			return 2
		}
	} else if *runtimeTarget != "" {
		fmt.Fprintln(stderr, "--target-agent requires --dev-runtime-action")
		return 2
	}
	dataDir, err := controlleridentity.DataDir()
	if err != nil {
		printError(stderr, err)
		return 1
	}
	var pairingWindow *controllernet.PairingWindow
	var token string
	var expiry time.Time
	if *pairing {
		pairingWindow, token, expiry, err = controllernet.NewPairingWindow(*pairingTTL, nil)
		if err != nil {
			printError(stderr, err)
			return 1
		}
	}
	var controllerIdentity controlleridentity.Identity
	if *initIdentity {
		controllerIdentity, err = controlleridentity.Initialize(dataDir)
	} else {
		controllerIdentity, err = controlleridentity.Load(dataDir)
	}
	if err != nil {
		printError(stderr, err)
		return 1
	}
	lease, err := controllerlock.Acquire(dataDir)
	if err != nil {
		printError(stderr, err)
		return 1
	}
	defer lease.Close()
	var pki *controllerpki.PKI
	if *initIdentity || *initPKI {
		pki, err = controllerpki.Initialize(dataDir, controllerIdentity.ControllerID, controllerIdentity.FarmID)
	} else if *backupAction != "" || !*insecureDev {
		pki, err = controllerpki.Load(dataDir, controllerIdentity.ControllerID, controllerIdentity.FarmID)
	} else if existing, loadErr := controllerpki.Load(dataDir, controllerIdentity.ControllerID, controllerIdentity.FarmID); loadErr == nil {
		pki = existing
	}
	if err != nil {
		if *backupAction == "restore" {
			printError(stderr, farmerr.Error{Code: farmerr.RESTORE_IDENTITY_MISMATCH, HumanMessage: "restore requires the original Controller cryptographic trust context"})
			return 1
		}
		printError(stderr, err)
		return 1
	}
	// Recover or fail closed on an interrupted offline restore before any code
	// opens farm.db. Public PKI provenance is loaded first so a crash recovery
	// cannot accept a barriered database from a different trust context.
	trustFingerprint := ""
	if pki != nil {
		trustFingerprint = pki.CAFingerprint()
	}
	if err := controllerbackup.RecoverInterruptedRestore(context.Background(), dataDir, lease, controllerIdentity.ControllerID, controllerIdentity.FarmID, trustFingerprint); err != nil {
		printError(stderr, err)
		return 1
	}
	if *backupAction != "" {
		return runBackupAction(*backupAction, *backupID, *backupDir, dataDir, controllerIdentity, trustFingerprint, lease, stdout, stderr)
	}
	trust, err := controllertrust.Open(dataDir, controllerIdentity.ControllerID, controllerIdentity.FarmID)
	if err != nil {
		printError(stderr, err)
		return 1
	}
	if runtimeCommand != nil && runtimeCommand.GetStartExecution() != nil {
		record, paired := trust.Find(targetAgent)
		if !paired {
			printError(stderr, farmerr.Error{Code: farmerr.PAIRING_REQUIRED, HumanMessage: "development START requires an already paired target Agent"})
			return 1
		}
		if err := attachDevOwnership(runtimeCommand, record.HostID); err != nil {
			printError(stderr, err)
			return 1
		}
	}
	farmDB, err := controllerdb.Open(context.Background(), dataDir)
	if err != nil {
		printError(stderr, err)
		return 1
	}
	defer farmDB.Close()
	catalog, err := packagecatalog.Builtin()
	if err != nil {
		printError(stderr, err)
		return 1
	}
	farmService, err := farmconfig.New(farmDB, catalog, farmconfig.Options{})
	if err != nil {
		printError(stderr, err)
		return 1
	}
	observed := controllerstate.New()
	listener, err := net.Listen("tcp", *listen)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	defer listener.Close()
	var runtimeCommands []*le0xv1.CommandEnvelope
	if runtimeCommand != nil {
		runtimeCommands = append(runtimeCommands, runtimeCommand)
	}
	server, err := controllernet.New(controllernet.Config{ListenAddress: *listen, InsecureDev: *insecureDev, ControllerID: controllerIdentity.ControllerID, FarmID: controllerIdentity.FarmID, Trust: trust, Pairing: pairingWindow, PKI: pki, RuntimeCommands: runtimeCommands, RuntimeTarget: targetAgent, Maintenance: farmService, Output: log.New(stdout, "", 0)})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	coordinator := reconcile.NewCoordinator(farmService, observed, server, log.New(stdout, "", 0))
	server.SetSessionHandler(coordinator)
	controllerID, farmID := server.IDs()
	if *pairing {
		if err := writeEnrollmentCredential(*pairingTokenFile, token); err != nil {
			printError(stderr, err)
			return 1
		}
	}
	security := "TLS 1.3 MUTUAL TLS"
	if *insecureDev {
		security = "INSECURE DEVELOPMENT MODE"
	}
	fmt.Fprintf(stdout, "Le0xController\nControllerID: %s\nFarmID: %s\nListening: %s\nSecurity: %s\n", controllerID, farmID, listener.Addr(), security)
	if *pairing {
		if *insecureDev {
			fmt.Fprintf(stdout, "Pairing: ENABLED — DEVELOPMENT MODE\nEnrollment credential file: %s\nExpires: %s\n", *pairingTokenFile, expiry.Format(time.RFC3339))
		} else {
			fmt.Fprintf(stdout, "Pairing: ENABLED\nEnrollment credential file: %s\nTLS fingerprint: %s\nExpires: %s\n", *pairingTokenFile, pki.ServerFingerprint(), expiry.Format(time.RFC3339))
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if pki != nil {
		backupManager, err := controllerbackup.New(controllerbackup.Config{DataDir: dataDir, ControllerID: controllerIdentity.ControllerID, FarmID: controllerIdentity.FarmID, TrustFingerprint: pki.CAFingerprint(), Lease: lease})
		if err != nil {
			printError(stderr, err)
			return 1
		}
		go backupManager.Run(ctx, farmDB, time.Hour, func(code farmerr.Code) {
			log.New(stdout, "", 0).Printf("BACKUP: code=%s", code)
		})
	} else {
		log.New(stdout, "", 0).Printf("BACKUP: code=%s", farmerr.BACKUP_FAILED)
	}
	go coordinator.Run(ctx, time.Second)
	if err := server.Serve(ctx, listener); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

func runBackupAction(action, idValue, backupDir, dataDir string, controllerIdentity controlleridentity.Identity, trustFingerprint string, lease *controllerlock.Lease, stdout, stderr io.Writer) int {
	manager, err := controllerbackup.New(controllerbackup.Config{DataDir: dataDir, BackupDir: backupDir, ControllerID: controllerIdentity.ControllerID, FarmID: controllerIdentity.FarmID, TrustFingerprint: trustFingerprint, Lease: lease})
	if err != nil {
		printError(stderr, err)
		return 1
	}
	ctx := context.Background()
	parseID := func() (controllerbackup.BackupID, bool) {
		id, parseErr := controllerbackup.ParseBackupID(idValue)
		if parseErr != nil {
			fmt.Fprintln(stderr, "verify and restore require a valid --backup-id")
			return "", false
		}
		return id, true
	}
	switch action {
	case "create":
		if idValue != "" {
			fmt.Fprintln(stderr, "create does not accept --backup-id")
			return 2
		}
		db, openErr := controllerdb.Open(ctx, dataDir)
		if openErr != nil {
			printError(stderr, openErr)
			return 1
		}
		result, createErr := manager.CreateManual(ctx, db)
		closeErr := db.Close()
		if err := errors.Join(createErr, closeErr); err != nil {
			printError(stderr, err)
			return 1
		}
		fmt.Fprintf(stdout, "Code: %s\nBackupID: %s\nCreated: %s\nClass: %s\nSchema: %d\n", result.Code, result.Manifest.BackupID, result.Manifest.CreatedAt.Format(time.RFC3339), result.Manifest.Class, result.Manifest.DatabaseSchema)
		return 0
	case "list":
		if idValue != "" {
			fmt.Fprintln(stderr, "list does not accept --backup-id")
			return 2
		}
		values, listErr := manager.List(ctx)
		if listErr != nil {
			printError(stderr, listErr)
			return 1
		}
		for _, item := range values {
			fmt.Fprintf(stdout, "%s %s %s schema=%d bytes=%d\n", item.BackupID, item.Class, item.CreatedAt.Format(time.RFC3339), item.DatabaseSchema, item.DatabaseSize)
		}
		return 0
	case "verify":
		id, ok := parseID()
		if !ok {
			return 2
		}
		manifest, verifyErr := manager.Verify(ctx, id)
		if verifyErr != nil {
			printError(stderr, verifyErr)
			return 1
		}
		fmt.Fprintf(stdout, "Code: VERIFIED\nBackupID: %s\nChecksum: %s\nSchema: %d\n", manifest.BackupID, manifest.DatabaseSHA256, manifest.DatabaseSchema)
		return 0
	case "restore":
		id, ok := parseID()
		if !ok {
			return 2
		}
		result, restoreErr := manager.Restore(ctx, id, lease)
		if restoreErr != nil {
			printError(stderr, restoreErr)
			return 1
		}
		fmt.Fprintf(stdout, "Code: %s\nBackupID: %s\nSafetyBackupID: %s\nHostBarriers: %d\nRestartRequired: %t\n", result.Code, result.Backup.BackupID, result.SafetyBackupID, result.HostBarriers, result.RestartRequired)
		return 0
	default:
		fmt.Fprintln(stderr, "--backup-action must be create, list, verify, or restore")
		return 2
	}
}

func writeEnrollmentCredential(path, token string) error {
	if path == "" || token == "" || strings.ContainsAny(token, "\r\n") {
		return farmerr.Error{Code: farmerr.CONFIG_CONFLICT, HumanMessage: "invalid enrollment credential output"}
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return farmerr.Error{Code: farmerr.PERMISSION_DENIED, HumanMessage: "cannot create enrollment credential file", Details: map[string]string{"reason": err.Error()}}
	}
	_, writeErr := fmt.Fprintln(file, token)
	syncErr := file.Sync()
	closeErr := file.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return farmerr.Error{Code: farmerr.PERMISSION_DENIED, HumanMessage: "cannot persist enrollment credential", Details: map[string]string{"reason": err.Error()}}
	}
	return nil
}

func buildRuntimeCommand(action, executionID, executable string, args []string, workingDirectory, restartPolicy string) (*le0xv1.CommandEnvelope, error) {
	switch action {
	case "":
		if executionID != "" || executable != "" || len(args) != 0 || workingDirectory != "" || restartPolicy != "NEVER" {
			return nil, errors.New("execution options require --dev-runtime-action")
		}
		return nil, nil
	case "start":
		if executionID == "" || executable == "" {
			return nil, errors.New("start requires --execution-id and --executable")
		}
		if restartPolicy != "NEVER" && restartPolicy != "ON_FAILURE" {
			return nil, errors.New("--restart-policy must be NEVER or ON_FAILURE")
		}
		return &le0xv1.CommandEnvelope{Command: &le0xv1.CommandEnvelope_StartExecution{StartExecution: &le0xv1.StartExecution{Plan: &le0xv1.ExecutionPlan{ExecutionId: executionID, Executable: executable, Args: args, WorkingDirectory: workingDirectory, RestartPolicy: restartPolicy}}}}, nil
	case "stop":
		if executionID == "" {
			return nil, errors.New("stop requires --execution-id")
		}
		return &le0xv1.CommandEnvelope{Command: &le0xv1.CommandEnvelope_StopExecution{StopExecution: &le0xv1.StopExecution{ExecutionId: executionID}}}, nil
	case "restart":
		if executionID == "" {
			return nil, errors.New("restart requires --execution-id")
		}
		return &le0xv1.CommandEnvelope{Command: &le0xv1.CommandEnvelope_RestartExecution{RestartExecution: &le0xv1.RestartExecution{ExecutionId: executionID}}}, nil
	case "get":
		return &le0xv1.CommandEnvelope{Command: &le0xv1.CommandEnvelope_GetExecutions{GetExecutions: &le0xv1.GetExecutions{}}}, nil
	default:
		return nil, errors.New("--dev-runtime-action must be start, start-miner, stop, restart, or get")
	}
}

func buildMinerRuntimeCommand(executionID, adapterID, packageID, packageVersion, mode, coin, algorithm string, cpuThreads uint32, restartPolicy string, endpoint *le0xv1.MiningEndpoint) (*le0xv1.CommandEnvelope, error) {
	if executionID == "" || adapterID == "" || packageID == "" || packageVersion == "" || mode == "" {
		return nil, errors.New("start-miner requires execution, adapter, package, version, and mode")
	}
	if restartPolicy != "NEVER" && restartPolicy != "ON_FAILURE" {
		return nil, errors.New("--restart-policy must be NEVER or ON_FAILURE")
	}
	miner := &le0xv1.MinerSpec{AdapterId: adapterID, SpecVersion: 1, PackageId: packageID, PackageVersion: packageVersion, Mode: mode, Coin: coin, Algorithm: algorithm, Endpoint: endpoint}
	if cpuThreads > 0 {
		miner.CpuThreads = &cpuThreads
	}
	return &le0xv1.CommandEnvelope{Command: &le0xv1.CommandEnvelope_StartExecution{StartExecution: &le0xv1.StartExecution{Plan: &le0xv1.ExecutionPlan{ExecutionId: executionID, RestartPolicy: restartPolicy, Miner: miner}}}}, nil
}

func endpointFromFlags(address string, tls bool, user, password, worker string) *le0xv1.MiningEndpoint {
	if address == "" && !tls && user == "" && password == "" && worker == "" {
		return nil
	}
	return &le0xv1.MiningEndpoint{Address: address, Tls: tls, User: user, Password: password, Worker: worker}
}

func attachDevOwnership(command *le0xv1.CommandEnvelope, hostID identity.HostID) error {
	plan := command.GetStartExecution().GetPlan()
	if plan == nil {
		return farmerr.Error{Code: farmerr.CONFIG_CONFLICT, HumanMessage: "development START requires an ExecutionPlan"}
	}
	workloadID, err := identity.NewWorkloadID()
	if err != nil {
		return farmerr.Error{Code: farmerr.INTERNAL_ERROR, HumanMessage: "cannot generate development WorkloadID"}
	}
	sum := sha256.Sum256([]byte("le0xfarm-dev-runtime\x00" + plan.ExecutionId))
	plan.Ownership = &le0xv1.WorkloadOwnership{WorkloadId: workloadID.String(), DesiredGeneration: 1, ResolvedHash: fmt.Sprintf("sha256:%x", sum[:]), HostId: hostID.String(), ResourceClaim: &le0xv1.ResourceClaim{Cpu: true}}
	return nil
}

func readOneLine(in io.Reader) (string, error) {
	scanner := bufio.NewScanner(io.LimitReader(in, 64<<10))
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return "", err
		}
		return "", errors.New("missing input")
	}
	value := scanner.Text()
	for scanner.Scan() {
		if strings.TrimSpace(scanner.Text()) != "" {
			return "", errors.New("unexpected additional input")
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	return value, nil
}

func printError(out io.Writer, err error) {
	fmt.Fprintln(out, err)
	var typed farmerr.Error
	if errors.As(err, &typed) && typed.SuggestedFix != "" {
		fmt.Fprintln(out, typed.SuggestedFix)
	}
}
