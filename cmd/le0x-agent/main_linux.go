// Le0xAgent M1.1 performs a local bootstrap and reports inventory, then exits.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/le0xdon/le0xfarm/internal/agentidentity"
	"github.com/le0xdon/le0xfarm/internal/agentnet"
	"github.com/le0xdon/le0xfarm/internal/farmerr"
	"github.com/le0xdon/le0xfarm/internal/inventory"
	"github.com/le0xdon/le0xfarm/internal/minerruntime"
	"github.com/le0xdon/le0xfarm/internal/miners/xmrig"
	"github.com/le0xdon/le0xfarm/internal/model"
	"github.com/le0xdon/le0xfarm/internal/packages"
	"github.com/le0xdon/le0xfarm/internal/processobserve"
	"github.com/le0xdon/le0xfarm/internal/runtime/supervisor"
)

type report struct {
	agentidentity.Identity
	Inventory model.Inventory `json:"inventory"`
	Warnings  []string        `json:"warnings"`
}

var approvedPackageManifest = xmrig.Manifest

func main() { os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr)) }

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("le0x-agent", flag.ContinueOnError)
	flags.SetOutput(stderr)
	asJSON := flags.Bool("json", false, "Print local identity and inventory as JSON")
	controller := flags.String("controller", "", "Controller host:port (enables persistent network mode)")
	insecureDev := flags.Bool("insecure-dev", false, "Allow plaintext gRPC for development/test only")
	allowRawExecution := flags.Bool("allow-raw-execution", false, "Allow raw executable plans in insecure development mode only")
	pair := flags.String("pair", "", "Controller enrollment token (development only)")
	pairStdin := flags.Bool("pair-stdin", false, "Read one secure enrollment token from stdin")
	tlsFingerprint := flags.String("tls-fingerprint", "", "Controller certificate SHA-256 fingerprint")
	packageAction := flags.String("package-action", "", "Local verified package action: import, verify, or list")
	packageArchive := flags.String("package-archive", "", "Local approved package archive for --package-action import")
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
	if *packageAction != "" && (*controller != "" || *insecureDev || *allowRawExecution || *pair != "" || *pairStdin || *tlsFingerprint != "") {
		return fail(stderr, *asJSON, errors.New("package actions cannot be combined with Controller connection or enrollment options"))
	}
	if (*packageAction == "import") != (*packageArchive != "") {
		return fail(stderr, *asJSON, errors.New("--package-action import requires exactly one --package-archive"))
	}
	if *packageAction != "" && *packageAction != "import" && *packageAction != "verify" && *packageAction != "list" {
		return fail(stderr, *asJSON, errors.New("--package-action must be import, verify, or list"))
	}
	if *pair != "" && *controller == "" {
		return fail(stderr, false, errors.New("--pair requires --controller"))
	}
	if *pair != "" && !*insecureDev {
		return fail(stderr, false, errors.New("--pair is allowed only with --insecure-dev; use --pair-stdin for secure enrollment"))
	}
	if *allowRawExecution && (!*insecureDev || *controller == "") {
		return fail(stderr, false, errors.New("--allow-raw-execution requires --insecure-dev and --controller"))
	}
	if *pairStdin && (*controller == "" || *insecureDev) {
		return fail(stderr, false, errors.New("--pair-stdin requires secure --controller mode"))
	}
	if *pairStdin && *tlsFingerprint == "" {
		return fail(stderr, false, errors.New("--pair-stdin requires --tls-fingerprint"))
	}
	if *tlsFingerprint != "" && !*pairStdin {
		return fail(stderr, false, errors.New("--tls-fingerprint requires --pair-stdin"))
	}
	token := *pair
	if *pairStdin {
		scanner := bufio.NewScanner(stdin)
		scanner.Buffer(make([]byte, 1024), 4096)
		if !scanner.Scan() || scanner.Text() == "" {
			return fail(stderr, false, errors.New("expected one enrollment token on stdin"))
		}
		token = scanner.Text()
		if scanner.Scan() {
			return fail(stderr, false, errors.New("expected exactly one enrollment token line"))
		}
		if err := scanner.Err(); err != nil {
			return fail(stderr, false, err)
		}
	}
	dir, err := agentidentity.DataDir()
	if err != nil {
		return fail(stderr, *asJSON, err)
	}
	id, err := agentidentity.LoadOrCreate(dir)
	if err != nil {
		return fail(stderr, *asJSON, err)
	}
	if *packageAction != "" {
		store := packages.New(dir)
		manifest := approvedPackageManifest()
		var installed packages.Installed
		if *packageAction == "import" {
			installed, err = store.Install(context.Background(), *packageArchive, manifest)
		} else {
			installed, err = store.Lookup(manifest.PackageID, manifest.Version)
		}
		if *packageAction == "list" {
			if code, _ := farmerr.CodeOf(err); code == farmerr.PACKAGE_NOT_INSTALLED {
				err = json.NewEncoder(stdout).Encode([]packages.Installed{})
			} else if err == nil {
				err = json.NewEncoder(stdout).Encode([]packages.Installed{installed})
			}
			if err != nil {
				return fail(stderr, *asJSON, err)
			}
			return 0
		}
		if err != nil {
			return fail(stderr, *asJSON, err)
		}
		if err := json.NewEncoder(stdout).Encode(installed); err != nil {
			return fail(stderr, *asJSON, err)
		}
		return 0
	}
	inventorySource := inventory.Local()
	facts, warnings := inventorySource.Discover(id.HostID)
	if *controller != "" {
		if *asJSON {
			return fail(stderr, true, fmt.Errorf("--json cannot be combined with --controller"))
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		runtimeSupervisor := supervisor.New(supervisor.Config{})
		registry := minerruntime.NewRegistry()
		if err := registry.Register(&xmrig.Adapter{}); err != nil {
			return fail(stderr, false, err)
		}
		packageStore := packages.New(dir)
		processSource := processobserve.Local()
		minerRuntime := minerruntime.New(runtimeSupervisor, registry, dir, packageStore, facts, minerruntime.Config{RefreshInventory: func() model.Inventory {
			refreshed, _ := inventorySource.Discover(id.HostID)
			return refreshed
		}, ProcessObserver: &processSource})
		defer func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
			defer cancel()
			_ = minerRuntime.Shutdown(shutdownCtx)
		}()
		err := agentnet.Run(ctx, agentnet.Config{Target: *controller, InsecureDev: *insecureDev, AllowRawExecution: *allowRawExecution, EnrollmentToken: token, TLSFingerprint: *tlsFingerprint, TrustDir: dir, AgentID: id.AgentID, HostID: id.HostID, Hostname: facts.Host.Hostname, Inventory: inventorySource, Supervisor: runtimeSupervisor, MinerRuntime: minerRuntime, Output: log.New(stdout, "", 0)})
		if err != nil {
			return fail(stderr, false, err)
		}
		return 0
	}
	if *insecureDev {
		return fail(stderr, false, errors.New("--insecure-dev requires --controller"))
	}
	if *pairStdin || *tlsFingerprint != "" {
		return fail(stderr, false, errors.New("pairing options require --controller"))
	}
	if err := present(stdout, report{Identity: id, Inventory: facts, Warnings: warnings}, *asJSON); err != nil {
		return fail(stderr, *asJSON, farmerr.Error{Code: farmerr.INTERNAL_ERROR, HumanMessage: "Cannot write Agent output", Details: map[string]string{"reason": err.Error()}})
	}
	return 0
}

