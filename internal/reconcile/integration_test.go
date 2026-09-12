package reconcile

import (
	"context"
	"io"
	"io/fs"
	"log"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/le0xdon/le0xfarm/internal/agentnet"
	"github.com/le0xdon/le0xfarm/internal/agenttrust"
	"github.com/le0xdon/le0xfarm/internal/controllerbackup"
	"github.com/le0xdon/le0xfarm/internal/controllerdb"
	"github.com/le0xdon/le0xfarm/internal/controllerlock"
	"github.com/le0xdon/le0xfarm/internal/controllernet"
	"github.com/le0xdon/le0xfarm/internal/controllerstate"
	"github.com/le0xdon/le0xfarm/internal/controllertrust"
	"github.com/le0xdon/le0xfarm/internal/farmconfig"
	"github.com/le0xdon/le0xfarm/internal/farmmodel"
	"github.com/le0xdon/le0xfarm/internal/identity"
	"github.com/le0xdon/le0xfarm/internal/inventory"
	"github.com/le0xdon/le0xfarm/internal/minerruntime"
	"github.com/le0xdon/le0xfarm/internal/model"
	"github.com/le0xdon/le0xfarm/internal/packagecatalog"
	"github.com/le0xdon/le0xfarm/internal/runtime/supervisor"
	"google.golang.org/grpc/test/bufconn"
)

const integrationAdapterID = "integration-sleep"

var integrationPackageID = mustIntegrationPackageID("package_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")

