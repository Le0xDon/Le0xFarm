package minerruntime

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/le0xdon/le0xfarm/internal/farmerr"
	"github.com/le0xdon/le0xfarm/internal/identity"
	"github.com/le0xdon/le0xfarm/internal/model"
	"github.com/le0xdon/le0xfarm/internal/runtime/supervisor"
)

type fakeAdapter struct {
	source         TelemetrySource
	mu             sync.Mutex
	cleaned        bool
	dropOwnership  bool
	capabilities   *Capabilities
	executable     string
	args           []string
	prepareEntered chan struct{}
	prepareRelease chan struct{}
	prepareOnce    sync.Once
}

func (a *fakeAdapter) ID() string { return "test-no-http" }
func (a *fakeAdapter) Capabilities() Capabilities {
	if a.capabilities != nil {
		return *a.capabilities
	}
	return Capabilities{CPU: true, StdoutTelemetry: true}
}
func (a *fakeAdapter) Validate(spec model.MinerSpec, _ model.Inventory) ([]string, error) {
	if spec.AdapterID != a.ID() {
		return nil, errors.New("wrong adapter")
	}
	return []string{"test adapter warning"}, nil
}
func (a *fakeAdapter) Prepare(ctx context.Context, request PrepareRequest) (Prepared, error) {
	if a.prepareEntered != nil {
		a.prepareOnce.Do(func() { close(a.prepareEntered) })
	}
	if a.prepareRelease != nil {
		select {
		case <-a.prepareRelease:
		case <-ctx.Done():
			return Prepared{}, ctx.Err()
		}
	}
	resolved := request.Plan
	if a.dropOwnership {
		resolved.Ownership = model.WorkloadOwnership{}
		resolved.HostID = identity.HostID{}
	}
	resolved.Executable = a.executable
	resolved.Args = append([]string(nil), a.args...)
	if resolved.Executable == "" {
		resolved.Executable = "/bin/sleep"
		resolved.Args = []string{"60"}
	}
	return Prepared{Plan: resolved, Telemetry: a.source, Cleanup: func() error {
		a.mu.Lock()
		defer a.mu.Unlock()
		a.cleaned = true
		return nil
	}}, nil
}

