package controllerstate

import (
	"testing"
	"time"

	"github.com/le0xdon/le0xfarm/internal/identity"
	"github.com/le0xdon/le0xfarm/internal/model"
)

func TestObservationFreshReadyDisconnectAndEpochSafety(t *testing.T) {
	store := New()
	agentID, _ := identity.ParseAgentID("agent_0123456789abcdef0123456789abcdef")
	hostID, _ := identity.ParseHostID("host_0123456789abcdef0123456789abcdef")
	executionID, _ := identity.ParseExecutionID("execution_0123456789abcdef0123456789abcdef")
	if !store.Connect(agentID, hostID, 1) {
		t.Fatal("initial connection rejected")
	}
	if got, _ := store.Get(hostID); !got.Connected || got.Ready || got.Fresh {
		t.Fatalf("new session trusted too early: %+v", got)
	}
	if !store.SetExecutions(hostID, 1, []model.ExecutionObservation{{ExecutionID: executionID, Status: model.ExecutionRunning}}, time.Now(), 0) {
		t.Fatal("fresh executions rejected")
	}
	if got, _ := store.Get(hostID); !got.Fresh || got.Ready {
		t.Fatalf("executions alone made session ready: %+v", got)
	}
	if !store.SetInventory(hostID, 1, model.Inventory{Host: model.Host{HostID: hostID}}) || !store.SetAgentState(hostID, 1, model.AgentStateMining) {
		t.Fatal("bootstrap observation rejected")
	}
	if !store.SetUnmanagedProcesses(hostID, 1, nil, time.Now()) {
		t.Fatal("fresh process observation rejected")
	}
	if !store.MarkReady(hostID, 1) {
		t.Fatal("complete bootstrap did not allow READY")
	}
	got, _ := store.Get(hostID)
	if !got.Ready || !got.Fresh || !got.InventoryFresh || got.ConnectionEpoch != 1 || len(got.Executions) != 1 {
		t.Fatalf("session did not become ready: %+v", got)
	}
	if !store.Connect(agentID, hostID, 2) {
		t.Fatal("reconnect rejected")
	}
	got, _ = store.Get(hostID)
	if !got.Connected || got.Ready || got.Fresh || got.ConnectionEpoch != 2 || len(got.Executions) != 0 {
		t.Fatalf("reconnect retained stale facts: %+v", got)
	}
	if store.SetExecutions(hostID, 1, []model.ExecutionObservation{{ExecutionID: executionID, Status: model.ExecutionRunning}}, time.Now(), 0) || store.Disconnect(hostID, 1) {
		t.Fatal("stale epoch mutated current observation")
	}
	got, _ = store.Get(hostID)
	if got.ConnectionEpoch != 2 || !got.Connected || got.Fresh {
		t.Fatalf("stale epoch affected current session: %+v", got)
	}
	if !store.Disconnect(hostID, 2) {
		t.Fatal("current disconnect rejected")
	}
	got, _ = store.Get(hostID)
	if got.Connected || got.Ready || got.Fresh {
		t.Fatalf("disconnect did not invalidate observation: %+v", got)
	}
}

func TestInventoryFreshnessExpiresIndependentlyAndRefreshes(t *testing.T) {
	now := time.Unix(3_000, 0).UTC()
	store := NewWithOptions(StoreOptions{Now: func() time.Time { return now }, FreshnessTimeout: 45 * time.Second})
	hostID, _ := identity.NewHostID()
	agentID, _ := identity.NewAgentID()
	store.Connect(agentID, hostID, 1)
	store.SetExecutions(hostID, 1, nil, now, 0)
	store.SetInventory(hostID, 1, model.Inventory{Host: model.Host{HostID: hostID}, CPU: model.CPU{Threads: 16}})
	store.SetUnmanagedProcesses(hostID, 1, nil, now)
	store.SetAgentState(hostID, 1, model.AgentStateIdle)
	if !store.MarkReady(hostID, 1) {
		t.Fatal("bootstrap did not become READY")
	}
	now = now.Add(40 * time.Second)
	store.SetExecutions(hostID, 1, nil, now, 0)
	now = now.Add(6 * time.Second)
	got, _ := store.Get(hostID)
	if !got.Fresh || got.InventoryFresh || !got.Ready {
		t.Fatalf("inventory did not expire independently: %+v", got)
	}
	store.SetInventory(hostID, 1, model.Inventory{Host: model.Host{HostID: hostID}, CPU: model.CPU{Threads: 8}})
	got, _ = store.Get(hostID)
	if !got.InventoryFresh || got.Inventory.CPU.Threads != 8 {
		t.Fatalf("inventory refresh did not recover: %+v", got)
	}
}

