package incidents

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/le0xdon/le0xfarm/internal/controllerstate"
	"github.com/le0xdon/le0xfarm/internal/farmerr"
	"github.com/le0xdon/le0xfarm/internal/farmmodel"
	"github.com/le0xdon/le0xfarm/internal/identity"
	"github.com/le0xdon/le0xfarm/internal/model"
)

func TestUsefulWorkLifecycleAndStoppedHistory(t *testing.T) {
	input := healthyInput(t)
	if got := Evaluate(input).Conditions; len(got) != 0 {
		t.Fatalf("fresh confirmed work created incidents: %+v", got)
	}
	input.Observed.Executions[0].UsefulWork.Availability = model.TelemetryStale
	input.Observed.Executions[0].UsefulWork.ReasonCode = farmerr.TELEMETRY_STALE
	got := Evaluate(input).Conditions
	requireIncident(t, got, farmmodel.IncidentUsefulWorkDegraded)
	rejectIncident(t, got, farmmodel.IncidentDesiredRunningNotExecuting)
	input.Observed.Executions[0].UsefulWork.Availability = model.TelemetryUnavailable
	input.Observed.Executions[0].UsefulWork.UsefulWork = model.UsefulWorkUnknown
	got = Evaluate(input).Conditions
	requireIncident(t, got, farmmodel.IncidentTelemetryUnavailable)
	input.Workloads[0].Workload.RunState = farmmodel.DesiredStopped
	input.Observed.Executions[0].Status = model.ExecutionStopped
	if got = Evaluate(input).Conditions; len(got) != 0 {
		t.Fatalf("stopped historical evidence created current incident: %+v", got)
	}
}

func TestUnknownUnavailableStaleAndExplicitZeroAreDistinct(t *testing.T) {
	input := healthyInput(t)
	evidence := input.Observed.Executions[0].UsefulWork
	evidence.UsefulWork = model.UsefulWorkNotConfirmed
	evidence.ReasonCode = farmerr.ZERO_HASHRATE
	zero := requireIncident(t, Evaluate(input).Conditions, farmmodel.IncidentUsefulWorkDegraded)
	if zero.ReasonCode != farmerr.ZERO_HASHRATE {
		t.Fatalf("explicit zero reason lost: %+v", zero)
	}
	evidence.Availability = model.TelemetryUnavailable
	evidence.UsefulWork = model.UsefulWorkUnknown
	unavailable := requireIncident(t, Evaluate(input).Conditions, farmmodel.IncidentTelemetryUnavailable)
	if unavailable.ReasonCode != farmerr.TELEMETRY_UNAVAILABLE {
		t.Fatalf("unavailable became explicit zero: %+v", unavailable)
	}
	evidence.Availability = model.TelemetryUnknown
	unknown := requireIncident(t, Evaluate(input).Conditions, farmmodel.IncidentTelemetryUnknown)
	if unknown.ReasonCode != farmerr.TELEMETRY_UNKNOWN || unknown.IncidentID == unavailable.IncidentID || unknown.IncidentID == zero.IncidentID {
		t.Fatalf("unknown evidence was collapsed: unknown=%+v unavailable=%+v zero=%+v", unknown, unavailable, zero)
	}
	evidence.Availability = model.TelemetryStale
	evidence.ReasonCode = farmerr.TELEMETRY_STALE
	stale := requireIncident(t, Evaluate(input).Conditions, farmmodel.IncidentUsefulWorkDegraded)
	if stale.ReasonCode != farmerr.TELEMETRY_STALE || stale.IncidentID == unknown.IncidentID || stale.IncidentID == unavailable.IncidentID || stale.IncidentID == zero.IncidentID {
		t.Fatalf("stale evidence was collapsed: stale=%+v unknown=%+v unavailable=%+v zero=%+v", stale, unknown, unavailable, zero)
	}
	input.Observed.Executions[0].UsefulWork = nil
	absent := requireIncident(t, Evaluate(input).Conditions, farmmodel.IncidentTelemetryUnknown)
	if absent.ReasonCode != farmerr.TELEMETRY_UNKNOWN || absent.IncidentID != unknown.IncidentID {
		t.Fatalf("absent evidence did not map to generic unknown: absent=%+v unknown=%+v", absent, unknown)
	}
}