func TestControllerBackupRestoreRequiresFreshActualBeforeReconcile(t *testing.T) {
	ctx := context.Background()
	controllerID, _ := identity.ParseControllerID("controller_0123456789abcdef0123456789abcdef")
	farmID, _ := identity.ParseFarmID("farm_0123456789abcdef0123456789abcdef")
	agentID := testAgent(1)
	hostID := testHost(1)
	trust, err := controllertrust.Open(t.TempDir(), controllerID, farmID)
	if err != nil {
		t.Fatal(err)
	}
	if err := trust.Pair(agentID, hostID); err != nil {
		t.Fatal(err)
	}
	agentDir := t.TempDir()
	if err := agenttrust.Save(agentDir, agenttrust.Binding{ControllerID: controllerID, FarmID: farmID}); err != nil {
		t.Fatal(err)
	}
	catalog, err := packagecatalog.NewStatic([]farmmodel.PackageRelease{{Ref: farmmodel.PackageRef{PackageID: integrationPackageID, Version: "1"}, AdapterIDs: []string{integrationAdapterID}, Runtime: farmmodel.RuntimeCapabilities{CPU: true}}})
	if err != nil {
		t.Fatal(err)
	}
	dataDir := filepath.Join(t.TempDir(), "controller")
	db, err := controllerdb.Open(ctx, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	backupLease, err := controllerlock.Acquire(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	service, err := farmconfig.New(db, catalog, farmconfig.Options{})
	if err != nil {
		t.Fatal(err)
	}
	pool, err := service.CreatePool(ctx, farmmodel.PoolContent{Name: "pool", Address: "pool.example:1", Auth: farmmodel.PoolAuth{Kind: farmmodel.PoolAuthNone}})
	if err != nil {
		t.Fatal(err)
	}
	wallet, err := service.CreateWalletRef(ctx, farmmodel.WalletRefContent{Name: "wallet", Coin: "TEST", Address: "public-payout"})
	if err != nil {
		t.Fatal(err)
	}
	profile, err := service.CreateMiningProfile(ctx, farmmodel.MiningProfileContent{Name: "profile", AdapterID: integrationAdapterID, Package: farmmodel.PackageRef{PackageID: integrationPackageID, Version: "1"}, Mode: farmmodel.ProfileModeMining, Coin: "TEST", PoolID: pool.PoolID, WalletID: wallet.WalletID, LoginPolicy: farmmodel.LoginPolicy{UserTemplate: "${wallet}", WorkerPlacement: farmmodel.WorkerNone}})
	if err != nil {
		t.Fatal(err)
	}
	workload, err := service.CreateDesiredWorkload(ctx, farmmodel.DesiredWorkloadContent{Name: "work", HostID: hostID, ProfileID: profile.ProfileID, RunState: farmmodel.DesiredRunning, Resources: farmmodel.ResourceClaim{CPU: true}})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := service.GetCurrentResolvedSnapshot(ctx, workload.WorkloadID)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := controllerbackup.New(controllerbackup.Config{DataDir: dataDir, ControllerID: controllerID, FarmID: farmID, TrustFingerprint: "SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", Lease: backupLease})
	if err != nil {
		t.Fatal(err)
	}
	backup, err := manager.CreateManual(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	stopped := workload.DesiredWorkloadContent
	stopped.RunState = farmmodel.DesiredStopped
	if _, err := service.UpdateDesiredWorkload(ctx, workload.WorkloadID, workload.Meta.Revision, stopped); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := backupLease.Close(); err != nil {
		t.Fatal(err)
	}
	lease, err := controllerlock.Acquire(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	restoreManager, err := controllerbackup.New(controllerbackup.Config{DataDir: dataDir, ControllerID: controllerID, FarmID: farmID, TrustFingerprint: "SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", Lease: lease})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restoreManager.Restore(ctx, backup.Manifest.BackupID, lease); err != nil {
		t.Fatal(err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}

	// Reconstruct the Controller after the offline restore and attach a real
	// Agent through bufconn. Inventory blocks after Hello/execution publication,
	// exposing the exact reconnect-before-complete-bootstrap safety window.
	runtimeLease, err := controllerlock.Acquire(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	db, err = controllerdb.Open(ctx, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	service, err = farmconfig.New(db, catalog, farmconfig.Options{})
	if err != nil {
		t.Fatal(err)
	}
	restored, err := service.GetDesiredWorkload(ctx, workload.WorkloadID)
	if err != nil || restored.RunState != farmmodel.DesiredRunning {
		t.Fatalf("restored Desired=%+v err=%v", restored, err)
	}
	listener := bufconn.Listen(1024 * 1024)
	server, err := controllernet.New(controllernet.Config{InsecureDev: true, ControllerID: controllerID, FarmID: farmID, Trust: trust, Maintenance: service, ShutdownGracePeriod: 20 * time.Millisecond, Output: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	commands := &countingCommander{server: server}
	observed := controllerstate.New()
	coordinator := NewCoordinator(service, observed, commands, nil)
	server.SetSessionHandler(coordinator)
	controllerCtx, stopController := context.WithCancel(context.Background())
	controllerDone := make(chan error, 1)
	go func() { controllerDone <- server.Serve(controllerCtx, listener) }()

	processes := supervisor.New(supervisor.Config{StopGrace: 100 * time.Millisecond})
	registry := minerruntime.NewRegistry()
	if err := registry.Register(sleepRuntimeAdapter{}); err != nil {
		t.Fatal(err)
	}
	localInventory := inventory.Local()
	blockInventory := &oneShotInventoryBarrier{base: localInventory.Files, entered: make(chan struct{}), release: make(chan struct{})}
	localInventory.Files = blockInventory
	facts, _ := inventory.Local().Discover(hostID)
	minerRuntime := minerruntime.New(processes, registry, agentDir, nil, facts, minerruntime.Config{PollInterval: 10 * time.Millisecond})
	defer minerRuntime.Shutdown(context.Background())
	agentCtx, stopAgent := context.WithCancel(context.Background())
	agentDone := make(chan error, 1)
	go func() {
		agentDone <- agentnet.Run(agentCtx, agentnet.Config{Target: "buf", InsecureDev: true, TrustDir: agentDir, AgentID: agentID, HostID: hostID, Inventory: localInventory, Supervisor: processes, MinerRuntime: minerRuntime, HeartbeatInterval: 20 * time.Millisecond, ReconnectInitial: time.Millisecond, ReconnectMax: 5 * time.Millisecond, Dialer: func(context.Context, string) (net.Conn, error) { return listener.Dial() }, Output: log.New(io.Discard, "", 0)})
	}()
	select {
	case <-blockInventory.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("Agent did not reach the incomplete bootstrap barrier")
	}
	if session, ok := server.Session(hostID); !ok || session.Ready {
		t.Fatalf("reconnect was not observable before bootstrap: %+v exists=%t", session, ok)
	}
	if commands.Count(controllernet.RuntimeStart) != 0 {
		t.Fatal("restored Desired dispatched before fresh complete bootstrap")
	}
	if _, exists, err := service.GetRestoreHostBarrier(ctx, hostID); err != nil || !exists {
		t.Fatalf("incomplete bootstrap cleared restore barrier exists=%t err=%v", exists, err)
	}
	close(blockInventory.release)
	waitSessionReady(t, server, hostID)
	waitExecutionState(t, processes, snapshot.ExecutionID, model.ExecutionRunning)
	if commands.Count(controllernet.RuntimeStart) != 1 {
		t.Fatalf("fresh compatible Actual START count=%d", commands.Count(controllernet.RuntimeStart))
	}
	if _, exists, err := service.GetRestoreHostBarrier(ctx, hostID); err != nil || exists {
		t.Fatalf("fresh comparison did not clear restore barrier exists=%t err=%v", exists, err)
	}
	running := mustRuntimeSnapshot(t, processes, snapshot.ExecutionID)
	if running.PID <= 0 || running.ProcessInstance == "" {
		t.Fatalf("full restore path did not create a real managed process: %+v", running)
	}

	stopAgent()
	select {
	case <-agentDone:
	case <-time.After(time.Second):
		t.Fatal("restored Agent did not stop")
	}
	stopController()
	if err := <-controllerDone; err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := runtimeLease.Close(); err != nil {
		t.Fatal(err)
	}

	type restoredControllerPhase struct {
		lease       *controllerlock.Lease
		db          *controllerdb.DB
		service     *farmconfig.Service
		server      *controllernet.Server
		commands    *countingCommander
		coordinator *Coordinator
		listener    *bufconn.Listener
		stop        context.CancelFunc
		done        <-chan error
	}
	restoreAgain := func() {
		lease, err := controllerlock.Acquire(dataDir)
		if err != nil {
			t.Fatal(err)
		}
		restoreManager, err := controllerbackup.New(controllerbackup.Config{DataDir: dataDir, ControllerID: controllerID, FarmID: farmID, TrustFingerprint: "SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", Lease: lease})
		if err != nil {
			_ = lease.Close()
			t.Fatal(err)
		}
		if _, err := restoreManager.Restore(ctx, backup.Manifest.BackupID, lease); err != nil {
			_ = lease.Close()
			t.Fatal(err)
		}
		if err := lease.Close(); err != nil {
			t.Fatal(err)
		}
	}
	startRestoredController := func() restoredControllerPhase {
		lease, err := controllerlock.Acquire(dataDir)
		if err != nil {
			t.Fatal(err)
		}
		db, err := controllerdb.Open(ctx, dataDir)
		if err != nil {
			_ = lease.Close()
			t.Fatal(err)
		}
		service, err := farmconfig.New(db, catalog, farmconfig.Options{})
		if err != nil {
			_ = db.Close()
			_ = lease.Close()
			t.Fatal(err)
		}
		listener := bufconn.Listen(1024 * 1024)
		server, err := controllernet.New(controllernet.Config{InsecureDev: true, ControllerID: controllerID, FarmID: farmID, Trust: trust, Maintenance: service, ShutdownGracePeriod: 20 * time.Millisecond, Output: log.New(io.Discard, "", 0)})
		if err != nil {
			_ = db.Close()
			_ = lease.Close()
			t.Fatal(err)
		}
		commands := &countingCommander{server: server}
		coordinator := NewCoordinator(service, controllerstate.New(), commands, nil)
		server.SetSessionHandler(coordinator)
		phaseCtx, stop := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- server.Serve(phaseCtx, listener) }()
		return restoredControllerPhase{lease: lease, db: db, service: service, server: server, commands: commands, coordinator: coordinator, listener: listener, stop: stop, done: done}
	}
	startAgent := func(listener *bufconn.Listener) (context.CancelFunc, <-chan error) {
		agentCtx, stop := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			done <- agentnet.Run(agentCtx, agentnet.Config{Target: "buf", InsecureDev: true, TrustDir: agentDir, AgentID: agentID, HostID: hostID, Inventory: inventory.Local(), Supervisor: processes, MinerRuntime: minerRuntime, HeartbeatInterval: 20 * time.Millisecond, ReconnectInitial: time.Millisecond, ReconnectMax: 5 * time.Millisecond, Dialer: func(context.Context, string) (net.Conn, error) { return listener.Dial() }, Output: log.New(io.Discard, "", 0)})
		}()
		return stop, done
	}
	stopPhase := func(phase restoredControllerPhase, stopAgent context.CancelFunc, agentDone <-chan error) {
		stopAgent()
		select {
		case <-agentDone:
		case <-time.After(time.Second):
			t.Fatal("Agent did not stop")
		}
		phase.stop()
		if err := <-phase.done; err != nil {
			t.Fatal(err)
		}
		if err := phase.db.Close(); err != nil {
			t.Fatal(err)
		}
		if err := phase.lease.Close(); err != nil {
			t.Fatal(err)
		}
	}
	waitBarrier := func(service *farmconfig.Service, want farmmodel.RestoreBarrierStatus, wantExists bool) {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			barrier, exists, err := service.GetRestoreHostBarrier(ctx, hostID)
			if err == nil && exists == wantExists && (!exists || barrier.Status == want) {
				return
			}
			time.Sleep(time.Millisecond)
		}
		t.Fatalf("restore barrier did not reach exists=%t status=%s", wantExists, want)
	}

	// Restore the same backup while the exact managed execution survives.
	// A real reconnect reports the full ownership tuple and must not duplicate it.
	restoreAgain()
	exactPhase := startRestoredController()
	stopExactAgent, exactAgentDone := startAgent(exactPhase.listener)
	waitSessionReady(t, exactPhase.server, hostID)
	waitBarrier(exactPhase.service, "", false)
	for range 10 {
		exactPhase.coordinator.ReconcileHost(ctx, hostID)
	}
	if exactPhase.commands.Count(controllernet.RuntimeStart) != 0 || exactPhase.commands.Count(controllernet.RuntimeStop) != 0 {
		t.Fatalf("exact restored execution was mutated starts=%d stops=%d", exactPhase.commands.Count(controllernet.RuntimeStart), exactPhase.commands.Count(controllernet.RuntimeStop))
	}
	exact := mustRuntimeSnapshot(t, processes, snapshot.ExecutionID)
	if exact.PID != running.PID || exact.ProcessInstance != running.ProcessInstance || exact.State != model.ExecutionRunning {
		t.Fatalf("exact execution continuity was lost: before=%+v after=%+v", running, exact)
	}
	stopPhase(exactPhase, stopExactAgent, exactAgentDone)

	// Replace the physical execution outside the restored database, then restore
	// T0 again. The current Agent truth is newer and must remain untouched while
	// the durable barrier transitions to CONFLICT.
	if _, _, err := minerRuntime.Stop(snapshot.ExecutionID); err != nil {
		t.Fatal(err)
	}
	newerID, err := identity.NewExecutionID()
	if err != nil {
		t.Fatal(err)
	}
	newerPlan := snapshot.Plan
	newerPlan.ExecutionID = newerID
	newerPlan.Ownership.DesiredGeneration = snapshot.DesiredGeneration + 1
	newerPlan.Ownership.ResolvedHash = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	newer, _, err := minerRuntime.Start(ctx, newerPlan)
	if err != nil || newer.Process.PID <= 0 {
		t.Fatalf("prepare newer physical reality=%+v err=%v", newer, err)
	}
	restoreAgain()
	newerPhase := startRestoredController()
	stopNewerAgent, newerAgentDone := startAgent(newerPhase.listener)
	waitSessionReady(t, newerPhase.server, hostID)
	waitBarrier(newerPhase.service, farmmodel.RestoreBarrierConflict, true)
	for range 10 {
		newerPhase.coordinator.ReconcileHost(ctx, hostID)
	}
	if newerPhase.commands.Count(controllernet.RuntimeStart) != 0 || newerPhase.commands.Count(controllernet.RuntimeStop) != 0 {
		t.Fatalf("newer reality was mutated starts=%d stops=%d", newerPhase.commands.Count(controllernet.RuntimeStart), newerPhase.commands.Count(controllernet.RuntimeStop))
	}
	newerCurrent := mustRuntimeSnapshot(t, processes, newerID)
	if newerCurrent.PID != newer.Process.PID || newerCurrent.State != model.ExecutionRunning {
		t.Fatalf("newer execution did not survive restore comparison: before=%+v after=%+v", newer.Process, newerCurrent)
	}
	stopPhase(newerPhase, stopNewerAgent, newerAgentDone)
}

func TestDesiredRuntimeSurvivesControllerRestartEndToEnd(t *testing.T) {
	controllerID, _ := identity.NewControllerID()
	farmID, _ := identity.NewFarmID()
	agentID := testAgent(1)
	hostID := testHost(1)
	trust, err := controllertrust.Open(t.TempDir(), controllerID, farmID)
	if err != nil {
		t.Fatal(err)
	}
	if err := trust.Pair(agentID, hostID); err != nil {
		t.Fatal(err)
	}
	agentDir := t.TempDir()
	if err := agenttrust.Save(agentDir, agenttrust.Binding{ControllerID: controllerID, FarmID: farmID}); err != nil {
		t.Fatal(err)
	}
	catalog, err := packagecatalog.NewStatic([]farmmodel.PackageRelease{{Ref: farmmodel.PackageRef{PackageID: integrationPackageID, Version: "1"}, AdapterIDs: []string{integrationAdapterID}, Runtime: farmmodel.RuntimeCapabilities{CPU: true}, Tuning: farmmodel.TuningCapabilities{CPUThreads: true, HugePages: true, MSR: true}}})
	if err != nil {
		t.Fatal(err)
	}
	databaseDir := filepath.Join(t.TempDir(), "controller")
	setupDB, err := controllerdb.Open(context.Background(), databaseDir)
	if err != nil {
		t.Fatal(err)
	}
	setupService, err := farmconfig.New(setupDB, catalog, farmconfig.Options{})
	if err != nil {
		t.Fatal(err)
	}
	pool, err := setupService.CreatePool(context.Background(), farmmodel.PoolContent{Name: "synthetic", Address: "127.0.0.1:1", Auth: farmmodel.PoolAuth{Kind: farmmodel.PoolAuthNone}})
	if err != nil {
		t.Fatal(err)
	}
	wallet, err := setupService.CreateWalletRef(context.Background(), farmmodel.WalletRefContent{Name: "synthetic", Coin: "TEST", Address: "synthetic-public-payout"})
	if err != nil {
		t.Fatal(err)
	}
	profile, err := setupService.CreateMiningProfile(context.Background(), farmmodel.MiningProfileContent{Name: "safe", AdapterID: integrationAdapterID, Package: farmmodel.PackageRef{PackageID: integrationPackageID, Version: "1"}, Mode: farmmodel.ProfileModeMining, Coin: "TEST", PoolID: pool.PoolID, WalletID: wallet.WalletID, LoginPolicy: farmmodel.LoginPolicy{UserTemplate: "${wallet}", WorkerPlacement: farmmodel.WorkerNone}})
	if err != nil {
		t.Fatal(err)
	}
	workload, err := setupService.CreateDesiredWorkload(context.Background(), farmmodel.DesiredWorkloadContent{Name: "safe", HostID: hostID, ProfileID: profile.ProfileID, RunState: farmmodel.DesiredRunning, Resources: farmmodel.ResourceClaim{CPU: true}})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := setupService.GetCurrentResolvedSnapshot(context.Background(), workload.WorkloadID)
	if err != nil {
		t.Fatal(err)
	}
	if err := setupDB.Close(); err != nil {
		t.Fatal(err)
	}

	dialer := &switchingBufDialer{}
	runtime := supervisor.New(supervisor.Config{StopGrace: 100 * time.Millisecond})
	registry := minerruntime.NewRegistry()
	telemetrySource := &reconnectBarrierTelemetrySource{}
	if err := registry.Register(sleepRuntimeAdapter{telemetry: telemetrySource}); err != nil {
		t.Fatal(err)
	}
	facts, _ := inventory.Local().Discover(hostID)
	minerRuntime := minerruntime.New(runtime, registry, agentDir, nil, facts, minerruntime.Config{PollInterval: 10 * time.Millisecond})
	defer minerRuntime.Shutdown(context.Background())
	agentCtx, stopAgent := context.WithCancel(context.Background())
	defer stopAgent()
	agentDone := make(chan error, 1)
	go func() {
		agentDone <- agentnet.Run(agentCtx, agentnet.Config{Target: "buf", InsecureDev: true, TrustDir: agentDir, AgentID: agentID, HostID: hostID, Inventory: inventory.Local(), Supervisor: runtime, MinerRuntime: minerRuntime, HeartbeatInterval: 20 * time.Millisecond, ReconnectInitial: time.Millisecond, ReconnectMax: 5 * time.Millisecond, Dialer: func(context.Context, string) (net.Conn, error) { return dialer.Dial() }})
	}()

	startController := func() (*controllernet.Server, *countingCommander, *Coordinator, *farmconfig.Service, *controllerdb.DB, context.CancelFunc, <-chan error) {
		db, err := controllerdb.Open(context.Background(), databaseDir)
		if err != nil {
			t.Fatal(err)
		}
		store, err := farmconfig.New(db, catalog, farmconfig.Options{})
		if err != nil {
			db.Close()
			t.Fatal(err)
		}
		listener := bufconn.Listen(1024 * 1024)
		dialer.Set(listener)
		server, err := controllernet.New(controllernet.Config{InsecureDev: true, ControllerID: controllerID, FarmID: farmID, Trust: trust, Maintenance: store, ShutdownGracePeriod: 20 * time.Millisecond})
		if err != nil {
			db.Close()
			t.Fatal(err)
		}
		commands := &countingCommander{server: server}
		observed := controllerstate.New()
		coordinator := NewCoordinator(store, observed, commands, nil)
		server.SetSessionHandler(coordinator)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go coordinator.Run(ctx, 10*time.Millisecond)
		go func() { done <- server.Serve(ctx, listener) }()
		return server, commands, coordinator, store, db, cancel, done
	}

	server1, commands1, coordinator1, _, db1, stop1, done1 := startController()
	waitSessionReady(t, server1, hostID)
	waitExecutionState(t, runtime, snapshot.ExecutionID, model.ExecutionRunning)
	waitUsefulWorkConfirmed(t, coordinator1.observed, hostID, snapshot.ExecutionID)
	first := mustRuntimeSnapshot(t, runtime, snapshot.ExecutionID)
	if commands1.Count(controllernet.RuntimeStart) != 1 {
		t.Fatalf("initial START count=%d", commands1.Count(controllernet.RuntimeStart))
	}
	pollEntered, releasePoll := telemetrySource.blockNext()
	defer releasePoll()
	<-pollEntered
	stop1()
	if err := <-done1; err != nil {
		t.Fatal(err)
	}
	if err := db1.Close(); err != nil {
		t.Fatal(err)
	}
	if got := mustRuntimeSnapshot(t, runtime, snapshot.ExecutionID); got.State != model.ExecutionRunning || got.PID != first.PID {
		t.Fatalf("Controller shutdown changed Agent execution: %+v", got)
	}

	server2, commands2, coordinator2, store2, db2, stop2, done2 := startController()
	waitSessionReady(t, server2, hostID)
	currentObservation, ok := coordinator2.observed.Get(hostID)
	if !ok {
		t.Fatal("restarted Controller has no current-epoch observation")
	}
	foundRunningWithoutOldEvidence := false
	for _, execution := range currentObservation.Executions {
		if execution.ExecutionID != snapshot.ExecutionID {
			continue
		}
		foundRunningWithoutOldEvidence = execution.Status == model.ExecutionRunning && execution.PID == first.PID && (execution.UsefulWork == nil || execution.UsefulWork.Availability != model.TelemetryAvailable || execution.UsefulWork.UsefulWork != model.UsefulWorkConfirmed)
	}
	if !foundRunningWithoutOldEvidence || currentObservation.AgentState == model.AgentStateMining {
		t.Fatalf("old-epoch telemetry was authoritative before a fresh poll: %+v", currentObservation)
	}
	releasePoll()
	waitUsefulWorkConfirmed(t, coordinator2.observed, hostID, snapshot.ExecutionID)
	time.Sleep(30 * time.Millisecond)
	if commands2.Count(controllernet.RuntimeStart) != 0 || mustRuntimeSnapshot(t, runtime, snapshot.ExecutionID).PID != first.PID {
		t.Fatal("restarted Controller duplicated the persisted execution")
	}
	threads := uint32(1)
	settings, err := store2.CreateHostProfileSettings(context.Background(), hostID, profile.ProfileID, farmmodel.HostProfileSettingsContent{CPUThreads: &threads})
	if err != nil {
		t.Fatal(err)
	}
	overridden, err := store2.GetCurrentResolvedSnapshot(context.Background(), workload.WorkloadID)
	if err != nil {
		t.Fatal(err)
	}
	waitExecutionState(t, runtime, snapshot.ExecutionID, model.ExecutionStopped)
	waitExecutionState(t, runtime, overridden.ExecutionID, model.ExecutionRunning)
	if overridden.ExecutionID == snapshot.ExecutionID || overridden.Plan.CPUThreads == nil || *overridden.Plan.CPUThreads != 1 || commands2.Count(controllernet.RuntimeStop) != 1 || commands2.Count(controllernet.RuntimeStart) != 1 {
		t.Fatalf("Host override did not safely replace execution: snapshot=%+v starts=%d stops=%d", overridden, commands2.Count(controllernet.RuntimeStart), commands2.Count(controllernet.RuntimeStop))
	}

	impossible := facts.CPU.Threads + 1
	settings, err = store2.UpdateHostProfileSettings(context.Background(), hostID, profile.ProfileID, settings.Meta.Revision, farmmodel.HostProfileSettingsContent{CPUThreads: &impossible})
	if err != nil {
		t.Fatal(err)
	}
	blockedSnapshot, err := store2.GetCurrentResolvedSnapshot(context.Background(), workload.WorkloadID)
	if err != nil {
		t.Fatal(err)
	}
	waitExecutionState(t, runtime, overridden.ExecutionID, model.ExecutionStopped)
	time.Sleep(50 * time.Millisecond)
	if running, ok := runtime.Get(blockedSnapshot.ExecutionID); ok && (running.State == model.ExecutionRunning || running.State == model.ExecutionStarting) {
		t.Fatal("impossible CPUThreads started a miner")
	}
	if commands2.Count(controllernet.RuntimeStop) != 2 || commands2.Count(controllernet.RuntimeStart) != 1 {
		t.Fatalf("impossible settings commands starts=%d stops=%d", commands2.Count(controllernet.RuntimeStart), commands2.Count(controllernet.RuntimeStop))
	}

	settings, err = store2.UpdateHostProfileSettings(context.Background(), hostID, profile.ProfileID, settings.Meta.Revision, farmmodel.HostProfileSettingsContent{CPUThreads: &threads})
	if err != nil {
		t.Fatal(err)
	}
	corrected, err := store2.GetCurrentResolvedSnapshot(context.Background(), workload.WorkloadID)
	if err != nil {
		t.Fatal(err)
	}
	waitExecutionState(t, runtime, corrected.ExecutionID, model.ExecutionRunning)
	if commands2.Count(controllernet.RuntimeStart) != 2 || commands2.Count(controllernet.RuntimeStop) != 2 {
		t.Fatalf("corrected settings did not recover exactly once: starts=%d stops=%d", commands2.Count(controllernet.RuntimeStart), commands2.Count(controllernet.RuntimeStop))
	}
	beforeUnchanged, _ := store2.GetDesiredWorkload(context.Background(), workload.WorkloadID)
	settings, err = store2.UpdateHostProfileSettings(context.Background(), hostID, profile.ProfileID, settings.Meta.Revision, farmmodel.HostProfileSettingsContent{CPUThreads: &threads})
	if err != nil {
		t.Fatal(err)
	}
	afterUnchanged, _ := store2.GetDesiredWorkload(context.Background(), workload.WorkloadID)
	if beforeUnchanged.DesiredGeneration != afterUnchanged.DesiredGeneration || settings.Meta.Revision != 3 {
		t.Fatal("unchanged effective settings changed generation or settings revision")
	}
	time.Sleep(30 * time.Millisecond)
	if commands2.Count(controllernet.RuntimeStart) != 2 || commands2.Count(controllernet.RuntimeStop) != 2 {
		t.Fatal("unchanged effective settings caused runtime commands")
	}
	snapshot = corrected
	hold, err := coordinator2.SetMaintenanceHold(context.Background(), hostID, 0, true, "integration maintenance")
	if err != nil || !hold.Active || hold.Revision != 1 {
		t.Fatalf("enter Maintenance Hold=%+v err=%v", hold, err)
	}
	waitExecutionState(t, runtime, snapshot.ExecutionID, model.ExecutionStopped)
	if current, _ := store2.GetDesiredWorkload(context.Background(), workload.WorkloadID); current.RunState != farmmodel.DesiredRunning {
		t.Fatal("Maintenance Hold rewrote Desired RUNNING")
	}
	stop2()
	if err := <-done2; err != nil {
		t.Fatal(err)
	}
	if err := db2.Close(); err != nil {
		t.Fatal(err)
	}

	server3, commands3, coordinator3, store3, db3, stop3, done3 := startController()
	waitSessionReady(t, server3, hostID)
	time.Sleep(30 * time.Millisecond)
	if commands3.Count(controllernet.RuntimeStart) != 0 || mustRuntimeSnapshot(t, runtime, snapshot.ExecutionID).State != model.ExecutionStopped {
		t.Fatal("persistent Maintenance Hold allowed reconnect restart")
	}
	active, revision := runtime.MaintenanceHold()
	if !active || revision != 1 {
		t.Fatalf("Agent did not retain reconnect hold: active=%t revision=%d", active, revision)
	}
	if _, err := coordinator3.SetMaintenanceHold(context.Background(), hostID, 1, false, ""); err != nil {
		t.Fatal(err)
	}
	waitExecutionState(t, runtime, snapshot.ExecutionID, model.ExecutionRunning)
	if commands3.Count(controllernet.RuntimeStart) != 1 {
		t.Fatalf("hold exit START count=%d", commands3.Count(controllernet.RuntimeStart))
	}
	current, err := store3.GetDesiredWorkload(context.Background(), workload.WorkloadID)
	if err != nil {
		t.Fatal(err)
	}
	stopped := current.DesiredWorkloadContent
	stopped.RunState = farmmodel.DesiredStopped
	if _, err := store3.UpdateDesiredWorkload(context.Background(), workload.WorkloadID, current.Meta.Revision, stopped); err != nil {
		t.Fatal(err)
	}
	waitExecutionState(t, runtime, snapshot.ExecutionID, model.ExecutionStopped)
	if commands3.Count(controllernet.RuntimeStop) != 1 {
		t.Fatalf("final exact STOP count=%d", commands3.Count(controllernet.RuntimeStop))
	}
	stop3()
	if err := <-done3; err != nil {
		t.Fatal(err)
	}
	if err := db3.Close(); err != nil {
		t.Fatal(err)
	}
	stopAgent()
	select {
	case <-agentDone:
	case <-time.After(time.Second):
		t.Fatal("Agent did not stop")
	}
}

func TestGPUDesiredRuntimeFullPathAndHardwareInvalidation(t *testing.T) {
	controllerID, _ := identity.NewControllerID()
	farmID, _ := identity.NewFarmID()
	agentID := testAgent(2)
	hostID := testHost(2)
	trust, err := controllertrust.Open(t.TempDir(), controllerID, farmID)
	if err != nil {
		t.Fatal(err)
	}
	if err := trust.Pair(agentID, hostID); err != nil {
		t.Fatal(err)
	}
	agentDir := t.TempDir()
	if err := agenttrust.Save(agentDir, agenttrust.Binding{ControllerID: controllerID, FarmID: farmID}); err != nil {
		t.Fatal(err)
	}

	files := newMutableGPUFS("0000:01:00.0", "gpu-a")
	inventorySource := inventory.Source{Files: files, Hostname: func() (string, error) { return "duplicate-hostnames-are-valid", nil }, Architecture: "amd64"}
	initialInventory, _ := inventorySource.Discover(hostID)
	if len(initialInventory.GPUs) != 1 {
		t.Fatalf("synthetic GPU discovery=%+v", initialInventory.GPUs)
	}
	deviceA := initialInventory.GPUs[0].DeviceID

	catalog, err := packagecatalog.NewStatic([]farmmodel.PackageRelease{{
		Ref:        farmmodel.PackageRef{PackageID: integrationPackageID, Version: "gpu-1"},
		AdapterIDs: []string{integrationGPUAdapterID},
		Runtime:    farmmodel.RuntimeCapabilities{GPU: true, GPUVendors: []string{"nvidia"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	databaseDir := filepath.Join(t.TempDir(), "controller")
	db, err := controllerdb.Open(context.Background(), databaseDir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store, err := farmconfig.New(db, catalog, farmconfig.Options{})
	if err != nil {
		t.Fatal(err)
	}
	pool, err := store.CreatePool(context.Background(), farmmodel.PoolContent{Name: "synthetic", Address: "127.0.0.1:1", Auth: farmmodel.PoolAuth{Kind: farmmodel.PoolAuthNone}})
	if err != nil {
		t.Fatal(err)
	}
	wallet, err := store.CreateWalletRef(context.Background(), farmmodel.WalletRefContent{Name: "synthetic", Coin: "TEST", Address: "synthetic-public-payout"})
	if err != nil {
		t.Fatal(err)
	}
	profile, err := store.CreateMiningProfile(context.Background(), farmmodel.MiningProfileContent{Name: "gpu-safe", AdapterID: integrationGPUAdapterID, Package: farmmodel.PackageRef{PackageID: integrationPackageID, Version: "gpu-1"}, Mode: farmmodel.ProfileModeMining, Coin: "TEST", PoolID: pool.PoolID, WalletID: wallet.WalletID, LoginPolicy: farmmodel.LoginPolicy{UserTemplate: "${wallet}", WorkerPlacement: farmmodel.WorkerNone}})
	if err != nil {
		t.Fatal(err)
	}
	workload, err := store.CreateDesiredWorkload(context.Background(), farmmodel.DesiredWorkloadContent{Name: "gpu-safe", HostID: hostID, ProfileID: profile.ProfileID, RunState: farmmodel.DesiredRunning, Resources: farmmodel.ResourceClaim{DeviceIDs: []identity.DeviceID{deviceA}}})
	if err != nil {
		t.Fatal(err)
	}

	runtime := supervisor.New(supervisor.Config{StopGrace: 100 * time.Millisecond})
	adapter := &gpuIntegrationAdapter{}
	registry := minerruntime.NewRegistry()
	if err := registry.Register(adapter); err != nil {
		t.Fatal(err)
	}
	minerRuntime := minerruntime.New(runtime, registry, agentDir, nil, initialInventory, minerruntime.Config{PollInterval: 10 * time.Millisecond, RefreshInventory: func() model.Inventory {
		facts, _ := inventorySource.Discover(hostID)
		return facts
	}})
	defer minerRuntime.Shutdown(context.Background())

	dialer := &switchingBufDialer{}
	agentCtx, stopAgent := context.WithCancel(context.Background())
	defer stopAgent()
	agentDone := make(chan error, 1)
	go func() {
		agentDone <- agentnet.Run(agentCtx, agentnet.Config{Target: "buf", InsecureDev: true, TrustDir: agentDir, AgentID: agentID, HostID: hostID, Inventory: inventorySource, Supervisor: runtime, MinerRuntime: minerRuntime, HeartbeatInterval: 10 * time.Millisecond, ReconnectInitial: time.Millisecond, ReconnectMax: 5 * time.Millisecond, Output: log.New(io.Discard, "", 0), Dialer: func(context.Context, string) (net.Conn, error) { return dialer.Dial() }})
	}()

	listener := bufconn.Listen(1024 * 1024)
	dialer.Set(listener)
	server, err := controllernet.New(controllernet.Config{InsecureDev: true, ControllerID: controllerID, FarmID: farmID, Trust: trust, ShutdownGracePeriod: 20 * time.Millisecond, Output: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	commands := &countingCommander{server: server}
	observed := controllerstate.New()
	coordinator := NewCoordinator(store, observed, commands, log.New(io.Discard, "", 0))
	server.SetSessionHandler(coordinator)
	controllerCtx, stopController := context.WithCancel(context.Background())
	defer stopController()
	serverDone := make(chan error, 1)
	go coordinator.Run(controllerCtx, 5*time.Millisecond)
	go func() { serverDone <- server.Serve(controllerCtx, listener) }()

	waitSessionReady(t, server, hostID)
	first := waitCurrentSnapshot(t, store, workload.WorkloadID, 1)
	waitExecutionState(t, runtime, first.ExecutionID, model.ExecutionRunning)
	prepared := adapter.waitPrepared(t, 1)
	if len(prepared.Miner.GPUAssignments) != 1 || prepared.Miner.GPUAssignments[0].DeviceID != deviceA || prepared.Miner.GPUAssignments[0].RuntimeSelector != "0000:01:00.0" {
		t.Fatalf("Agent adapter received wrong binding: %+v", prepared.Miner.GPUAssignments)
	}
	firstProcess := mustRuntimeSnapshot(t, runtime, first.ExecutionID)
	if firstProcess.PID <= 0 || commands.Count(controllernet.RuntimeStart) != 1 {
		t.Fatalf("initial GPU execution=%+v starts=%d", firstProcess, commands.Count(controllernet.RuntimeStart))
	}

	replacementSource := inventory.Source{Files: newMutableGPUFS("0000:02:00.0", "gpu-b"), Hostname: func() (string, error) { return "duplicate-hostnames-are-valid", nil }, Architecture: "amd64"}
	replacementInventory, _ := replacementSource.Discover(hostID)
	if len(replacementInventory.GPUs) != 1 {
		t.Fatalf("replacement discovery=%+v", replacementInventory.GPUs)
	}
	deviceB := replacementInventory.GPUs[0].DeviceID
	content := workload.DesiredWorkloadContent
	content.Resources = farmmodel.ResourceClaim{DeviceIDs: []identity.DeviceID{deviceB}}
	workload, err = store.UpdateDesiredWorkload(context.Background(), workload.WorkloadID, workload.Meta.Revision, content)
	if err != nil {
		t.Fatal(err)
	}
	waitExecutionState(t, runtime, first.ExecutionID, model.ExecutionStopped)
	waitObservedExecutionState(t, observed, hostID, first.ExecutionID, model.ExecutionStopped)
	if commands.Count(controllernet.RuntimeStop) != 1 || commands.Count(controllernet.RuntimeStart) != 1 {
		t.Fatalf("replacement did not stop old execution first: starts=%d stops=%d", commands.Count(controllernet.RuntimeStart), commands.Count(controllernet.RuntimeStop))
	}
	files.SetGPU("0000:02:00.0", "gpu-b")
	replacementInventory, _ = inventorySource.Discover(hostID)
	minerRuntime.SetInventory(replacementInventory)
	session, ok := server.Session(hostID)
	if !ok {
		t.Fatal("Agent session disappeared")
	}
	coordinator.InventoryObserved(session, replacementInventory)
	coordinator.ReconcileHost(context.Background(), hostID)
	second := waitCurrentSnapshot(t, store, workload.WorkloadID, workload.DesiredGeneration)
	waitExecutionState(t, runtime, second.ExecutionID, model.ExecutionRunning)
	prepared = adapter.waitPrepared(t, 2)
	if second.ExecutionID == first.ExecutionID || len(prepared.Miner.GPUAssignments) != 1 || prepared.Miner.GPUAssignments[0].DeviceID != deviceB || prepared.Miner.GPUAssignments[0].RuntimeSelector != "0000:02:00.0" {
		t.Fatalf("replacement binding=%+v first=%s second=%s", prepared.Miner.GPUAssignments, first.ExecutionID, second.ExecutionID)
	}
	if commands.Count(controllernet.RuntimeStart) != 2 || mustRuntimeSnapshot(t, runtime, first.ExecutionID).PID != 0 {
		t.Fatal("replacement overlapped or did not start exactly once")
	}

	// A later authoritative inventory showing replacement hardware under the
	// same selector invalidates and exactly stops the managed execution locally.
	files.SetGPU("0000:02:00.0", "gpu-c")
	invalidInventory, _ := inventorySource.Discover(hostID)
	minerRuntime.SetInventory(invalidInventory)
	waitExecutionState(t, runtime, second.ExecutionID, model.ExecutionStopped)
	if mustRuntimeSnapshot(t, runtime, second.ExecutionID).PID != 0 {
		t.Fatal("invalidated execution remained running")
	}

	content = workload.DesiredWorkloadContent
	content.RunState = farmmodel.DesiredStopped
	if _, err := store.UpdateDesiredWorkload(context.Background(), workload.WorkloadID, workload.Meta.Revision, content); err != nil {
		t.Fatal(err)
	}
	stopController()
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
	stopAgent()
	select {
	case err := <-agentDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Agent did not stop")
	}
}

type sleepRuntimeAdapter struct {
	telemetry minerruntime.TelemetrySource
}

const integrationGPUAdapterID = "integration-gpu-sleep"

type gpuIntegrationAdapter struct {
	mu       sync.Mutex
	prepared []model.ExecutionPlan
}

func (*gpuIntegrationAdapter) ID() string { return integrationGPUAdapterID }
func (*gpuIntegrationAdapter) Capabilities() minerruntime.Capabilities {
	return minerruntime.Capabilities{GPU: true, GPUVendors: []string{"nvidia"}}
}
func (*gpuIntegrationAdapter) Validate(model.MinerSpec, model.Inventory) ([]string, error) {
	return nil, nil
}
func (adapter *gpuIntegrationAdapter) Prepare(_ context.Context, request minerruntime.PrepareRequest) (minerruntime.Prepared, error) {
	plan := request.Plan
	adapter.mu.Lock()
	adapter.prepared = append(adapter.prepared, plan)
	adapter.mu.Unlock()
	plan.Executable = "/bin/sleep"
	plan.Args = []string{"60"}
	plan.RestartPolicy = model.RestartNever
	return minerruntime.Prepared{Plan: plan, Telemetry: gpuHealthyTestTelemetry{}}, nil
}

func (adapter *gpuIntegrationAdapter) waitPrepared(t *testing.T, count int) model.ExecutionPlan {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		adapter.mu.Lock()
		if len(adapter.prepared) >= count {
			plan := adapter.prepared[count-1]
			adapter.mu.Unlock()
			return plan
		}
		adapter.mu.Unlock()
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("adapter did not prepare execution %d", count)
	return model.ExecutionPlan{}
}

type mutableGPUFS struct {
	mu      sync.RWMutex
	entries fstest.MapFS
}

func newMutableGPUFS(selector, hardwareIdentity string) *mutableGPUFS {
	value := &mutableGPUFS{}
	value.SetGPU(selector, hardwareIdentity)
	return value
}

type oneShotInventoryBarrier struct {
	base    fs.FS
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

func (files *oneShotInventoryBarrier) Open(name string) (fs.File, error) {
	files.once.Do(func() {
		close(files.entered)
		<-files.release
	})
	return files.base.Open(name)
}

func (files *mutableGPUFS) SetGPU(selector, hardwareIdentity string) {
	files.mu.Lock()
	defer files.mu.Unlock()
	base := "sys/bus/pci/devices/" + selector
	files.entries = fstest.MapFS{
		base:                &fstest.MapFile{Mode: fs.ModeDir | 0555},
		base + "/class":     &fstest.MapFile{Data: []byte("0x030000\n")},
		base + "/vendor":    &fstest.MapFile{Data: []byte("0x10de\n")},
		base + "/device":    &fstest.MapFile{Data: []byte("0x2684\n")},
		base + "/unique_id": &fstest.MapFile{Data: []byte(hardwareIdentity + "\n")},
	}
}

func (files *mutableGPUFS) Open(name string) (fs.File, error) {
	files.mu.RLock()
	snapshot := make(fstest.MapFS, len(files.entries))
	for path, entry := range files.entries {
		copy := *entry
		copy.Data = append([]byte(nil), entry.Data...)
		snapshot[path] = &copy
	}
	files.mu.RUnlock()
	return snapshot.Open(name)
}

func (sleepRuntimeAdapter) ID() string { return integrationAdapterID }
func (sleepRuntimeAdapter) Capabilities() minerruntime.Capabilities {
	return minerruntime.Capabilities{CPU: true}
}
func (sleepRuntimeAdapter) Validate(model.MinerSpec, model.Inventory) ([]string, error) {
	return nil, nil
}
func (adapter sleepRuntimeAdapter) Prepare(_ context.Context, request minerruntime.PrepareRequest) (minerruntime.Prepared, error) {
	plan := request.Plan
	plan.Executable = "/bin/sleep"
	plan.Args = []string{"60"}
	plan.RestartPolicy = model.RestartNever
	telemetry := adapter.telemetry
	if telemetry == nil {
		telemetry = healthyTestTelemetry{}
	}
	return minerruntime.Prepared{Plan: plan, Telemetry: telemetry}, nil
}

type healthyTestTelemetry struct{}

func (healthyTestTelemetry) Poll(context.Context) (*model.MinerTelemetry, error) {
	healthy := true
	return &model.MinerTelemetry{AdapterID: integrationAdapterID, UsefulWork: &model.UsefulWorkEvidence{Provider: integrationAdapterID, Availability: model.TelemetryAvailable, RuntimeHealthy: &healthy, UsefulWork: model.UsefulWorkConfirmed, Upstream: model.UpstreamUnknown, Confidence: model.EvidenceConfidenceAdapterReported}}, nil
}

type reconnectBarrierTelemetrySource struct {
	mu      sync.Mutex
	block   bool
	entered chan struct{}
	release chan struct{}
}

func (source *reconnectBarrierTelemetrySource) Poll(context.Context) (*model.MinerTelemetry, error) {
	source.mu.Lock()
	block, entered, release := source.block, source.entered, source.release
	source.mu.Unlock()
	if block {
		close(entered)
		<-release
	}
	return healthyTestTelemetry{}.Poll(context.Background())
}

func (source *reconnectBarrierTelemetrySource) blockNext() (<-chan struct{}, func()) {
	source.mu.Lock()
	source.block = true
	source.entered = make(chan struct{})
	source.release = make(chan struct{})
	entered, release := source.entered, source.release
	source.mu.Unlock()
	var once sync.Once
	return entered, func() {
		once.Do(func() {
			source.mu.Lock()
			if source.release == release {
				source.block = false
			}
			source.mu.Unlock()
			close(release)
		})
	}
}

type gpuHealthyTestTelemetry struct{}

func (gpuHealthyTestTelemetry) Poll(context.Context) (*model.MinerTelemetry, error) {
	healthy := true
	return &model.MinerTelemetry{
		AdapterID:   integrationGPUAdapterID,
		CollectedAt: time.Now().UTC(),
		Health:      model.MinerHealthHealthy,
		UsefulWork:  &model.UsefulWorkEvidence{Provider: integrationGPUAdapterID, Availability: model.TelemetryAvailable, RuntimeHealthy: &healthy, UsefulWork: model.UsefulWorkConfirmed, Upstream: model.UpstreamUnknown, Confidence: model.EvidenceConfidenceAdapterReported},
	}, nil
}

func mustIntegrationPackageID(value string) identity.PackageID {
	id, err := identity.ParsePackageID(value)
	if err != nil {
		panic(err)
	}
	return id
}

type countingCommander struct {
	server *controllernet.Server
	mu     sync.Mutex
	counts map[controllernet.RuntimeKind]int
}

func (commands *countingCommander) Session(host identity.HostID) (controllernet.SessionInfo, bool) {
	return commands.server.Session(host)
}
func (commands *countingCommander) SendRuntime(ctx context.Context, info controllernet.SessionInfo, request controllernet.RuntimeRequest) (uint64, error) {
	commands.mu.Lock()
	if commands.counts == nil {
		commands.counts = make(map[controllernet.RuntimeKind]int)
	}
	commands.counts[request.Kind]++
	commands.mu.Unlock()
	return commands.server.SendRuntime(ctx, info, request)
}
func (commands *countingCommander) SendMaintenanceHold(ctx context.Context, info controllernet.SessionInfo, active bool, revision uint64) error {
	return commands.server.SendMaintenanceHold(ctx, info, active, revision)
}
func (commands *countingCommander) AcquireMaintenanceAuthority(host identity.HostID) func() {
	return commands.server.AcquireMaintenanceAuthority(host)
}
func (commands *countingCommander) Count(kind controllernet.RuntimeKind) int {
	commands.mu.Lock()
	defer commands.mu.Unlock()
	return commands.counts[kind]
}

type switchingBufDialer struct {
	mu       sync.RWMutex
	listener *bufconn.Listener
}

func (dialer *switchingBufDialer) Dial() (net.Conn, error) {
	for {
		dialer.mu.RLock()
		listener := dialer.listener
		dialer.mu.RUnlock()
		if listener != nil {
			return listener.Dial()
		}
		time.Sleep(time.Millisecond)
	}
}
func (dialer *switchingBufDialer) Set(listener *bufconn.Listener) {
	dialer.mu.Lock()
	dialer.listener = listener
	dialer.mu.Unlock()
}

func waitSessionReady(t *testing.T, server *controllernet.Server, host identity.HostID) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if session, ok := server.Session(host); ok && session.Ready {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("Agent session did not become READY")
}

func waitExecutionState(t *testing.T, runtime *supervisor.Supervisor, executionID identity.ExecutionID, state model.ExecutionStatus) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if snapshot, ok := runtime.Get(executionID); ok && snapshot.State == state {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("execution %s did not reach %s", executionID, state)
}

func mustRuntimeSnapshot(t *testing.T, runtime *supervisor.Supervisor, executionID identity.ExecutionID) supervisor.Snapshot {
	t.Helper()
	snapshot, ok := runtime.Get(executionID)
	if !ok {
		t.Fatalf("execution %s missing", executionID)
	}
	return snapshot
}

func waitCurrentSnapshot(t *testing.T, store *farmconfig.Service, workloadID identity.WorkloadID, generation uint64) farmmodel.ResolvedExecutionSnapshot {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		snapshot, err := store.GetCurrentResolvedSnapshot(context.Background(), workloadID)
		if err == nil && snapshot.DesiredGeneration == generation {
			return snapshot
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("workload %s did not resolve generation %d", workloadID, generation)
	return farmmodel.ResolvedExecutionSnapshot{}
}

func waitObservedExecutionState(t *testing.T, observed *controllerstate.Store, hostID identity.HostID, executionID identity.ExecutionID, state model.ExecutionStatus) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		current, ok := observed.Get(hostID)
		if ok {
			for _, execution := range current.Executions {
				if execution.ExecutionID == executionID && execution.Status == state {
					return
				}
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("Controller did not observe execution %s in state %s", executionID, state)
}

func waitUsefulWorkConfirmed(t *testing.T, observed *controllerstate.Store, hostID identity.HostID, executionID identity.ExecutionID) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		current, ok := observed.Get(hostID)
		if ok {
			for _, execution := range current.Executions {
				if execution.ExecutionID == executionID && execution.UsefulWork != nil && execution.UsefulWork.Availability == model.TelemetryAvailable && execution.UsefulWork.UsefulWork == model.UsefulWorkConfirmed && execution.MinerTelemetry != nil && execution.MinerTelemetry.Health == model.MinerHealthMining {
					return
				}
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("Controller did not receive confirmed useful work for %s", executionID)
}
