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
