package reconcile

import (
	"context"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/le0xdon/le0xfarm/internal/agentnet"
	"github.com/le0xdon/le0xfarm/internal/agenttrust"
	"github.com/le0xdon/le0xfarm/internal/controllerdb"
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
	catalog, err := packagecatalog.NewStatic([]farmmodel.PackageRelease{{Ref: farmmodel.PackageRef{PackageID: integrationPackageID, Version: "1"}, AdapterIDs: []string{integrationAdapterID}, Tuning: farmmodel.TuningCapabilities{CPUThreads: true, HugePages: true, MSR: true}}})
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
	if err := registry.Register(sleepRuntimeAdapter{}); err != nil {
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

	startController := func() (*controllernet.Server, *countingCommander, *farmconfig.Service, *controllerdb.DB, context.CancelFunc, <-chan error) {
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
		server, err := controllernet.New(controllernet.Config{InsecureDev: true, ControllerID: controllerID, FarmID: farmID, Trust: trust, ShutdownGracePeriod: 20 * time.Millisecond})
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
		return server, commands, store, db, cancel, done
	}

	server1, commands1, _, db1, stop1, done1 := startController()
	waitSessionReady(t, server1, hostID)
	waitExecutionState(t, runtime, snapshot.ExecutionID, model.ExecutionRunning)
	first := mustRuntimeSnapshot(t, runtime, snapshot.ExecutionID)
	if commands1.Count(controllernet.RuntimeStart) != 1 {
		t.Fatalf("initial START count=%d", commands1.Count(controllernet.RuntimeStart))
	}
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

	server2, commands2, store2, db2, stop2, done2 := startController()
	waitSessionReady(t, server2, hostID)
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
	stopped := workload.DesiredWorkloadContent
	stopped.RunState = farmmodel.DesiredStopped
	workload, err = store2.UpdateDesiredWorkload(context.Background(), workload.WorkloadID, workload.Meta.Revision, stopped)
	if err != nil {
		t.Fatal(err)
	}
	// The periodic coordinator notices the persistent desired mutation.
	waitExecutionState(t, runtime, snapshot.ExecutionID, model.ExecutionStopped)
	if commands2.Count(controllernet.RuntimeStop) != 3 {
		t.Fatalf("STOP count=%d", commands2.Count(controllernet.RuntimeStop))
	}
	stop2()
	if err := <-done2; err != nil {
		t.Fatal(err)
	}
	if err := db2.Close(); err != nil {
		t.Fatal(err)
	}

	server3, commands3, _, db3, stop3, done3 := startController()
	waitSessionReady(t, server3, hostID)
	time.Sleep(30 * time.Millisecond)
	if commands3.Count(controllernet.RuntimeStart) != 0 || commands3.Count(controllernet.RuntimeStop) != 0 || mustRuntimeSnapshot(t, runtime, snapshot.ExecutionID).State != model.ExecutionStopped {
		t.Fatal("STOPPED desired state was not stable after reconnect")
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

type sleepRuntimeAdapter struct{}

func (sleepRuntimeAdapter) ID() string { return integrationAdapterID }
func (sleepRuntimeAdapter) Capabilities() minerruntime.Capabilities {
	return minerruntime.Capabilities{CPU: true}
}
func (sleepRuntimeAdapter) Validate(model.MinerSpec, model.Inventory) ([]string, error) {
	return nil, nil
}
func (sleepRuntimeAdapter) Prepare(_ context.Context, request minerruntime.PrepareRequest) (minerruntime.Prepared, error) {
	plan := request.Plan
	plan.Executable = "/bin/sleep"
	plan.Args = []string{"60"}
	plan.RestartPolicy = model.RestartNever
	return minerruntime.Prepared{Plan: plan, Telemetry: healthyTestTelemetry{}}, nil
}

type healthyTestTelemetry struct{}

func (healthyTestTelemetry) Poll(context.Context) (*model.MinerTelemetry, error) {
	return &model.MinerTelemetry{AdapterID: integrationAdapterID, Health: model.MinerHealthHealthy}, nil
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
