package reconcile

import (
	"fmt"
	"testing"

	"github.com/le0xdon/le0xfarm/internal/controllerstate"
	"github.com/le0xdon/le0xfarm/internal/farmmodel"
	"github.com/le0xdon/le0xfarm/internal/identity"
	"github.com/le0xdon/le0xfarm/internal/model"
)

func TestPlannerCoreConvergenceRules(t *testing.T) {
	host := testHost(1)
	workloadID := testWorkload(1)
	current := testSnapshot(workloadID, host, 3, testExecution(3), farmmodel.ResourceClaim{CPU: true})
	old := testSnapshot(workloadID, host, 2, testExecution(2), farmmodel.ResourceClaim{CPU: true})
	running := testWorkloadObject(workloadID, host, farmmodel.DesiredRunning, 3, farmmodel.ResourceClaim{CPU: true})
	stopped := running
	stopped.RunState = farmmodel.DesiredStopped
	observation := readyObservation(host)
	cases := []struct {
		name       string
		input      PlanInput
		wantAction ActionKind
		wantID     identity.ExecutionID
		conflict   bool
	}{
		{"running-none-start", PlanInput{Workload: running, Current: &current, Snapshots: []farmmodel.ResolvedExecutionSnapshot{old, current}, Observed: observation}, ActionStart, current.ExecutionID, false},
		{"current-running-noop", PlanInput{Workload: running, Current: &current, Snapshots: []farmmodel.ResolvedExecutionSnapshot{current}, Observed: withExecutions(observation, observed(current, model.ExecutionRunning))}, "", identity.ExecutionID{}, false},
		{"current-starting-noop", PlanInput{Workload: running, Current: &current, Snapshots: []farmmodel.ResolvedExecutionSnapshot{current}, Observed: withExecutions(observation, observed(current, model.ExecutionStarting))}, "", identity.ExecutionID{}, false},
		{"current-watchdog-backoff-noop", PlanInput{Workload: running, Current: &current, Snapshots: []farmmodel.ResolvedExecutionSnapshot{current}, Observed: withExecutions(observation, observed(current, model.ExecutionBackoff))}, "", identity.ExecutionID{}, false},
		{"current-degraded-noop", PlanInput{Workload: running, Current: &current, Snapshots: []farmmodel.ResolvedExecutionSnapshot{current}, Observed: withExecutions(observation, degraded(current))}, "", identity.ExecutionID{}, false},
		{"stopped-owned-stop", PlanInput{Workload: stopped, Snapshots: []farmmodel.ResolvedExecutionSnapshot{old}, Observed: withExecutions(observation, observed(old, model.ExecutionRunning))}, ActionStop, old.ExecutionID, false},
		{"stopped-none-noop", PlanInput{Workload: stopped, Snapshots: []farmmodel.ResolvedExecutionSnapshot{old}, Observed: observation}, "", identity.ExecutionID{}, false},
		{"obsolete-stop-first", PlanInput{Workload: running, Current: &current, Snapshots: []farmmodel.ResolvedExecutionSnapshot{old, current}, Observed: withExecutions(observation, observed(old, model.ExecutionRunning))}, ActionStop, old.ExecutionID, false},
		{"obsolete-and-current-stop-old", PlanInput{Workload: running, Current: &current, Snapshots: []farmmodel.ResolvedExecutionSnapshot{old, current}, Observed: withExecutions(observation, observed(current, model.ExecutionRunning), observed(old, model.ExecutionRunning))}, ActionStop, old.ExecutionID, false},
		{"terminal-failed-block", PlanInput{Workload: running, Current: &current, Snapshots: []farmmodel.ResolvedExecutionSnapshot{current}, Observed: withExecutions(observation, observed(current, model.ExecutionFailed))}, ActionBlock, current.ExecutionID, false},
		{"offline-noop", PlanInput{Workload: running, Current: &current, Snapshots: []farmmodel.ResolvedExecutionSnapshot{current}, Observed: controllerstate.HostObservation{HostID: host}}, "", identity.ExecutionID{}, false},
		{"not-ready-noop", PlanInput{Workload: running, Current: &current, Snapshots: []farmmodel.ResolvedExecutionSnapshot{current}, Observed: controllerstate.HostObservation{HostID: host, Connected: true, Fresh: true}}, "", identity.ExecutionID{}, false},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			got := Plan(test.input)
			if (got.Conflict != "") != test.conflict {
				t.Fatalf("conflict=%q", got.Conflict)
			}
			if test.wantAction == "" {
				if len(got.Actions) != 0 {
					t.Fatalf("unexpected actions: %+v", got.Actions)
				}
				return
			}
			if len(got.Actions) != 1 || got.Actions[0].Key.Action != test.wantAction || got.Actions[0].Key.ExecutionID != test.wantID {
				t.Fatalf("actions=%+v", got.Actions)
			}
		})
	}
}

