package minerruntime

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/le0xdon/le0xfarm/internal/farmerr"
	"github.com/le0xdon/le0xfarm/internal/identity"
	"github.com/le0xdon/le0xfarm/internal/model"
	"github.com/le0xdon/le0xfarm/internal/runtime/supervisor"
)

type fakeAdapter struct {
	source  TelemetrySource
	mu      sync.Mutex
	cleaned bool
}

func (a *fakeAdapter) ID() string { return "test-no-http" }
func (a *fakeAdapter) Capabilities() Capabilities {
	return Capabilities{CPU: true, StdoutTelemetry: true}
}
func (a *fakeAdapter) Validate(spec model.MinerSpec, _ model.Inventory) ([]string, error) {
	if spec.AdapterID != a.ID() {
		return nil, errors.New("wrong adapter")
	}
	return []string{"test adapter warning"}, nil
}
func (a *fakeAdapter) Prepare(_ context.Context, request PrepareRequest) (Prepared, error) {
	resolved := request.Plan
	resolved.Executable = "/bin/sleep"
	resolved.Args = []string{"60"}
	return Prepared{Plan: resolved, Telemetry: a.source, Cleanup: func() error {
		a.mu.Lock()
		defer a.mu.Unlock()
		a.cleaned = true
		return nil
	}}, nil
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
	return model.ExecutionPlan{ExecutionID: executionID, RestartPolicy: model.RestartNever, Miner: &model.MinerSpec{AdapterID: "test-no-http", SpecVersion: 1, PackageID: packageID, PackageVersion: "1", Mode: mode}}
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

func TestTelemetryRecoversAfterTemporaryFailure(t *testing.T) {
	hash := 10.0
	source := &sequenceSource{results: []sourceResult{{err: errors.New("temporary")}, {telemetry: &model.MinerTelemetry{AdapterID: "test-no-http", HashrateShortHPS: &hash}}}}
	adapter := &fakeAdapter{source: source}
	registry := NewRegistry()
	_ = registry.Register(adapter)
	manager := New(supervisor.New(supervisor.Config{StopGrace: 20 * time.Millisecond}), registry, t.TempDir(), nil, model.Inventory{}, Config{PollInterval: time.Millisecond, StartupGrace: time.Millisecond, StaleAfter: time.Millisecond})
	plan := testPlan(t, model.MinerModeStress)
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
