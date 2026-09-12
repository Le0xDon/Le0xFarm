// Package incidents evaluates durable, platform-neutral Controller incidents
// from normalized Desired and Observed state.
package incidents

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"sort"
	"strings"

	"github.com/le0xdon/le0xfarm/internal/controllerstate"
	"github.com/le0xdon/le0xfarm/internal/farmerr"
	"github.com/le0xdon/le0xfarm/internal/farmmodel"
	"github.com/le0xdon/le0xfarm/internal/identity"
	"github.com/le0xdon/le0xfarm/internal/model"
)

type WorkloadFacts struct {
	Workload farmmodel.DesiredWorkload
	Current  *farmmodel.ResolvedExecutionSnapshot
	Binding  *farmmodel.WorkloadRuntimeBinding
}

type Input struct {
	HostID       identity.HostID
	Workloads    []WorkloadFacts
	AllSnapshots []farmmodel.ResolvedExecutionSnapshot
	Hold         *farmmodel.MaintenanceHold
	Observed     controllerstate.HostObservation
	HasObserved  bool
}

type Evaluation struct {
	Conditions      []farmmodel.IncidentCondition
	ResolvableTypes map[farmmodel.IncidentType]bool
}

// Evaluate does not infer recovery from missing evidence. Operational incident
// families become resolvable only when the Host has a complete, fresh current-
// epoch bootstrap. Persistent Maintenance Hold is independently authoritative.
func Evaluate(input Input) Evaluation {
	result := Evaluation{ResolvableTypes: map[farmmodel.IncidentType]bool{farmmodel.IncidentMaintenanceHold: true}}
	add := func(kind farmmodel.IncidentType, severity farmmodel.IncidentSeverity, workload *identity.WorkloadID, execution *identity.ExecutionID, device *identity.DeviceID, reason farmerr.Code) {
		result.Conditions = append(result.Conditions, NewCondition(kind, severity, input.HostID, workload, execution, device, reason))
	}

	if input.Hold != nil && input.Hold.Active {
		add(farmmodel.IncidentMaintenanceHold, farmmodel.IncidentSeverityInfo, nil, nil, nil, farmerr.MAINTENANCE_HOLD)
	}
	runningDesired := false
	for _, facts := range input.Workloads {
		runningDesired = runningDesired || facts.Workload.RunState == farmmodel.DesiredRunning
	}
	if !input.HasObserved {
		// A configured RUNNING workload with no monitoring authority is offline;
		// a never-observed Host with no work is not yet evidence of a disconnect.
		if !runningDesired {
			return finish(result)
		}
		add(farmmodel.IncidentAgentOffline, farmmodel.IncidentSeverityError, nil, nil, nil, farmerr.SERVICE_NOT_READY)
		return finish(result)
	}
	if !input.Observed.Connected || input.Observed.HostID != input.HostID || input.Observed.ConnectionEpoch == 0 {
		// A previously known Host becoming disconnected is itself actionable
		// monitoring loss, even when it currently has no RUNNING Desired work.
		// Reconnect without a complete bootstrap still cannot resolve it.
		add(farmmodel.IncidentAgentOffline, farmmodel.IncidentSeverityError, nil, nil, nil, farmerr.SERVICE_NOT_READY)
		return finish(result)
	}
	fullyFresh := input.Observed.Ready && input.Observed.Fresh && input.Observed.InventoryFresh && input.Observed.ProcessesFresh
	if !fullyFresh {
		if runningDesired {
			add(farmmodel.IncidentMonitoringStale, farmmodel.IncidentSeverityWarning, nil, nil, nil, farmerr.SERVICE_NOT_READY)
		}
		return finish(result)
	}
	for _, kind := range allTypes() {
		result.ResolvableTypes[kind] = true
	}
	if !runningDesired {
		return finish(result)
	}
	holdActive := input.Hold != nil && input.Hold.Active

	known := make(map[identity.ExecutionID]farmmodel.ResolvedExecutionSnapshot, len(input.AllSnapshots))
	for _, snapshot := range input.AllSnapshots {
		known[snapshot.ExecutionID] = snapshot
	}
	for i := range input.Workloads {
		facts := input.Workloads[i]
		if facts.Workload.RunState != farmmodel.DesiredRunning {
			continue
		}
		workloadID := facts.Workload.WorkloadID
		if facts.Binding != nil && facts.Binding.BlockedGeneration == facts.Workload.DesiredGeneration {
			kind := incidentTypeForCode(facts.Binding.ErrorCode)
			add(kind, farmmodel.IncidentSeverityError, &workloadID, nil, nil, facts.Binding.ErrorCode)
			continue
		}
		if facts.Current == nil {
			add(farmmodel.IncidentConfigurationBlocked, farmmodel.IncidentSeverityError, &workloadID, nil, nil, farmerr.NOT_FOUND)
			continue
		}
		if devices := missingDevices(input.Observed.Inventory, facts.Current.Resources.DeviceIDs); len(devices) != 0 {
			for i := range devices {
				add(farmmodel.IncidentIncompatibleHardware, farmmodel.IncidentSeverityError, &workloadID, nil, &devices[i], farmerr.INCOMPATIBLE_HARDWARE)
			}
			continue
		}
		if devices, unscoped := unmanagedConflicts(facts.Current.Resources, input.Observed.UnmanagedProcesses); len(devices) != 0 || unscoped {
			for i := range devices {
				add(farmmodel.IncidentUnmanagedProcessConflict, farmmodel.IncidentSeverityWarning, &workloadID, nil, &devices[i], farmerr.UNMANAGED_PROCESS_CONFLICT)
			}
			if unscoped {
				add(farmmodel.IncidentUnmanagedProcessConflict, farmmodel.IncidentSeverityWarning, &workloadID, nil, nil, farmerr.UNMANAGED_PROCESS_CONFLICT)
			}
			continue
		}
		if devices, cpu := ownedResourceConflicts(*facts.Current, input.Observed.Executions, known); len(devices) != 0 || cpu {
			for i := range devices {
				add(farmmodel.IncidentResourceConflict, farmmodel.IncidentSeverityError, &workloadID, nil, &devices[i], farmerr.CONFIG_CONFLICT)
			}
			if cpu {
				add(farmmodel.IncidentResourceConflict, farmmodel.IncidentSeverityError, &workloadID, nil, nil, farmerr.CONFIG_CONFLICT)
			}
			continue
		}
		// Hold intentionally suppresses managed execution and useful-work activity,
		// but it does not erase independent configuration, hardware, unmanaged, or
		// resource-conflict facts evaluated above.
		if holdActive {
			continue
		}
		execution := currentExecution(*facts.Current, input.Observed.Executions)
		if execution == nil {
			add(farmmodel.IncidentDesiredRunningNotExecuting, farmmodel.IncidentSeverityError, &workloadID, nil, nil, farmerr.PROCESS_CRASHED)
			continue
		}
		executionID := execution.ExecutionID
		switch execution.Status {
		case model.ExecutionFailed, model.ExecutionCrashed, model.ExecutionBackoff:
			add(farmmodel.IncidentRuntimeError, farmmodel.IncidentSeverityError, &workloadID, &executionID, nil, farmerr.PROCESS_CRASHED)
			continue
		case model.ExecutionStopped:
			add(farmmodel.IncidentDesiredRunningNotExecuting, farmmodel.IncidentSeverityError, &workloadID, &executionID, nil, farmerr.PROCESS_CRASHED)
			continue
		case model.ExecutionStarting, model.ExecutionStopping:
			continue
		}
		if execution.MinerTelemetry != nil && execution.MinerTelemetry.Health == model.MinerHealthError {
			reason := execution.MinerTelemetry.ErrorCode
			if reason == "" {
				reason = farmerr.TELEMETRY_UNAVAILABLE
			}
			add(farmmodel.IncidentRuntimeError, farmmodel.IncidentSeverityError, &workloadID, &executionID, nil, reason)
			continue
		}
		useful := execution.UsefulWork
		if useful == nil || useful.Availability == model.TelemetryUnknown {
			add(farmmodel.IncidentTelemetryUnknown, farmmodel.IncidentSeverityWarning, &workloadID, &executionID, nil, farmerr.TELEMETRY_UNKNOWN)
			continue
		}
		if useful.Availability == model.TelemetryUnavailable {
			add(farmmodel.IncidentTelemetryUnavailable, farmmodel.IncidentSeverityWarning, &workloadID, &executionID, nil, farmerr.TELEMETRY_UNAVAILABLE)
			continue
		}
		if useful.Availability == model.TelemetryStale {
			add(farmmodel.IncidentUsefulWorkDegraded, farmmodel.IncidentSeverityWarning, &workloadID, &executionID, nil, farmerr.TELEMETRY_STALE)
			continue
		}
		if useful.UsefulWork == model.UsefulWorkUnknown {
			add(farmmodel.IncidentTelemetryUnknown, farmmodel.IncidentSeverityWarning, &workloadID, &executionID, nil, farmerr.TELEMETRY_UNKNOWN)
			continue
		}
		if useful.UsefulWork != model.UsefulWorkConfirmed {
			reason := useful.ReasonCode
			if reason == "" {
				reason = farmerr.USEFUL_WORK_NOT_CONFIRMED
			}
			add(farmmodel.IncidentUsefulWorkDegraded, farmmodel.IncidentSeverityWarning, &workloadID, &executionID, nil, reason)
		}
	}
	return finish(result)
}