func TestMonitoringLossDoesNotAuthorizeResolution(t *testing.T) {
	input := healthyInput(t)
	input.Observed.Executions[0].UsefulWork.Availability = model.TelemetryStale
	active := Evaluate(input)
	if !active.ResolvableTypes[farmmodel.IncidentUsefulWorkDegraded] {
		t.Fatal("fresh evaluation could not resolve useful-work family")
	}
	input.Observed.Connected = false
	lost := Evaluate(input)
	requireIncident(t, lost.Conditions, farmmodel.IncidentAgentOffline)
	if lost.ResolvableTypes[farmmodel.IncidentUsefulWorkDegraded] || lost.ResolvableTypes[farmmodel.IncidentAgentOffline] {
		t.Fatal("lost authority was allowed to resolve operational incidents")
	}
	input.Observed.Connected = true
	input.Observed.Ready = false
	reconnecting := Evaluate(input)
	requireIncident(t, reconnecting.Conditions, farmmodel.IncidentMonitoringStale)
	if reconnecting.ResolvableTypes[farmmodel.IncidentAgentOffline] {
		t.Fatal("reconnect without bootstrap could resolve offline incident")
	}
}

func TestKnownDisconnectedHostCreatesOfflineWithoutDesiredWorkload(t *testing.T) {
	host := mustHost(7)
	evaluation := Evaluate(Input{
		HostID:      host,
		HasObserved: true,
		Observed: controllerstate.HostObservation{
			HostID:          host,
			ConnectionEpoch: 3,
			Connected:       false,
		},
	})
	requireIncident(t, evaluation.Conditions, farmmodel.IncidentAgentOffline)
	if evaluation.ResolvableTypes[farmmodel.IncidentAgentOffline] {
		t.Fatal("disconnected authority could resolve its own offline incident")
	}
}

func TestMaintenanceSuppressesIntentionalStopButNotOffline(t *testing.T) {
	input := healthyInput(t)
	input.Hold = &farmmodel.MaintenanceHold{HostID: input.HostID, Active: true}
	input.Observed.Executions[0].Status = model.ExecutionStopped
	input.Observed.Executions[0].PID = 0
	got := Evaluate(input).Conditions
	requireIncident(t, got, farmmodel.IncidentMaintenanceHold)
	rejectIncident(t, got, farmmodel.IncidentDesiredRunningNotExecuting)
	input.Observed.Connected = false
	got = Evaluate(input).Conditions
	requireIncident(t, got, farmmodel.IncidentMaintenanceHold)
	requireIncident(t, got, farmmodel.IncidentAgentOffline)
}

func TestMaintenanceEvaluatesIndependentConflictsAndTruthfulRecovery(t *testing.T) {
	input := healthyInput(t)
	device := mustDevice(1)
	input.Hold = &farmmodel.MaintenanceHold{HostID: input.HostID, Active: true}
	input.Workloads[0].Workload.Resources = farmmodel.ResourceClaim{DeviceIDs: []identity.DeviceID{device}}
	input.Workloads[0].Current.Resources = input.Workloads[0].Workload.Resources
	input.Observed.Inventory.GPUs = []model.GPU{{DeviceID: device}}
	input.Observed.Executions[0].Status = model.ExecutionStopped
	input.Observed.Executions[0].PID = 0
	input.Observed.Executions[0].UsefulWork = nil
	input.Observed.UnmanagedProcesses = []model.UnmanagedProcessObservation{{PID: 99, ProcessInstance: "ticks:1", GPURelevant: true, ResourceScope: model.ProcessResourcesExact, DeviceIDs: []identity.DeviceID{device}}}

	active := Evaluate(input)
	requireIncident(t, active.Conditions, farmmodel.IncidentMaintenanceHold)
	requireIncident(t, active.Conditions, farmmodel.IncidentUnmanagedProcessConflict)
	rejectIncident(t, active.Conditions, farmmodel.IncidentDesiredRunningNotExecuting)
	rejectIncident(t, active.Conditions, farmmodel.IncidentTelemetryUnknown)

	input.Observed.UnmanagedProcesses = nil
	recovered := Evaluate(input)
	rejectIncident(t, recovered.Conditions, farmmodel.IncidentUnmanagedProcessConflict)
	if !recovered.ResolvableTypes[farmmodel.IncidentUnmanagedProcessConflict] {
		t.Fatal("fresh conflict removal during Hold could not resolve unmanaged incident")
	}

	input.Observed.Inventory.GPUs = nil
	requireIncident(t, Evaluate(input).Conditions, farmmodel.IncidentIncompatibleHardware)
	input.Observed.Inventory.GPUs = []model.GPU{{DeviceID: device}}
	input.Workloads[0].Binding = &farmmodel.WorkloadRuntimeBinding{WorkloadID: input.Workloads[0].Workload.WorkloadID, BlockedGeneration: 1, ErrorCode: farmerr.PACKAGE_NOT_INSTALLED}
	requireIncident(t, Evaluate(input).Conditions, farmmodel.IncidentConfigurationBlocked)

	input.Hold.Active = false
	input.Observed.Ready = false
	staleExit := Evaluate(input)
	if staleExit.ResolvableTypes[farmmodel.IncidentConfigurationBlocked] || staleExit.ResolvableTypes[farmmodel.IncidentUnmanagedProcessConflict] {
		t.Fatal("stale Hold exit authorized false recovery")
	}
}

