// Le0xController M1.2 development-only gRPC runtime skeleton.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/le0xdon/le0xfarm/internal/controlleridentity"
	"github.com/le0xdon/le0xfarm/internal/controllernet"
	"github.com/le0xdon/le0xfarm/internal/controllertrust"
	"github.com/le0xdon/le0xfarm/internal/farmerr"
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("le0x-controller", flag.ContinueOnError)
	flags.SetOutput(stderr)
	listen := flags.String("listen", "127.0.0.1:50051", "TCP listen address")
	insecureDev := flags.Bool("insecure-dev", false, "Allow plaintext gRPC for development/test only")
	initIdentity := flags.Bool("init", false, "Initialize a new Controller/Farm identity")
	pairing := flags.Bool("pairing", false, "Enable temporary development enrollment pairing")
	pairingTTL := flags.Duration("pairing-ttl", 15*time.Minute, "Enrollment token lifetime")
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
	if !*insecureDev {
		fmt.Fprintln(stderr, "Secure transport will be implemented in a future stage; refusing plaintext. Pass --insecure-dev for development/test only.")
		return 1
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
	server, err := controllernet.New(controllernet.Config{ListenAddress: *listen, InsecureDev: true, ControllerID: controllerIdentity.ControllerID, FarmID: controllerIdentity.FarmID, Trust: trust, Pairing: pairingWindow, Output: log.New(stdout, "", 0)})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	controllerID, farmID := server.IDs()
	fmt.Fprintf(stdout, "Le0xController\nControllerID: %s\nFarmID: %s\nListening: %s\nSecurity: INSECURE DEVELOPMENT MODE\n", controllerID, farmID, listener.Addr())
	if *pairing {
		fmt.Fprintf(stdout, "Pairing: ENABLED — DEVELOPMENT MODE\nEnrollment token: %s\nExpires: %s\n", token, expiry.Format(time.RFC3339))
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := server.Serve(ctx, listener); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

func printError(out io.Writer, err error) {
	fmt.Fprintln(out, err)
	var typed farmerr.Error
	if errors.As(err, &typed) && typed.SuggestedFix != "" {
		fmt.Fprintln(out, typed.SuggestedFix)
	}
}