func TestObservationReturnsDeepCopies(t *testing.T) {
	store := New()
	agentID, _ := identity.NewAgentID()
	hostID, _ := identity.NewHostID()
	deviceID, _ := identity.NewDeviceID()
	executionID, _ := identity.NewExecutionID()
	workloadID, _ := identity.NewWorkloadID()
	owner := &model.WorkloadOwnership{WorkloadID: workloadID, DesiredGeneration: 1, ResolvedHash: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", HostID: hostID, DeviceIDs: []identity.DeviceID{deviceID}}
	store.Connect(agentID, hostID, 1)
	store.SetExecutions(hostID, 1, []model.ExecutionObservation{{ExecutionID: executionID, Ownership: owner, Status: model.ExecutionRunning}}, time.Now(), 0)
	store.SetUnmanagedProcesses(hostID, 1, []model.UnmanagedProcessObservation{{PID: 1, ProcessInstance: "linux-proc-start-ticks:1", DeviceIDs: []identity.DeviceID{deviceID}, Evidence: []model.ProcessEvidence{{Kind: "KNOWN", Provider: "test"}}}}, time.Now())
	first, _ := store.Get(hostID)
	first.Executions[0].Ownership.DeviceIDs[0] = identity.DeviceID{}
	first.UnmanagedProcesses[0].DeviceIDs[0] = identity.DeviceID{}
	first.UnmanagedProcesses[0].Evidence[0].Provider = "mutated"
	second, _ := store.Get(hostID)
	if second.Executions[0].Ownership.DeviceIDs[0] != deviceID {
		t.Fatal("caller mutated stored observation")
	}
	if second.UnmanagedProcesses[0].DeviceIDs[0] != deviceID || second.UnmanagedProcesses[0].Evidence[0].Provider != "test" {
		t.Fatal("caller mutated stored process observation")
	}
}

func TestObservationExpiresWithoutCurrentEpochRefreshAndCanRecover(t *testing.T) {
	now := time.Unix(1_000, 0).UTC()
	store := NewWithOptions(StoreOptions{Now: func() time.Time { return now }, FreshnessTimeout: 45 * time.Second})
	hostID, _ := identity.NewHostID()
	agentID, _ := identity.NewAgentID()
	store.Connect(agentID, hostID, 1)
	store.SetExecutions(hostID, 1, nil, now, 0)
	store.SetInventory(hostID, 1, model.Inventory{Host: model.Host{HostID: hostID}})
	store.SetUnmanagedProcesses(hostID, 1, nil, now)
	store.SetAgentState(hostID, 1, model.AgentStateIdle)
	if !store.MarkReady(hostID, 1) {
		t.Fatal("bootstrap observation did not become ready")
	}
	now = now.Add(45 * time.Second)
	if got, _ := store.Get(hostID); !got.Fresh || !got.Ready {
		t.Fatalf("observation expired at the boundary: %+v", got)
	}
	now = now.Add(time.Nanosecond)
	if got, _ := store.Get(hostID); got.Fresh || !got.Ready || !got.Connected {
		t.Fatalf("half-open observation remained usable: %+v", got)
	}
	// A heartbeat alone is only liveness. The GET_EXECUTIONS response it
	// triggers is the fact that restores safe current-epoch freshness.
	if !store.SetExecutions(hostID, 1, nil, now, 0) {
		t.Fatal("current-epoch execution refresh was rejected")
	}
	if got, _ := store.Get(hostID); !got.Fresh || !got.Ready {
		t.Fatalf("valid current-epoch refresh did not recover freshness: %+v", got)
	}
}

func TestOlderExecutionSnapshotCannotOverwriteNewerRuntimeResult(t *testing.T) {
	store := New()
	hostID, _ := identity.NewHostID()
	agentID, _ := identity.NewAgentID()
	executionID, _ := identity.NewExecutionID()
	store.Connect(agentID, hostID, 1)
	if !store.SetExecutions(hostID, 1, nil, time.Now(), 0) {
		t.Fatal("bootstrap snapshot rejected")
	}
	if !store.UpsertExecution(hostID, 1, model.ExecutionObservation{ExecutionID: executionID, Status: model.ExecutionRunning}, time.Now(), 1) {
		t.Fatal("runtime result rejected")
	}
	if store.SetExecutions(hostID, 1, nil, time.Now(), 0) {
		t.Fatal("pre-dispatch execution snapshot overwrote newer runtime result")
	}
	got, _ := store.Get(hostID)
	if len(got.Executions) != 1 || got.Executions[0].ExecutionID != executionID {
		t.Fatalf("newer runtime fact was lost: %+v", got.Executions)
	}
}

func TestUsefulWorkEvidenceIsEpochScopedAndDeepCopied(t *testing.T) {
	store := New()
	hostID, _ := identity.NewHostID()
	agentID, _ := identity.NewAgentID()
	executionID, _ := identity.NewExecutionID()
	last := time.Unix(900, 0).UTC()
	healthy := true
	accepted := uint64(1)
	evidence := &model.UsefulWorkEvidence{Provider: "test", Availability: model.TelemetryAvailable, RuntimeHealthy: &healthy, UsefulWork: model.UsefulWorkConfirmed, AcceptedWork: &accepted, Upstream: model.UpstreamUnknown, Confidence: model.EvidenceConfidenceAdapterReported, LastUsefulWorkAt: &last, Metrics: []model.WorkMetric{{Kind: "TASKS", Unit: "TASK/S", Value: 1}}}
	store.Connect(agentID, hostID, 1)
	if !store.SetExecutions(hostID, 1, []model.ExecutionObservation{{ExecutionID: executionID, Status: model.ExecutionRunning, UsefulWork: evidence}}, time.Now(), 0) {
		t.Fatal("current telemetry rejected")
	}
	first, _ := store.Get(hostID)
	first.Executions[0].UsefulWork.Metrics[0].Kind = "MUTATED"
	*first.Executions[0].UsefulWork.LastUsefulWorkAt = time.Time{}
	*first.Executions[0].UsefulWork.AcceptedWork = 99
	*first.Executions[0].UsefulWork.RuntimeHealthy = false
	second, _ := store.Get(hostID)
	if second.Executions[0].UsefulWork.Metrics[0].Kind != "TASKS" || second.Executions[0].UsefulWork.LastUsefulWorkAt.IsZero() || *second.Executions[0].UsefulWork.AcceptedWork != 1 || !*second.Executions[0].UsefulWork.RuntimeHealthy {
		t.Fatal("caller mutated retained useful-work evidence")
	}
	if !store.Connect(agentID, hostID, 2) {
		t.Fatal("new epoch rejected")
	}
	if store.SetExecutions(hostID, 1, []model.ExecutionObservation{{ExecutionID: executionID, Status: model.ExecutionRunning, UsefulWork: evidence}}, time.Now(), 1) {
		t.Fatal("old-epoch telemetry was accepted")
	}
	current, _ := store.Get(hostID)
	if len(current.Executions) != 0 || current.Fresh {
		t.Fatalf("new epoch inherited old telemetry: %+v", current)
	}
}

func TestControllerObservationExpiresUsefulWorkBeforeHostFreshness(t *testing.T) {
	now := time.Unix(2_000, 0).UTC()
	store := NewWithOptions(StoreOptions{Now: func() time.Time { return now }, FreshnessTimeout: 45 * time.Second})
	hostID, _ := identity.NewHostID()
	agentID, _ := identity.NewAgentID()
	executionID, _ := identity.NewExecutionID()
	evidence := &model.UsefulWorkEvidence{Provider: "test", Availability: model.TelemetryAvailable, UsefulWork: model.UsefulWorkConfirmed, Upstream: model.UpstreamUnknown, Confidence: model.EvidenceConfidenceAdapterReported, CollectedAt: now, FreshFor: 10 * time.Second}
	store.Connect(agentID, hostID, 1)
	store.SetExecutions(hostID, 1, []model.ExecutionObservation{{ExecutionID: executionID, Status: model.ExecutionRunning, MinerTelemetry: &model.MinerTelemetry{CollectedAt: now, Health: model.MinerHealthMining}, UsefulWork: evidence}}, now, 0)
	store.SetAgentState(hostID, 1, model.AgentStateMining)
	if got, _ := store.Get(hostID); got.Executions[0].UsefulWork.Availability != model.TelemetryAvailable || got.AgentState != model.AgentStateMining {
		t.Fatalf("fresh evidence changed early: %+v", got)
	}
	now = now.Add(10*time.Second + time.Nanosecond)
	got, _ := store.Get(hostID)
	if !got.Fresh || got.Executions[0].UsefulWork.Availability != model.TelemetryStale || got.Executions[0].MinerTelemetry.Health != model.MinerHealthDegraded || got.AgentState != model.AgentStateDegraded {
		t.Fatalf("stale evidence remained confirmed: %+v", got)
	}
}

func TestGenericUsefulWorkExpiryWithoutMinerTelemetryDegradesHost(t *testing.T) {
	now := time.Unix(3_000, 0).UTC()
	store := NewWithOptions(StoreOptions{Now: func() time.Time { return now }, FreshnessTimeout: time.Minute})
	hostID, _ := identity.NewHostID()
	agentID, _ := identity.NewAgentID()
	executionID, _ := identity.NewExecutionID()
	evidence := &model.UsefulWorkEvidence{Provider: "future-compute", Availability: model.TelemetryAvailable, UsefulWork: model.UsefulWorkConfirmed, Upstream: model.UpstreamUnknown, Confidence: model.EvidenceConfidenceAdapterReported, CollectedAt: now, FreshFor: 5 * time.Second}
	store.Connect(agentID, hostID, 1)
	store.SetExecutions(hostID, 1, []model.ExecutionObservation{{ExecutionID: executionID, Status: model.ExecutionRunning, UsefulWork: evidence}}, now, 0)
	store.SetAgentState(hostID, 1, model.AgentStateMining)
	if fresh, _ := store.Get(hostID); fresh.AgentState != model.AgentStateMining || fresh.Executions[0].UsefulWork.Availability != model.TelemetryAvailable {
		t.Fatalf("fresh generic evidence did not retain working state: %+v", fresh)
	}
	now = now.Add(5*time.Second + time.Nanosecond)
	stale, _ := store.Get(hostID)
	if stale.AgentState != model.AgentStateDegraded || stale.Executions[0].UsefulWork.Availability != model.TelemetryStale {
		t.Fatalf("stale generic evidence left host working: %+v", stale)
	}
}

func TestUsefulWorkAgeIsDerivedWithoutDoubleAging(t *testing.T) {
	now := time.Unix(4_000, 0).UTC()
	store := NewWithOptions(StoreOptions{Now: func() time.Time { return now }, FreshnessTimeout: time.Minute})
	hostID, _ := identity.NewHostID()
	agentID, _ := identity.NewAgentID()
	executionID, _ := identity.NewExecutionID()
	evidence := &model.UsefulWorkEvidence{Provider: "future-compute", Availability: model.TelemetryAvailable, UsefulWork: model.UsefulWorkConfirmed, Upstream: model.UpstreamUnknown, Confidence: model.EvidenceConfidenceAdapterReported, CollectedAt: now.Add(-2 * time.Second), Age: 2 * time.Second, FreshFor: 30 * time.Second}
	store.Connect(agentID, hostID, 1)
	store.SetExecutions(hostID, 1, []model.ExecutionObservation{{ExecutionID: executionID, Status: model.ExecutionRunning, UsefulWork: evidence}}, now, 0)
	now = now.Add(3 * time.Second)
	first, _ := store.Get(hostID)
	second, _ := store.Get(hostID)
	if first.Executions[0].UsefulWork.Age != 5*time.Second || second.Executions[0].UsefulWork.Age != 5*time.Second {
		t.Fatalf("repeated read double-aged evidence: first=%s second=%s", first.Executions[0].UsefulWork.Age, second.Executions[0].UsefulWork.Age)
	}
	now = now.Add(time.Second)
	third, _ := store.Get(hostID)
	if third.Executions[0].UsefulWork.Age != 6*time.Second {
		t.Fatalf("evidence age did not derive from retained source sample: %s", third.Executions[0].UsefulWork.Age)
	}
}

func TestMixedUsefulWorkAggregationIsTruthful(t *testing.T) {
	now := time.Unix(5_000, 0).UTC()
	store := NewWithOptions(StoreOptions{Now: func() time.Time { return now }, FreshnessTimeout: time.Minute})
	hostID, _ := identity.NewHostID()
	agentID, _ := identity.NewAgentID()
	staleID, _ := identity.ParseExecutionID("execution_0123456789abcdef0123456789abcdef")
	freshID, _ := identity.ParseExecutionID("execution_1123456789abcdef0123456789abcdef")
	staleEvidence := &model.UsefulWorkEvidence{Provider: "old-compute", Availability: model.TelemetryAvailable, UsefulWork: model.UsefulWorkConfirmed, Upstream: model.UpstreamUnknown, Confidence: model.EvidenceConfidenceAdapterReported, CollectedAt: now, FreshFor: 5 * time.Second}
	freshEvidence := &model.UsefulWorkEvidence{Provider: "current-compute", Availability: model.TelemetryAvailable, UsefulWork: model.UsefulWorkConfirmed, Upstream: model.UpstreamUnknown, Confidence: model.EvidenceConfidenceAdapterReported, CollectedAt: now, FreshFor: 20 * time.Second}
	store.Connect(agentID, hostID, 1)
	store.SetExecutions(hostID, 1, []model.ExecutionObservation{{ExecutionID: staleID, Status: model.ExecutionRunning, UsefulWork: staleEvidence}, {ExecutionID: freshID, Status: model.ExecutionRunning, UsefulWork: freshEvidence}}, now, 0)
	store.SetAgentState(hostID, 1, model.AgentStateMining)
	now = now.Add(10 * time.Second)
	mixed, _ := store.Get(hostID)
	if mixed.AgentState != model.AgentStateDegraded || mixed.Executions[0].UsefulWork.Availability != model.TelemetryStale || mixed.Executions[1].UsefulWork.Availability != model.TelemetryAvailable {
		t.Fatalf("mixed fresh/stale aggregation was not conservative: %+v", mixed)
	}

	store.SetExecutions(hostID, 1, []model.ExecutionObservation{{ExecutionID: staleID, Status: model.ExecutionStopped, UsefulWork: staleEvidence}, {ExecutionID: freshID, Status: model.ExecutionRunning, UsefulWork: freshEvidence}}, now, 0)
	store.SetAgentState(hostID, 1, model.AgentStateMining)
	now = now.Add(10 * time.Second)
	stoppedOld, _ := store.Get(hostID)
	if stoppedOld.AgentState != model.AgentStateMining || stoppedOld.Executions[0].UsefulWork.Availability != model.TelemetryStale || stoppedOld.Executions[1].UsefulWork.Availability != model.TelemetryAvailable {
		t.Fatalf("stopped historical evidence affected active host status: %+v", stoppedOld)
	}
}
