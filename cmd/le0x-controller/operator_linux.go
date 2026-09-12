//go:build linux

package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"math"
	"strings"
	"time"

	"github.com/le0xdon/le0xfarm/internal/controlleradmin"
)

type operatorFlags struct {
	action, id, name, address, coin, adapter, packageID, packageVersion    *string
	mode, algorithm, poolID, walletID, profileID, hostID, workloadID       *string
	runState, worker, userTemplate, workerPlacement, reason, incidentState *string
	tls, cpu                                                               *bool
	cpuThreads                                                             *uint
	deviceIDs                                                              stringList
	limit                                                                  *int
}

func registerOperatorFlags(flags *flag.FlagSet) *operatorFlags {
	o := &operatorFlags{}
	o.action = flags.String("operator-action", "", "Live operator action; use --operator-action help for actions")
	o.id = flags.String("op-id", "", "Object ID for show/delete actions")
	o.name = flags.String("op-name", "", "Object display name")
	o.address = flags.String("op-address", "", "Pool host:port or public WalletRef address")
	o.coin = flags.String("op-coin", "", "Coin identifier")
	o.adapter = flags.String("op-adapter", "", "Miner adapter ID")
	o.packageID = flags.String("op-package-id", "", "PackageID")
	o.packageVersion = flags.String("op-package-version", "", "Package version")
	o.mode = flags.String("op-mode", "MINING", "Profile mode")
	o.algorithm = flags.String("op-algorithm", "", "Miner algorithm")
	o.poolID = flags.String("op-pool-id", "", "PoolID")
	o.walletID = flags.String("op-wallet-id", "", "WalletID")
	o.profileID = flags.String("op-profile-id", "", "ProfileID")
	o.hostID = flags.String("op-host-id", "", "HostID")
	o.workloadID = flags.String("op-workload-id", "", "WorkloadID")
	o.runState = flags.String("op-state", "", "RUNNING or STOPPED")
	o.worker = flags.String("op-worker", "", "Optional worker name")
	o.userTemplate = flags.String("op-user-template", "${wallet}", "Public pool-login template")
	o.workerPlacement = flags.String("op-worker-placement", "NONE", "NONE, IN_USER, or SEPARATE")
	o.reason = flags.String("op-reason", "", "Bounded Maintenance Hold reason")
	o.incidentState = flags.String("op-incident-state", "", "ACTIVE, RESOLVED, or empty")
	o.tls = flags.Bool("op-tls", false, "Require TLS for Pool")
	o.cpu = flags.Bool("op-cpu", false, "Claim the Host CPU")
	o.cpuThreads = flags.Uint("op-cpu-threads", 0, "Optional CPU thread count")
	flags.Var(&o.deviceIDs, "op-device-id", "Stable GPU DeviceID; repeatable")
	o.limit = flags.Int("op-limit", 100, "Maximum incident rows")
	return o
}

func (o *operatorFlags) request() controlleradmin.Request {
	return controlleradmin.Request{Action: *o.action, ID: *o.id, Name: *o.name, Address: *o.address, Coin: *o.coin, AdapterID: *o.adapter, PackageID: *o.packageID, PackageVersion: *o.packageVersion, Mode: *o.mode, Algorithm: *o.algorithm, PoolID: *o.poolID, WalletID: *o.walletID, ProfileID: *o.profileID, HostID: *o.hostID, WorkloadID: *o.workloadID, RunState: *o.runState, Worker: *o.worker, UserTemplate: *o.userTemplate, WorkerPlacement: *o.workerPlacement, Reason: *o.reason, IncidentState: *o.incidentState, TLS: *o.tls, CPU: *o.cpu, CPUThreads: uint32(*o.cpuThreads), DeviceIDs: append([]string(nil), o.deviceIDs...), Limit: *o.limit}
}

func runOperator(dataDir string, options *operatorFlags, stdout, stderr io.Writer) int {
	if *options.action == "help" {
		fmt.Fprintln(stdout, "actions: hosts-list host-show pool-create pool-list pool-show wallet-create wallet-list wallet-show profile-create profile-list profile-show workload-create workload-list workload-show workload-set workload-delete hold-set hold-clear hold-list hold-show incidents-active incidents-list status")
		return 0
	}
	if *options.cpuThreads > math.MaxUint32 {
		fmt.Fprintln(stderr, "invalid CPU thread count")
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	data, err := controlleradmin.Call(ctx, "unix", controlleradmin.SocketPath(dataDir), options.request())
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	_, _ = io.Copy(stdout, strings.NewReader(string(data)+"\n"))
	return 0
}