func TestUnmanagedAndStableGPUResourceIncidents(t *testing.T) {
	input := healthyInput(t)
	device, secondDevice := mustDevice(1), mustDevice(2)
	input.Workloads[0].Workload.Resources = farmmodel.ResourceClaim{DeviceIDs: []identity.DeviceID{device, secondDevice}}
	input.Workloads[0].Current.Resources = input.Workloads[0].Workload.Resources
	input.Observed.Executions = nil
	missing := Evaluate(input).Conditions
	if countIncident(missing, farmmodel.IncidentIncompatibleHardware) != 2 {
		t.Fatalf("missing GPUs collapsed: %+v", missing)
	}
	condition := requireIncident(t, missing, farmmodel.IncidentIncompatibleHardware)
	if condition.DeviceID == nil || *condition.DeviceID != device {
		t.Fatalf("hardware incident lacks stable DeviceID: %+v", condition)
	}
	input.Observed.Inventory.GPUs = []model.GPU{{DeviceID: device}, {DeviceID: secondDevice}}
	input.Observed.UnmanagedProcesses = []model.UnmanagedProcessObservation{{PID: 99, ProcessInstance: "ticks:1", GPURelevant: true, ResourceScope: model.ProcessResourcesExact, DeviceIDs: []identity.DeviceID{device}}}
	condition = requireIncident(t, Evaluate(input).Conditions, farmmodel.IncidentUnmanagedProcessConflict)
	if condition.DeviceID == nil || *condition.DeviceID != device || contains(condition.Key, "99") || contains(condition.Key, "ticks") {
		t.Fatalf("unmanaged incident identity used transient process identity: %+v", condition)
	}
}

func TestConditionIDIsDeterministicAndScoped(t *testing.T) {
	host := mustHost(1)
	workload := mustWorkload(1)
	a := NewCondition(farmmodel.IncidentRuntimeError, farmmodel.IncidentSeverityError, host, &workload, nil, nil, farmerr.PROCESS_CRASHED)
	b := NewCondition(farmmodel.IncidentRuntimeError, farmmodel.IncidentSeverityError, host, &workload, nil, nil, farmerr.PROCESS_CRASHED)
	other := mustWorkload(2)
	c := NewCondition(farmmodel.IncidentRuntimeError, farmmodel.IncidentSeverityError, host, &other, nil, nil, farmerr.PROCESS_CRASHED)
	if a.IncidentID != b.IncidentID || a.Key != b.Key || a.IncidentID == c.IncidentID {
		t.Fatalf("deterministic/scoped identity failed: a=%+v b=%+v c=%+v", a, b, c)
	}
}

func TestRuntimeConfigurationAndManagedResourceConditions(t *testing.T) {
	input := healthyInput(t)
	input.Observed.Executions[0].Status = model.ExecutionFailed
	requireIncident(t, Evaluate(input).Conditions, farmmodel.IncidentRuntimeError)

	input = healthyInput(t)
	input.Workloads[0].Binding = &farmmodel.WorkloadRuntimeBinding{WorkloadID: input.Workloads[0].Workload.WorkloadID, BlockedGeneration: 1, ErrorCode: farmerr.PACKAGE_NOT_INSTALLED}
	requireIncident(t, Evaluate(input).Conditions, farmmodel.IncidentConfigurationBlocked)

	input = healthyInput(t)
	otherWorkload, otherExecution := mustWorkload(2), mustExecution(2)
	hash := fmt.Sprintf("sha256:%064x", 2)
	ownership := model.WorkloadOwnership{WorkloadID: otherWorkload, DesiredGeneration: 1, ResolvedHash: hash, HostID: input.HostID, CPU: true}
	other := farmmodel.ResolvedExecutionSnapshot{WorkloadID: otherWorkload, DesiredGeneration: 1, ExecutionID: otherExecution, HostID: input.HostID, Resources: farmmodel.ResourceClaim{CPU: true}, ResolvedHash: hash}
	input.AllSnapshots = append(input.AllSnapshots, other)
	input.Observed.Executions = append(input.Observed.Executions, model.ExecutionObservation{ExecutionID: otherExecution, Ownership: &ownership, Status: model.ExecutionRunning, PID: 456, StartedAt: time.Now().UTC()})
	requireIncident(t, Evaluate(input).Conditions, farmmodel.IncidentResourceConflict)

	input = healthyInput(t)
	input.Observed.Executions = nil
	requireIncident(t, Evaluate(input).Conditions, farmmodel.IncidentDesiredRunningNotExecuting)
}

