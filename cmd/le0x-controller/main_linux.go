// Le0xController M1.2 development-only gRPC runtime skeleton.
package main

import (
	"bufio"
	"context"
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

	"github.com/le0xdon/le0xfarm/internal/controlleridentity"
	"github.com/le0xdon/le0xfarm/internal/controllernet"
	"github.com/le0xdon/le0xfarm/internal/controllerpki"
	"github.com/le0xdon/le0xfarm/internal/controllertrust"
	"github.com/le0xdon/le0xfarm/internal/farmerr"
	"github.com/le0xdon/le0xfarm/internal/identity"
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
	if *initIdentity && *initPKI {
		fmt.Fprintln(stderr, "--init and --init-pki cannot be combined")
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
	var pki *controllerpki.PKI
	if *initIdentity || *initPKI {
		pki, err = controllerpki.Initialize(dataDir, controllerIdentity.ControllerID, controllerIdentity.FarmID)
	} else if !*insecureDev {
		pki, err = controllerpki.Load(dataDir, controllerIdentity.ControllerID, controllerIdentity.FarmID)
	}
	if err != nil {
		printError(stderr, err)
		return 1
	}
	trust, err := controllertrust.Open(dataDir, controllerIdentity.ControllerID, controllerIdentity.FarmID)
	if err != nil {
		printError(stderr, err)
		return 1
	}
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
	server, err := controllernet.New(controllernet.Config{ListenAddress: *listen, InsecureDev: *insecureDev, ControllerID: controllerIdentity.ControllerID, FarmID: controllerIdentity.FarmID, Trust: trust, Pairing: pairingWindow, PKI: pki, RuntimeCommands: runtimeCommands, RuntimeTarget: targetAgent, Output: log.New(stdout, "", 0)})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	controllerID, farmID := server.IDs()
	security := "TLS 1.3 MUTUAL TLS"
	if *insecureDev {
		security = "INSECURE DEVELOPMENT MODE"
	}
	fmt.Fprintf(stdout, "Le0xController\nControllerID: %s\nFarmID: %s\nListening: %s\nSecurity: %s\n", controllerID, farmID, listener.Addr(), security)
	if *pairing {
		if *insecureDev {
			fmt.Fprintf(stdout, "Pairing: ENABLED — DEVELOPMENT MODE\nEnrollment token: %s\nExpires: %s\n", token, expiry.Format(time.RFC3339))
		} else {
			fmt.Fprintf(stdout, "Pairing: ENABLED\nEnrollment token: %s\nTLS fingerprint: %s\nExpires: %s\n", token, pki.ServerFingerprint(), expiry.Format(time.RFC3339))
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := server.Serve(ctx, listener); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
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