func TestAdapterCannotDiscardOwnershipMetadata(t *testing.T) {
	source := &sequenceSource{results: []sourceResult{{telemetry: &model.MinerTelemetry{AdapterID: "test-no-http"}}}}
	registry := NewRegistry()
	if err := registry.Register(&fakeAdapter{source: source, dropOwnership: true}); err != nil {
		t.Fatal(err)
	}
	runtime := supervisor.New(supervisor.Config{StopGrace: 20 * time.Millisecond})
	manager := New(runtime, registry, t.TempDir(), nil, model.Inventory{}, Config{})
	plan := testPlan(t, model.MinerModeStress)
	workloadID, _ := identity.NewWorkloadID()
	hostID, _ := identity.NewHostID()
	plan.HostID = hostID
	plan.Ownership = model.WorkloadOwnership{WorkloadID: workloadID, DesiredGeneration: 5, ResolvedHash: "sha256:" + strings.Repeat("a", 64), HostID: hostID, CPU: true}
	manager.SetInventory(model.Inventory{Host: model.Host{HostID: hostID}, CPU: model.CPU{Threads: 4}})
	observation, _, err := manager.Start(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Shutdown(context.Background())
	if observation.Process.Ownership.WorkloadID != workloadID || observation.Process.Ownership.DesiredGeneration != 5 || observation.Process.Ownership.HostID != hostID {
		t.Fatalf("adapter discarded ownership: %+v", observation.Process.Ownership)
	}
}

type sequenceSource struct {
	mu      sync.Mutex
	results []sourceResult
	index   int
}
type sourceResult struct {
	telemetry *model.MinerTelemetry
	err       error
}

func (s *sequenceSource) Poll(context.Context) (*model.MinerTelemetry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	index := s.index
	if index >= len(s.results) {
		index = len(s.results) - 1
	} else {
		s.index++
	}
	return s.results[index].telemetry, s.results[index].err
}

func testPlan(t *testing.T, mode model.MinerMode) model.ExecutionPlan {
	t.Helper()
	executionID, err := identity.NewExecutionID()
	if err != nil {
		t.Fatal(err)
	}
	packageID, err := identity.NewPackageID()
	if err != nil {
		t.Fatal(err)
	}
	hostID, err := identity.NewHostID()
	if err != nil {
		t.Fatal(err)
	}
	workloadID, err := identity.NewWorkloadID()
	if err != nil {
		t.Fatal(err)
	}
	return model.ExecutionPlan{ExecutionID: executionID, HostID: hostID, Ownership: model.WorkloadOwnership{WorkloadID: workloadID, DesiredGeneration: 1, ResolvedHash: "sha256:" + strings.Repeat("a", 64), HostID: hostID, CPU: true}, RestartPolicy: model.RestartNever, Miner: &model.MinerSpec{AdapterID: "test-no-http", SpecVersion: 1, PackageID: packageID, PackageVersion: "1", Mode: mode}}
}

func TestSecondAdapterUsesGenericRuntimeWithoutHTTP(t *testing.T) {
	hashrate := 42.5
	source := &sequenceSource{results: []sourceResult{{telemetry: &model.MinerTelemetry{AdapterID: "test-no-http", HashrateShortHPS: &hashrate}}}}
	adapter := &fakeAdapter{source: source}
	registry := NewRegistry()
	if err := registry.Register(adapter); err != nil {
		t.Fatal(err)
	}
	processes := supervisor.New(supervisor.Config{StopGrace: 20 * time.Millisecond})
	manager := New(processes, registry, t.TempDir(), nil, model.Inventory{}, Config{PollInterval: 5 * time.Millisecond, StartupGrace: time.Millisecond, StaleAfter: time.Second})
	plan := testPlan(t, model.MinerModeStress)
	manager.SetInventory(model.Inventory{Host: model.Host{HostID: plan.HostID}, CPU: model.CPU{Threads: 4}})
	observation, _, err := manager.Start(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if observation.Process.State != model.ExecutionRunning || observation.Process.PID <= 0 {
		t.Fatalf("process=%+v", observation.Process)
	}
	if observation.Telemetry == nil || observation.Telemetry.AdapterID != adapter.ID() || observation.Telemetry.Age != 0 {
		t.Fatalf("initial telemetry=%+v", observation.Telemetry)
	}
	deadline := time.Now().Add(time.Second)
	for {
		items := manager.List()
		if len(items) == 1 && items[0].Telemetry != nil && items[0].Telemetry.HashrateShortHPS != nil {
			if items[0].Telemetry.Health == model.MinerHealthMining {
				t.Fatal("STRESS was reported as MINING")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("adapter telemetry was not normalized")
		}
		time.Sleep(time.Millisecond)
	}
	stopped, _, err := manager.Stop(plan.ExecutionID)
	if err != nil {
		t.Fatal(err)
	}
	if stopped.Process.State != model.ExecutionStopped || stopped.Telemetry != nil {
		t.Fatalf("stopped observation retained active telemetry: %+v", stopped)
	}
	adapter.mu.Lock()
	cleaned := adapter.cleaned
	adapter.mu.Unlock()
	if !cleaned {
		t.Fatal("adapter cleanup was not called")
	}
}

func TestGPUStartRequiresExactFreshHardwareBinding(t *testing.T) {
	device, err := identity.ParseDeviceID("device_00000000000000000000000000000001")
	if err != nil {
		t.Fatal(err)
	}
	caps := Capabilities{GPU: true, GPUVendors: []string{"nvidia"}, StdoutTelemetry: true}
	registry := NewRegistry()
	if err := registry.Register(&fakeAdapter{source: &sequenceSource{results: []sourceResult{{telemetry: &model.MinerTelemetry{AdapterID: "test-no-http"}}}}, capabilities: &caps}); err != nil {
		t.Fatal(err)
	}
	manager := New(supervisor.New(supervisor.Config{StopGrace: 20 * time.Millisecond}), registry, t.TempDir(), nil, model.Inventory{}, Config{})
	defer manager.Shutdown(context.Background())
	plan := testPlan(t, model.MinerModeStress)
	plan.Ownership.CPU = false
	plan.Ownership.DeviceIDs = []identity.DeviceID{device}
	plan.DeviceIDs = []identity.DeviceID{device}
	plan.Miner.GPUDeviceIDs = []identity.DeviceID{device}
	plan.Miner.GPUAssignments = []model.GPUAssignment{{DeviceID: device, HardwareIdentity: "gpu-a", RuntimeSelector: "01:00.0"}}
	inventory := model.Inventory{Host: model.Host{HostID: plan.HostID}, GPUs: []model.GPU{{DeviceID: device, UUID: "gpu-a", PCIBusID: "01:00.0", Vendor: "nvidia"}}}
	manager.SetInventory(inventory)
	started, _, err := manager.Start(context.Background(), plan)
	if err != nil || started.Process.PID <= 0 {
		t.Fatalf("valid GPU plan did not start: observation=%+v err=%v", started, err)
	}
	if _, _, err := manager.Stop(plan.ExecutionID); err != nil {
		t.Fatal(err)
	}

	for _, changed := range []model.Inventory{
		{Host: inventory.Host, GPUs: []model.GPU{{DeviceID: device, UUID: "replacement", PCIBusID: "01:00.0", Vendor: "nvidia"}}},
		{Host: inventory.Host, GPUs: []model.GPU{{DeviceID: device, UUID: "gpu-a", PCIBusID: "02:00.0", Vendor: "nvidia"}}},
	} {
		manager.SetInventory(changed)
		plan.ExecutionID, _ = identity.NewExecutionID()
		if _, _, err := manager.Start(context.Background(), plan); err == nil {
			t.Fatalf("stale GPU binding started against %+v", changed.GPUs)
		}
		for _, observation := range manager.List() {
			if observation.Process.State != model.ExecutionStopped {
				t.Fatalf("failed GPU validation left an active process: %+v", observation)
			}
		}
	}
}

func TestGPUStartRevalidatesAfterPrepareBarrier(t *testing.T) {
	device, _ := identity.ParseDeviceID("device_00000000000000000000000000000001")
	replacementDevice, _ := identity.ParseDeviceID("device_00000000000000000000000000000002")
	caps := Capabilities{GPU: true, GPUVendors: []string{"nvidia"}, StdoutTelemetry: true}
	entered := make(chan struct{})
	release := make(chan struct{})
	adapter := &fakeAdapter{
		source:         &sequenceSource{results: []sourceResult{{telemetry: &model.MinerTelemetry{AdapterID: "test-no-http"}}}},
		capabilities:   &caps,
		prepareEntered: entered,
		prepareRelease: release,
	}
	registry := NewRegistry()
	if err := registry.Register(adapter); err != nil {
		t.Fatal(err)
	}
	plan, original := gpuTestPlan(t, device, "gpu-a", "01:00.0")
	replacement := model.Inventory{Host: original.Host, GPUs: []model.GPU{{DeviceID: replacementDevice, UUID: "gpu-b", PCIBusID: "01:00.0", Vendor: "nvidia"}}}
	var inventoryMu sync.Mutex
	current := original
	manager := New(supervisor.New(supervisor.Config{StopGrace: 20 * time.Millisecond}), registry, t.TempDir(), nil, original, Config{RefreshInventory: func() model.Inventory {
		inventoryMu.Lock()
		defer inventoryMu.Unlock()
		return cloneInventory(current)
	}})
	defer manager.Shutdown(context.Background())

	result := make(chan error, 1)
	go func() {
		_, _, err := manager.Start(context.Background(), plan)
		result <- err
	}()
	<-entered
	inventoryMu.Lock()
	current = replacement
	inventoryMu.Unlock()
	manager.SetInventory(replacement)
	close(release)
	if err := <-result; errorCode(err) != farmerr.INCOMPATIBLE_HARDWARE {
		t.Fatalf("START error=%v", err)
	}
	if snapshot, ok := manager.supervisor.Get(plan.ExecutionID); !ok || snapshot.PID != 0 || snapshot.State == model.ExecutionRunning {
		t.Fatalf("process was dispatched after binding changed during Prepare: %+v", snapshot)
	}
}

func TestFreshInventoryStopsOnlyInvalidManagedGPUExecution(t *testing.T) {
	device, _ := identity.ParseDeviceID("device_00000000000000000000000000000001")
	replacementDevice, _ := identity.ParseDeviceID("device_00000000000000000000000000000002")
	caps := Capabilities{GPU: true, GPUVendors: []string{"nvidia"}, StdoutTelemetry: true}
	registry := NewRegistry()
	if err := registry.Register(&fakeAdapter{source: &sequenceSource{results: []sourceResult{{telemetry: &model.MinerTelemetry{AdapterID: "test-no-http"}}}}, capabilities: &caps}); err != nil {
		t.Fatal(err)
	}
	plan, original := gpuTestPlan(t, device, "gpu-a", "01:00.0")
	manager := New(supervisor.New(supervisor.Config{StopGrace: 20 * time.Millisecond}), registry, t.TempDir(), nil, original, Config{})
	defer manager.Shutdown(context.Background())
	started, _, err := manager.Start(context.Background(), plan)
	if err != nil || started.Process.PID <= 0 {
		t.Fatalf("managed start: %+v %v", started, err)
	}
	unmanaged := exec.Command("/bin/sleep", "60")
	if err := unmanaged.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = unmanaged.Process.Kill()
		_ = unmanaged.Wait()
	}()

	replacement := model.Inventory{Host: original.Host, GPUs: []model.GPU{{DeviceID: replacementDevice, UUID: "gpu-b", PCIBusID: "01:00.0", Vendor: "nvidia"}}}
	manager.SetInventory(replacement)
	snapshot, ok := manager.supervisor.Get(plan.ExecutionID)
	if !ok || snapshot.State != model.ExecutionStopped || snapshot.PID != 0 {
		t.Fatalf("invalid managed execution continued: %+v", snapshot)
	}
	if err := syscall.Kill(unmanaged.Process.Pid, 0); err != nil {
		t.Fatalf("unmanaged process was affected: %v", err)
	}
	for _, observation := range manager.List() {
		if observation.Process.PID != 0 || observation.Process.State == model.ExecutionRunning {
			t.Fatalf("replacement GPU was selected automatically: %+v", observation)
		}
	}
}

func TestGPUWatchdogFinalValidationBarrier(t *testing.T) {
	device, _ := identity.ParseDeviceID("device_00000000000000000000000000000001")
	replacementDevice, _ := identity.ParseDeviceID("device_00000000000000000000000000000002")
	caps := Capabilities{GPU: true, GPUVendors: []string{"nvidia"}, StdoutTelemetry: true}
	registry := NewRegistry()
	adapter := &fakeAdapter{source: &sequenceSource{results: []sourceResult{{telemetry: &model.MinerTelemetry{AdapterID: "test-no-http"}}}}, capabilities: &caps, executable: "/bin/false"}
	if err := registry.Register(adapter); err != nil {
		t.Fatal(err)
	}
	plan, original := gpuTestPlan(t, device, "gpu-a", "01:00.0")
	plan.RestartPolicy = model.RestartOnFailure
	replacement := model.Inventory{Host: original.Host, GPUs: []model.GPU{{DeviceID: replacementDevice, UUID: "gpu-b", PCIBusID: "01:00.0", Vendor: "nvidia"}}}
	watchdogValidation := make(chan struct{})
	releaseValidation := make(chan struct{})
	var refreshes atomic.Int32
	var inventoryMu sync.Mutex
	current := original
	manager := New(supervisor.New(supervisor.Config{RestartInitial: time.Millisecond, RestartMax: time.Millisecond, CrashLimit: 3}), registry, t.TempDir(), nil, original, Config{RefreshInventory: func() model.Inventory {
		call := refreshes.Add(1)
		if call == 3 {
			close(watchdogValidation)
			<-releaseValidation
		}
		inventoryMu.Lock()
		defer inventoryMu.Unlock()
		return cloneInventory(current)
	}})
	defer manager.Shutdown(context.Background())
	if _, _, err := manager.Start(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	select {
	case <-watchdogValidation:
	case <-time.After(time.Second):
		t.Fatal("watchdog did not reach final validation barrier")
	}
	inventoryMu.Lock()
	current = replacement
	inventoryMu.Unlock()
	close(releaseValidation)
	waitUntil(t, time.Second, func() bool {
		snapshot, ok := manager.supervisor.Get(plan.ExecutionID)
		return ok && snapshot.PID == 0 && (snapshot.State == model.ExecutionStopped || snapshot.State == model.ExecutionFailed)
	})
	snapshot, _ := manager.supervisor.Get(plan.ExecutionID)
	if snapshot.PID != 0 || snapshot.RestartCount != 1 {
		t.Fatalf("watchdog restarted stale binding: %+v", snapshot)
	}
}

func TestGPUWatchdogRestartRevalidatesPhysicalIdentity(t *testing.T) {
	device, _ := identity.ParseDeviceID("device_00000000000000000000000000000001")
	caps := Capabilities{GPU: true, GPUVendors: []string{"nvidia"}, StdoutTelemetry: true}
	registry := NewRegistry()
	if err := registry.Register(&fakeAdapter{source: &sequenceSource{results: []sourceResult{{telemetry: &model.MinerTelemetry{AdapterID: "test-no-http"}}}}, capabilities: &caps, executable: "/bin/false"}); err != nil {
		t.Fatal(err)
	}
	processes := supervisor.New(supervisor.Config{RestartInitial: time.Millisecond, RestartMax: time.Millisecond, CrashLimit: 3})
	plan := testPlan(t, model.MinerModeStress)
	plan.RestartPolicy = model.RestartOnFailure
	plan.Ownership.CPU = false
	plan.Ownership.DeviceIDs = []identity.DeviceID{device}
	plan.DeviceIDs = []identity.DeviceID{device}
	plan.Miner.GPUDeviceIDs = []identity.DeviceID{device}
	plan.Miner.GPUAssignments = []model.GPUAssignment{{DeviceID: device, HardwareIdentity: "gpu-a", RuntimeSelector: "01:00.0"}}
	original := model.Inventory{Host: model.Host{HostID: plan.HostID}, GPUs: []model.GPU{{DeviceID: device, UUID: "gpu-a", PCIBusID: "01:00.0", Vendor: "nvidia"}}}
	replacement := model.Inventory{Host: original.Host, GPUs: []model.GPU{{DeviceID: device, UUID: "gpu-replacement", PCIBusID: "01:00.0", Vendor: "nvidia"}}}
	var refreshes atomic.Int32
	manager := New(processes, registry, t.TempDir(), nil, original, Config{PollInterval: time.Millisecond, RefreshInventory: func() model.Inventory {
		if refreshes.Add(1) <= 2 {
			return original
		}
		return replacement
	}})
	defer manager.Shutdown(context.Background())
	if _, _, err := manager.Start(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		items := manager.List()
		if len(items) == 1 && (items[0].Process.State == model.ExecutionStopped || items[0].Process.State == model.ExecutionFailed) {
			if items[0].Process.PID != 0 || items[0].Process.RestartCount != 1 || refreshes.Load() < 2 {
				t.Fatalf("unsafe watchdog result: observation=%+v refreshes=%d", items[0], refreshes.Load())
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("watchdog did not fail closed: %+v", manager.List())
}

func gpuTestPlan(t *testing.T, device identity.DeviceID, hardwareIdentity, selector string) (model.ExecutionPlan, model.Inventory) {
	t.Helper()
	plan := testPlan(t, model.MinerModeStress)
	plan.Ownership.CPU = false
	plan.Ownership.DeviceIDs = []identity.DeviceID{device}
	plan.DeviceIDs = []identity.DeviceID{device}
	plan.Miner.GPUDeviceIDs = []identity.DeviceID{device}
	plan.Miner.GPUAssignments = []model.GPUAssignment{{DeviceID: device, HardwareIdentity: hardwareIdentity, RuntimeSelector: selector}}
	inventory := model.Inventory{Host: model.Host{HostID: plan.HostID}, GPUs: []model.GPU{{DeviceID: device, UUID: hardwareIdentity, PCIBusID: selector, Vendor: "nvidia"}}}
	return plan, inventory
}

func waitUntil(t *testing.T, timeout time.Duration, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition was not reached")
}

func errorCode(err error) farmerr.Code {
	code, _ := farmerr.CodeOf(err)
	return code
}

func TestEvaluateMinerStatus(t *testing.T) {
	now := time.Now().UTC()
	hash, zero := 100.0, 0.0
	connected, disconnected := true, false
	accepted, rejected := uint64(7), uint64(3)
	running := supervisor.Snapshot{State: model.ExecutionRunning, StartedAt: now.Add(-time.Minute)}
	cases := []struct {
		name      string
		mode      model.MinerMode
		process   supervisor.Snapshot
		telemetry *model.MinerTelemetry
		health    model.MinerHealth
		code      farmerr.Code
	}{
		{"startup", model.MinerModeMining, supervisor.Snapshot{State: model.ExecutionRunning, StartedAt: now}, nil, model.MinerHealthStarting, ""},
		{"running-only", model.MinerModeMining, running, nil, model.MinerHealthDegraded, farmerr.RPC_UNREACHABLE},
		{"mining", model.MinerModeMining, running, &model.MinerTelemetry{CollectedAt: now, HashrateShortHPS: &hash, PoolConnected: &connected}, model.MinerHealthMining, ""},
		{"zero", model.MinerModeMining, running, &model.MinerTelemetry{CollectedAt: now, HashrateShortHPS: &zero, PoolConnected: &connected}, model.MinerHealthDegraded, farmerr.ZERO_HASHRATE},
		{"pool", model.MinerModeMining, running, &model.MinerTelemetry{CollectedAt: now, HashrateShortHPS: &hash, PoolConnected: &disconnected}, model.MinerHealthDegraded, farmerr.POOL_UNREACHABLE},
		{"rejects", model.MinerModeMining, running, &model.MinerTelemetry{CollectedAt: now, HashrateShortHPS: &hash, PoolConnected: &connected, AcceptedShares: &accepted, RejectedShares: &rejected}, model.MinerHealthDegraded, farmerr.TOO_MANY_REJECTS},
		{"stale", model.MinerModeMining, running, &model.MinerTelemetry{CollectedAt: now.Add(-time.Minute), Age: time.Minute}, model.MinerHealthDegraded, farmerr.RPC_UNREACHABLE},
		{"stress-positive", model.MinerModeStress, running, &model.MinerTelemetry{CollectedAt: now, HashrateShortHPS: &hash}, model.MinerHealthHealthy, ""},
		{"failed", model.MinerModeMining, supervisor.Snapshot{State: model.ExecutionFailed, LastError: "crash"}, nil, model.MinerHealthError, farmerr.PROCESS_CRASHED},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Evaluate(tc.mode, tc.process, tc.telemetry, now, 30*time.Second, 10*time.Second)
			if got.Health != tc.health || got.ErrorCode != tc.code {
				t.Fatalf("health=%s code=%s", got.Health, got.ErrorCode)
			}
		})
	}
}

func TestOverallStatusTracksOnlyHealthyMiningExecution(t *testing.T) {
	hash := 100.0
	connected := true
	source := &sequenceSource{results: []sourceResult{{telemetry: &model.MinerTelemetry{AdapterID: "test-no-http", HashrateShortHPS: &hash, PoolConnected: &connected}}}}
	registry := NewRegistry()
	if err := registry.Register(&fakeAdapter{source: source}); err != nil {
		t.Fatal(err)
	}
	manager := New(supervisor.New(supervisor.Config{StopGrace: 20 * time.Millisecond}), registry, t.TempDir(), nil, model.Inventory{}, Config{PollInterval: time.Millisecond, StartupGrace: time.Millisecond, StaleAfter: time.Second})
	plan := testPlan(t, model.MinerModeMining)
	manager.SetInventory(model.Inventory{Host: model.Host{HostID: plan.HostID}, CPU: model.CPU{Threads: 4}})
	if _, _, err := manager.Start(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for manager.OverallStatus() != "MINING" && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := manager.OverallStatus(); got != "MINING" {
		t.Fatalf("status=%s", got)
	}
	if _, _, err := manager.Stop(plan.ExecutionID); err != nil {
		t.Fatal(err)
	}
	if got := manager.OverallStatus(); got != "IDLE" {
		t.Fatalf("stopped status=%s", got)
	}
}

func TestOverallAgentStatusSemanticsAndPrecedence(t *testing.T) {
	running := supervisor.Snapshot{State: model.ExecutionRunning}
	failed := supervisor.Snapshot{State: model.ExecutionFailed}
	stopped := supervisor.Snapshot{State: model.ExecutionStopped}
	starting := &model.MinerTelemetry{Health: model.MinerHealthStarting}
	mining := &model.MinerTelemetry{Health: model.MinerHealthMining}
	degraded := &model.MinerTelemetry{Health: model.MinerHealthDegraded}
	terminal := &model.MinerTelemetry{Health: model.MinerHealthError}
	cases := []struct {
		name  string
		items []overallStatusItem
		want  model.AgentState
	}{
		{"none", nil, model.AgentStateIdle},
		{"stress", []overallStatusItem{{mode: model.MinerModeStress, process: running, telemetry: starting}}, model.AgentStateIdle},
		{"benchmark", []overallStatusItem{{mode: model.MinerModeBenchmark, process: running, telemetry: degraded}}, model.AgentStateIdle},
		{"initializing", []overallStatusItem{{mode: model.MinerModeMining, process: running, telemetry: starting}}, model.AgentStateStarting},
		{"healthy", []overallStatusItem{{mode: model.MinerModeMining, process: running, telemetry: mining}}, model.AgentStateMining},
		{"degraded", []overallStatusItem{{mode: model.MinerModeMining, process: running, telemetry: degraded}}, model.AgentStateDegraded},
		{"terminal", []overallStatusItem{{mode: model.MinerModeMining, process: failed, telemetry: terminal}}, model.AgentStateError},
		{"stopped-terminal", []overallStatusItem{{mode: model.MinerModeMining, process: stopped, telemetry: terminal}}, model.AgentStateIdle},
		{"error-precedence", []overallStatusItem{{mode: model.MinerModeMining, process: running, telemetry: mining}, {mode: model.MinerModeMining, process: running, telemetry: starting}, {mode: model.MinerModeMining, process: running, telemetry: degraded}, {mode: model.MinerModeMining, process: failed, telemetry: terminal}}, model.AgentStateError},
		{"degraded-precedence", []overallStatusItem{{mode: model.MinerModeMining, process: running, telemetry: mining}, {mode: model.MinerModeMining, process: running, telemetry: starting}, {mode: model.MinerModeMining, process: running, telemetry: degraded}}, model.AgentStateDegraded},
		{"starting-precedence", []overallStatusItem{{mode: model.MinerModeMining, process: running, telemetry: mining}, {mode: model.MinerModeMining, process: running, telemetry: starting}}, model.AgentStateStarting},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := aggregateOverallStatus(tc.items); got != tc.want {
				t.Fatalf("status=%s want=%s", got, tc.want)
			}
		})
	}
}

func TestTelemetryFailureDoesNotExposeAdapterErrorText(t *testing.T) {
	secret := "pool-password-must-not-leak"
	if got := telemetryErrorMessage(errors.New("request failed with " + secret)); strings.Contains(got, secret) {
		t.Fatalf("untyped adapter error leaked: %q", got)
	}
	typed := farmerr.Error{Code: farmerr.RPC_UNREACHABLE, HumanMessage: "local telemetry unavailable", Details: map[string]string{"credential": secret}}
	if got := telemetryErrorMessage(typed); got != typed.HumanMessage || strings.Contains(got, secret) {
		t.Fatalf("typed adapter error leaked details: %q", got)
	}
}

func TestTelemetryRecoversAfterTemporaryFailure(t *testing.T) {
	hash := 10.0
	source := &sequenceSource{results: []sourceResult{{err: errors.New("temporary")}, {telemetry: &model.MinerTelemetry{AdapterID: "test-no-http", HashrateShortHPS: &hash}}}}
	adapter := &fakeAdapter{source: source}
	registry := NewRegistry()
	_ = registry.Register(adapter)
	manager := New(supervisor.New(supervisor.Config{StopGrace: 20 * time.Millisecond}), registry, t.TempDir(), nil, model.Inventory{}, Config{PollInterval: time.Millisecond, StartupGrace: time.Millisecond, StaleAfter: time.Millisecond})
	plan := testPlan(t, model.MinerModeStress)
	manager.SetInventory(model.Inventory{Host: model.Host{HostID: plan.HostID}, CPU: model.CPU{Threads: 4}})
	if _, _, err := manager.Start(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	defer manager.Stop(plan.ExecutionID)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		items := manager.List()
		if len(items) == 1 && items[0].Telemetry != nil && items[0].Telemetry.HashrateShortHPS != nil && items[0].Telemetry.Health == model.MinerHealthHealthy {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("telemetry did not recover")
}