func fail(stderr io.Writer, asJSON bool, err error) int {
	if asJSON {
		_ = json.NewEncoder(stderr).Encode(err)
	} else {
		fmt.Fprintln(stderr, err)
		var typed farmerr.Error
		if errors.As(err, &typed) {
			fmt.Fprintln(stderr, typed.Details["reason"])
			fmt.Fprintln(stderr, typed.SuggestedFix)
		}
	}
	return 1
}

// Presentation consumes facts; it does not read Linux files or persist identity.
func present(out io.Writer, result report, asJSON bool) error {
	if asJSON {
		encoder := json.NewEncoder(out)
		encoder.SetIndent("", "  ")
		return encoder.Encode(result)
	}
	facts := result.Inventory
	osName := facts.Host.OS.PrettyName
	if osName == "" {
		osName = strings.TrimSpace(facts.Host.OS.ID + " " + facts.Host.OS.VersionID)
	}
	if osName == "" {
		osName = "unknown"
	}
	_, err := fmt.Fprintf(out, "Le0xAgent\nAgentID: %s\nHostID: %s\nHostname: %s\nOS: %s\nKernel: %s\nArch: %s\nCPU: %s (%s)\nSockets: %d\nCores: %d\nThreads: %d\nMemory: %d bytes (%.2f GiB)\nGPUs: %d\n",
		result.AgentID, result.HostID, facts.Host.Hostname, osName, facts.Host.OS.Kernel, facts.Host.Architecture,
		facts.CPU.Model, facts.CPU.Vendor, facts.CPU.Sockets, facts.CPU.Cores, facts.CPU.Threads,
		facts.Memory.TotalBytes, float64(facts.Memory.TotalBytes)/(1<<30), len(facts.GPUs))
	if err != nil {
		return err
	}
	for _, gpu := range facts.GPUs {
		if _, err := fmt.Fprintf(out, "GPU: %s %s DeviceID=%s PCI=%s\n", gpu.Vendor, gpu.Model, gpu.DeviceID, gpu.PCIBusID); err != nil {
			return err
		}
	}
	for _, warning := range result.Warnings {
		if _, err := fmt.Fprintln(out, "Warning:", warning); err != nil {
			return err
		}
	}
	return nil
}
