// Le0xAgent M1.1 performs a local bootstrap and reports inventory, then exits.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/le0xdon/le0xfarm/internal/agentidentity"
	"github.com/le0xdon/le0xfarm/internal/farmerr"
	"github.com/le0xdon/le0xfarm/internal/inventory"
	"github.com/le0xdon/le0xfarm/internal/model"
)

type report struct {
	agentidentity.Identity
	Inventory model.Inventory `json:"inventory"`
	Warnings  []string        `json:"warnings"`
}

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("le0x-agent", flag.ContinueOnError)
	flags.SetOutput(stderr)
	asJSON := flags.Bool("json", false, "Print local identity and inventory as JSON")
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
	dir, err := agentidentity.DataDir()
	if err != nil {
		return fail(stderr, *asJSON, err)
	}
	id, err := agentidentity.LoadOrCreate(dir)
	if err != nil {
		return fail(stderr, *asJSON, err)
	}
	facts, warnings := inventory.Local().Discover(id.HostID)
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
	_, err := fmt.Fprintf(out, "Le0xAgent\nAgentID: %s\nHostID: %s\nHostname: %s\nOS: %s\nKernel: %s\nArch: %s\nCPU: %s (%s)\nSockets: %d\nCores: %d\nThreads: %d\nMemory: %d bytes (%.2f GiB)\nGPUs: discovery not implemented\n",
		result.AgentID, result.HostID, facts.Host.Hostname, osName, facts.Host.OS.Kernel, facts.Host.Architecture,
		facts.CPU.Model, facts.CPU.Vendor, facts.CPU.Sockets, facts.CPU.Cores, facts.CPU.Threads,
		facts.Memory.TotalBytes, float64(facts.Memory.TotalBytes)/(1<<30))
	if err != nil {
		return err
	}
	for _, warning := range result.Warnings {
		if _, err := fmt.Fprintln(out, "Warning:", warning); err != nil {
			return err
		}
	}
	return nil
}