func TestPlannerInFlightIsIdempotentAcrossOneHundredTicks(t *testing.T) {
	host := testHost(1)
	workloadID := testWorkload(1)
	snapshot := testSnapshot(workloadID, host, 1, testExecution(1), farmmodel.ResourceClaim{CPU: true})
	workload := testWorkloadObject(workloadID, host, farmmodel.DesiredRunning, 1, snapshot.Resources)
	input := PlanInput{Workload: workload, Current: &snapshot, Snapshots: []farmmodel.ResolvedExecutionSnapshot{snapshot}, Observed: readyObservation(host), InFlight: map[ActionKey]struct{}{}}
	first := Plan(input)
	if len(first.Actions) != 1 || first.Actions[0].Key.Action != ActionStart {
		t.Fatalf("first=%+v", first)
	}
	input.InFlight[first.Actions[0].Key] = struct{}{}
	for tick := 0; tick < 100; tick++ {
		if got := Plan(input); len(got.Actions) != 0 {
			t.Fatalf("tick %d duplicated START: %+v", tick, got.Actions)
		}
	}
	input.Observed = withExecutions(input.Observed, observed(snapshot, model.ExecutionRunning))
	input.InFlight = map[ActionKey]struct{}{}
	for tick := 0; tick < 100; tick++ {
		if got := Plan(input); len(got.Actions) != 0 {
			t.Fatalf("running tick %d emitted action: %+v", tick, got.Actions)
		}
	}
}