func NewCondition(kind farmmodel.IncidentType, severity farmmodel.IncidentSeverity, host identity.HostID, workload *identity.WorkloadID, execution *identity.ExecutionID, device *identity.DeviceID, reason farmerr.Code) farmmodel.IncidentCondition {
	parts := []string{string(kind), host.String(), string(reason)}
	for _, value := range []string{stringValue(workload), stringValue(execution), stringValue(device)} {
		parts = append(parts, value)
	}
	key := strings.Join(parts, "|")
	sum := sha256.Sum256([]byte(key))
	id, err := identity.ParseIncidentID("incident_" + hex.EncodeToString(sum[:16]))
	if err != nil {
		panic("deterministic IncidentID construction failed: " + err.Error())
	}
	return farmmodel.IncidentCondition{IncidentID: id, Key: key, Type: kind, Severity: severity, HostID: host, WorkloadID: clone(workload), ExecutionID: clone(execution), DeviceID: clone(device), ReasonCode: reason, Source: farmmodel.IncidentSourceController}
}

func finish(result Evaluation) Evaluation {
	sort.Slice(result.Conditions, func(i, j int) bool { return result.Conditions[i].Key < result.Conditions[j].Key })
	return result
}

func allTypes() []farmmodel.IncidentType {
	return []farmmodel.IncidentType{farmmodel.IncidentAgentOffline, farmmodel.IncidentMonitoringStale, farmmodel.IncidentDesiredRunningNotExecuting, farmmodel.IncidentRuntimeError, farmmodel.IncidentUsefulWorkDegraded, farmmodel.IncidentTelemetryUnknown, farmmodel.IncidentTelemetryUnavailable, farmmodel.IncidentIncompatibleHardware, farmmodel.IncidentUnmanagedProcessConflict, farmmodel.IncidentResourceConflict, farmmodel.IncidentMaintenanceHold, farmmodel.IncidentConfigurationBlocked}
}