func healthyInput(t *testing.T) Input {
	t.Helper()
	host := mustHost(1)
	workloadID := mustWorkload(1)
	executionID := mustExecution(1)
	hash := fmt.Sprintf("sha256:%064x", 1)
	claim := farmmodel.ResourceClaim{CPU: true}
	ownership := model.WorkloadOwnership{WorkloadID: workloadID, DesiredGeneration: 1, ResolvedHash: hash, HostID: host, CPU: true}
	snapshot := farmmodel.ResolvedExecutionSnapshot{WorkloadID: workloadID, DesiredGeneration: 1, ExecutionID: executionID, HostID: host, Resources: claim, ResolvedHash: hash, Plan: model.ExecutionPlan{ExecutionID: executionID, Ownership: ownership}}
	useful := &model.UsefulWorkEvidence{Provider: "generic", Availability: model.TelemetryAvailable, UsefulWork: model.UsefulWorkConfirmed, CollectedAt: time.Now().UTC(), FreshFor: time.Minute}
	return Input{HostID: host, Workloads: []WorkloadFacts{{Workload: farmmodel.DesiredWorkload{WorkloadID: workloadID, DesiredGeneration: 1, DesiredWorkloadContent: farmmodel.DesiredWorkloadContent{HostID: host, RunState: farmmodel.DesiredRunning, Resources: claim}}, Current: &snapshot}}, AllSnapshots: []farmmodel.ResolvedExecutionSnapshot{snapshot}, HasObserved: true, Observed: controllerstate.HostObservation{HostID: host, ConnectionEpoch: 1, Connected: true, Ready: true, Fresh: true, InventoryFresh: true, ProcessesFresh: true, Executions: []model.ExecutionObservation{{ExecutionID: executionID, Ownership: &ownership, Status: model.ExecutionRunning, PID: 123, StartedAt: time.Now().UTC(), UsefulWork: useful}}}}
}

func requireIncident(t *testing.T, values []farmmodel.IncidentCondition, kind farmmodel.IncidentType) farmmodel.IncidentCondition {
	t.Helper()
	for _, value := range values {
		if value.Type == kind {
			return value
		}
	}
	t.Fatalf("missing incident %s in %+v", kind, values)
	return farmmodel.IncidentCondition{}
}

func rejectIncident(t *testing.T, values []farmmodel.IncidentCondition, kind farmmodel.IncidentType) {
	t.Helper()
	for _, value := range values {
		if value.Type == kind {
			t.Fatalf("unexpected incident %s: %+v", kind, value)
		}
	}
}

func countIncident(values []farmmodel.IncidentCondition, kind farmmodel.IncidentType) int {
	count := 0
	for _, value := range values {
		if value.Type == kind {
			count++
		}
	}
	return count
}

func mustHost(n int) identity.HostID {
	id, _ := identity.ParseHostID(fmt.Sprintf("host_%032x", n))
	return id
}
func mustWorkload(n int) identity.WorkloadID {
	id, _ := identity.ParseWorkloadID(fmt.Sprintf("workload_%032x", n))
	return id
}
func mustExecution(n int) identity.ExecutionID {
	id, _ := identity.ParseExecutionID(fmt.Sprintf("execution_%032x", n))
	return id
}
func mustDevice(n int) identity.DeviceID {
	id, _ := identity.ParseDeviceID(fmt.Sprintf("device_%032x", n))
	return id
}
func contains(value, fragment string) bool { return strings.Contains(value, fragment) }