func TestPlannerUnmanagedAndResourceSafety(t *testing.T) {
	host := testHost(1)
	workloadID := testWorkload(1)
	cpu := farmmodel.ResourceClaim{CPU: true}
	current := testSnapshot(workloadID, host, 1, testExecution(1), cpu)
	workload := testWorkloadObject(workloadID, host, farmmodel.DesiredRunning, 1, cpu)
	base := PlanInput{Workload: workload, Current: &current, Snapshots: []farmmodel.ResolvedExecutionSnapshot{current}, Observed: readyObservation(host)}
	unknown := model.ExecutionObservation{ExecutionID: testExecution(9), Status: model.ExecutionRunning}
	if got := Plan(withObserved(base, unknown)); got.Conflict == "" || len(got.Actions) != 0 {
		t.Fatalf("unknown unmanaged claim did not block START: %+v", got)
	}
	unmanagedCPU := unmanagedObservation(host, testWorkload(9), testExecution(9), farmmodel.ResourceClaim{CPU: true})
	if got := Plan(withObserved(base, unmanagedCPU)); got.Conflict == "" || len(got.Actions) != 0 {
		t.Fatalf("overlapping unmanaged CPU did not block: %+v", got)
	}
	device := testDevice(1)
	unrelatedGPU := unmanagedObservation(host, testWorkload(9), testExecution(9), farmmodel.ResourceClaim{DeviceIDs: []identity.DeviceID{device}})
	if got := Plan(withObserved(base, unrelatedGPU)); len(got.Actions) != 1 || got.Actions[0].Key.Action != ActionStart {
		t.Fatalf("unrelated unmanaged GPU blocked CPU: %+v", got)
	}
	gpuCurrent := testSnapshot(workloadID, host, 1, testExecution(1), farmmodel.ResourceClaim{DeviceIDs: []identity.DeviceID{device}})
	gpuWorkload := testWorkloadObject(workloadID, host, farmmodel.DesiredRunning, 1, gpuCurrent.Resources)
	gpuBase := PlanInput{Workload: gpuWorkload, Current: &gpuCurrent, Snapshots: []farmmodel.ResolvedExecutionSnapshot{gpuCurrent}, Observed: readyObservation(host)}
	if got := Plan(gpuBase); got.Conflict == "" {
		t.Fatal("missing inventory DeviceID allowed START")
	}
	gpuBase.Observed.Inventory.GPUs = []model.GPU{{DeviceID: device}}
	if got := Plan(gpuBase); len(got.Actions) != 1 || got.Actions[0].Key.Action != ActionStart {
		t.Fatalf("inventory-proven GPU did not start: %+v", got)
	}
	otherWorkload := testWorkload(2)
	otherGPU := testSnapshot(otherWorkload, host, 1, testExecution(2), farmmodel.ResourceClaim{DeviceIDs: []identity.DeviceID{device}})
	base.OtherOwned = map[identity.ExecutionID]farmmodel.ResolvedExecutionSnapshot{otherGPU.ExecutionID: otherGPU}
	base.Observed = withExecutions(base.Observed, observed(otherGPU, model.ExecutionRunning))
	if got := Plan(base); len(got.Actions) != 1 || got.Actions[0].Key.Action != ActionStart {
		t.Fatalf("independent managed GPU blocked CPU: %+v", got)
	}
}

func TestPlannerGenerationAndBlockedSafety(t *testing.T) {
	host := testHost(1)
	workloadID := testWorkload(1)
	a := testSnapshot(workloadID, host, 1, testExecution(1), farmmodel.ResourceClaim{CPU: true})
	b := testSnapshot(workloadID, host, 2, testExecution(2), farmmodel.ResourceClaim{CPU: true})
	c := testSnapshot(workloadID, host, 3, testExecution(3), farmmodel.ResourceClaim{CPU: true})
	workload := testWorkloadObject(workloadID, host, farmmodel.DesiredRunning, 3, c.Resources)
	input := PlanInput{Workload: workload, Current: &c, Snapshots: []farmmodel.ResolvedExecutionSnapshot{a, b, c}, Observed: readyObservation(host)}
	if got := Plan(input); len(got.Actions) != 1 || got.Actions[0].Key.ExecutionID != c.ExecutionID {
		t.Fatalf("offline A-B-C did not converge directly to C: %+v", got)
	}
	input.Observed = withExecutions(input.Observed, observed(a, model.ExecutionRunning))
	if got := Plan(input); len(got.Actions) != 1 || got.Actions[0].Key.Action != ActionStop || got.Actions[0].Key.ExecutionID != a.ExecutionID {
		t.Fatalf("obsolete A not stopped first: %+v", got)
	}
	input.Observed = readyObservation(host)
	input.Binding = &farmmodel.WorkloadRuntimeBinding{WorkloadID: workloadID, BlockedGeneration: 3}
	for i := 0; i < 100; i++ {
		if got := Plan(input); len(got.Actions) != 0 {
			t.Fatalf("blocked generation retried at tick %d", i)
		}
	}
}

func withObserved(input PlanInput, executions ...model.ExecutionObservation) PlanInput {
	input.Observed = withExecutions(input.Observed, executions...)
	return input
}