func incidentTypeForCode(code farmerr.Code) farmmodel.IncidentType {
	switch code {
	case farmerr.INCOMPATIBLE_HARDWARE:
		return farmmodel.IncidentIncompatibleHardware
	case farmerr.UNMANAGED_PROCESS_CONFLICT:
		return farmmodel.IncidentUnmanagedProcessConflict
	default:
		return farmmodel.IncidentConfigurationBlocked
	}
}

func currentExecution(snapshot farmmodel.ResolvedExecutionSnapshot, executions []model.ExecutionObservation) *model.ExecutionObservation {
	for i := range executions {
		item := &executions[i]
		if item.ExecutionID == snapshot.ExecutionID && item.Ownership != nil && item.Ownership.WorkloadID == snapshot.WorkloadID && item.Ownership.DesiredGeneration == snapshot.DesiredGeneration && item.Ownership.ResolvedHash == snapshot.ResolvedHash && item.Ownership.HostID == snapshot.HostID && item.Ownership.CPU == snapshot.Resources.CPU && slices.Equal(item.Ownership.DeviceIDs, snapshot.Resources.DeviceIDs) {
			return item
		}
	}
	return nil
}

func missingDevices(inventory model.Inventory, devices []identity.DeviceID) []identity.DeviceID {
	var missing []identity.DeviceID
	for _, wanted := range devices {
		found := false
		for _, gpu := range inventory.GPUs {
			found = found || gpu.DeviceID == wanted
		}
		if !found {
			missing = append(missing, wanted)
		}
	}
	return missing
}

func unmanagedConflicts(claim farmmodel.ResourceClaim, processes []model.UnmanagedProcessObservation) ([]identity.DeviceID, bool) {
	devices := make(map[identity.DeviceID]struct{})
	unscoped := false
	for _, process := range processes {
		for _, device := range claim.DeviceIDs {
			if slices.Contains(process.DeviceIDs, device) {
				devices[device] = struct{}{}
			}
		}
		if process.CPU && claim.CPU || process.ResourceScope == model.ProcessResourcesUnknown && (claim.CPU && process.CPURelevant || len(claim.DeviceIDs) != 0 && process.GPURelevant) {
			unscoped = true
		}
	}
	result := make([]identity.DeviceID, 0, len(devices))
	for device := range devices {
		result = append(result, device)
	}
	slices.SortFunc(result, func(a, b identity.DeviceID) int { return strings.Compare(a.String(), b.String()) })
	return result, unscoped
}

func ownedResourceConflicts(current farmmodel.ResolvedExecutionSnapshot, executions []model.ExecutionObservation, known map[identity.ExecutionID]farmmodel.ResolvedExecutionSnapshot) ([]identity.DeviceID, bool) {
	devices := make(map[identity.DeviceID]struct{})
	cpu := false
	for _, execution := range executions {
		if execution.Status == model.ExecutionStopped || execution.ExecutionID == current.ExecutionID {
			continue
		}
		other, ok := known[execution.ExecutionID]
		if !ok || currentExecution(other, []model.ExecutionObservation{execution}) == nil || other.WorkloadID == current.WorkloadID {
			continue
		}
		if current.Resources.CPU && other.Resources.CPU {
			cpu = true
		}
		for _, device := range current.Resources.DeviceIDs {
			if slices.Contains(other.Resources.DeviceIDs, device) {
				devices[device] = struct{}{}
			}
		}
	}
	result := make([]identity.DeviceID, 0, len(devices))
	for device := range devices {
		result = append(result, device)
	}
	slices.SortFunc(result, func(a, b identity.DeviceID) int { return strings.Compare(a.String(), b.String()) })
	return result, cpu
}

func stringValue[T interface{ String() string }](value *T) string {
	if value == nil {
		return ""
	}
	return (*value).String()
}

func clone[T any](value *T) *T {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}