func readyObservation(host identity.HostID) controllerstate.HostObservation {
	return controllerstate.HostObservation{HostID: host, AgentID: testAgent(1), ConnectionEpoch: 7, Connected: true, Ready: true, Fresh: true, Inventory: model.Inventory{Host: model.Host{HostID: host}}}
}

func withExecutions(observation controllerstate.HostObservation, executions ...model.ExecutionObservation) controllerstate.HostObservation {
	observation.Executions = executions
	return observation
}

func observed(snapshot farmmodel.ResolvedExecutionSnapshot, state model.ExecutionStatus) model.ExecutionObservation {
	owner := snapshot.Plan.Ownership
	owner.DeviceIDs = append([]identity.DeviceID(nil), owner.DeviceIDs...)
	return model.ExecutionObservation{ExecutionID: snapshot.ExecutionID, Ownership: &owner, Status: state}
}

func degraded(snapshot farmmodel.ResolvedExecutionSnapshot) model.ExecutionObservation {
	value := observed(snapshot, model.ExecutionRunning)
	value.MinerTelemetry = &model.MinerTelemetry{Health: model.MinerHealthDegraded}
	return value
}

func unmanagedObservation(host identity.HostID, workloadID identity.WorkloadID, executionID identity.ExecutionID, claim farmmodel.ResourceClaim) model.ExecutionObservation {
	owner := &model.WorkloadOwnership{WorkloadID: workloadID, DesiredGeneration: 1, ResolvedHash: testHash(9), HostID: host, CPU: claim.CPU, DeviceIDs: append([]identity.DeviceID(nil), claim.DeviceIDs...)}
	return model.ExecutionObservation{ExecutionID: executionID, Ownership: owner, Status: model.ExecutionRunning}
}

func testSnapshot(workloadID identity.WorkloadID, host identity.HostID, generation uint64, executionID identity.ExecutionID, claim farmmodel.ResourceClaim) farmmodel.ResolvedExecutionSnapshot {
	hash := testHash(generation)
	ownership := model.WorkloadOwnership{WorkloadID: workloadID, DesiredGeneration: generation, ResolvedHash: hash, HostID: host, CPU: claim.CPU, DeviceIDs: append([]identity.DeviceID(nil), claim.DeviceIDs...)}
	return farmmodel.ResolvedExecutionSnapshot{WorkloadID: workloadID, DesiredGeneration: generation, ExecutionID: executionID, HostID: host, Resources: claim, ResolvedHash: hash, Plan: model.ExecutionPlan{ExecutionID: executionID, Ownership: ownership, HostID: host, DeviceIDs: append([]identity.DeviceID(nil), claim.DeviceIDs...)}}
}

func testWorkloadObject(id identity.WorkloadID, host identity.HostID, state farmmodel.DesiredRunState, generation uint64, claim farmmodel.ResourceClaim) farmmodel.DesiredWorkload {
	return farmmodel.DesiredWorkload{WorkloadID: id, DesiredGeneration: generation, DesiredWorkloadContent: farmmodel.DesiredWorkloadContent{HostID: host, RunState: state, Resources: claim}}
}

func testHash(value uint64) string { return fmt.Sprintf("sha256:%064x", value) }
func testHost(value uint64) identity.HostID {
	id, _ := identity.ParseHostID(fmt.Sprintf("host_%032x", value))
	return id
}
func testAgent(value uint64) identity.AgentID {
	id, _ := identity.ParseAgentID(fmt.Sprintf("agent_%032x", value))
	return id
}
func testWorkload(value uint64) identity.WorkloadID {
	id, _ := identity.ParseWorkloadID(fmt.Sprintf("workload_%032x", value))
	return id
}
func testExecution(value uint64) identity.ExecutionID {
	id, _ := identity.ParseExecutionID(fmt.Sprintf("execution_%032x", value))
	return id
}
func testDevice(value uint64) identity.DeviceID {
	id, _ := identity.ParseDeviceID(fmt.Sprintf("device_%032x", value))
	return id
}
