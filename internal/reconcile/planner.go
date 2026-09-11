// Package reconcile implements level-triggered desired/observed convergence.
package reconcile

import (
	"slices"
	"strings"

	"github.com/le0xdon/le0xfarm/internal/controllerstate"
	"github.com/le0xdon/le0xfarm/internal/farmerr"
	"github.com/le0xdon/le0xfarm/internal/farmmodel"
	"github.com/le0xdon/le0xfarm/internal/identity"
	"github.com/le0xdon/le0xfarm/internal/model"
)

type ActionKind string

const (
	ActionStart ActionKind = "START"
	ActionStop  ActionKind = "STOP"
	ActionBlock ActionKind = "BLOCK"
)

type ActionKey struct {
	HostID            identity.HostID
	WorkloadID        identity.WorkloadID
	DesiredGeneration uint64
	ExecutionID       identity.ExecutionID
	ConnectionEpoch   controllerstate.ConnectionEpoch
	Action            ActionKind
}

type Action struct {
	Key      ActionKey
	Snapshot farmmodel.ResolvedExecutionSnapshot
	Code     farmerr.Code
	Message  string
}

type PlanInput struct {
	Workload   farmmodel.DesiredWorkload
	Current    *farmmodel.ResolvedExecutionSnapshot
	Snapshots  []farmmodel.ResolvedExecutionSnapshot
	Binding    *farmmodel.WorkloadRuntimeBinding
	Hold       *farmmodel.MaintenanceHold
	Observed   controllerstate.HostObservation
	InFlight   map[ActionKey]struct{}
	OtherOwned map[identity.ExecutionID]farmmodel.ResolvedExecutionSnapshot
}

type PlanResult struct {
	Actions  []Action
	Code     farmerr.Code
	Conflict string
}

// Plan is pure and deterministic. It emits at most one ordered action for one
// workload; another level-triggered pass follows confirmed observation.
func Plan(input PlanInput) PlanResult {
	workload := input.Workload
	observed := input.Observed
	if !observed.Connected || !observed.Ready || !observed.Fresh || observed.HostID != workload.HostID {
		return PlanResult{}
	}

	known := make(map[identity.ExecutionID]farmmodel.ResolvedExecutionSnapshot, len(input.Snapshots)+len(input.OtherOwned))
	for _, snapshot := range input.Snapshots {
		known[snapshot.ExecutionID] = snapshot
	}
	for id, snapshot := range input.OtherOwned {
		known[id] = snapshot
	}

	activeOwned := make([]model.ExecutionObservation, 0)
	var current *model.ExecutionObservation
	for i := range observed.Executions {
		execution := observed.Executions[i]
		if !holdsResources(execution.Status) {
			continue
		}
		snapshot, owned := known[execution.ExecutionID]
		if !owned || !ownershipMatchesSnapshot(execution.Ownership, snapshot) {
			continue
		}
		if snapshot.WorkloadID != workload.WorkloadID {
			continue
		}
		activeOwned = append(activeOwned, execution)
		if input.Current != nil && snapshotIdentityEqual(snapshot, *input.Current) && claimMatches(execution.Ownership, input.Current.Resources) {
			copy := execution
			current = &copy
		}
	}
	slices.SortFunc(activeOwned, func(a, b model.ExecutionObservation) int {
		return strings.Compare(a.ExecutionID.String(), b.ExecutionID.String())
	})

	// Old or claim-mismatched owned executions are always retired first.
	for _, execution := range activeOwned {
		if current != nil && execution.ExecutionID == current.ExecutionID {
			continue
		}
		snapshot := known[execution.ExecutionID]
		action := Action{Key: ActionKey{HostID: workload.HostID, WorkloadID: workload.WorkloadID, DesiredGeneration: snapshot.DesiredGeneration, ExecutionID: execution.ExecutionID, ConnectionEpoch: observed.ConnectionEpoch, Action: ActionStop}, Snapshot: snapshot}
		if _, inFlight := input.InFlight[action.Key]; inFlight {
			return PlanResult{}
		}
		return PlanResult{Actions: []Action{action}}
	}

	if workload.RunState == farmmodel.DesiredStopped {
		if current == nil {
			return PlanResult{}
		}
		action := Action{Key: ActionKey{HostID: workload.HostID, WorkloadID: workload.WorkloadID, DesiredGeneration: current.Ownership.DesiredGeneration, ExecutionID: current.ExecutionID, ConnectionEpoch: observed.ConnectionEpoch, Action: ActionStop}}
		if input.Current != nil {
			action.Snapshot = *input.Current
		}
		if _, inFlight := input.InFlight[action.Key]; inFlight {
			return PlanResult{}
		}
		return PlanResult{Actions: []Action{action}}
	}
	if input.Hold != nil && input.Hold.Active {
		if current == nil {
			return PlanResult{Code: farmerr.MAINTENANCE_HOLD, Conflict: "Desired RUNNING is suppressed by Maintenance Hold"}
		}
		action := Action{Key: ActionKey{HostID: workload.HostID, WorkloadID: workload.WorkloadID, DesiredGeneration: current.Ownership.DesiredGeneration, ExecutionID: current.ExecutionID, ConnectionEpoch: observed.ConnectionEpoch, Action: ActionStop}, Snapshot: *input.Current}
		if _, inFlight := input.InFlight[action.Key]; inFlight {
			return PlanResult{}
		}
		return PlanResult{Actions: []Action{action}}
	}

	if input.Binding != nil && input.Binding.BlockedGeneration == workload.DesiredGeneration {
		return PlanResult{}
	}
	if current != nil {
		if current.Status == model.ExecutionFailed {
			action := Action{Key: ActionKey{HostID: workload.HostID, WorkloadID: workload.WorkloadID, DesiredGeneration: workload.DesiredGeneration, ExecutionID: current.ExecutionID, ConnectionEpoch: observed.ConnectionEpoch, Action: ActionBlock}, Code: farmerr.PROCESS_CRASHED, Message: "execution reached terminal FAILED state"}
			return PlanResult{Actions: []Action{action}}
		}
		return PlanResult{}
	}
	if input.Current == nil {
		return PlanResult{Conflict: "RUNNING workload has no current resolved snapshot"}
	}
	if code, conflict := startConflict(input, known); conflict != "" {
		return PlanResult{Code: code, Conflict: conflict}
	}
	action := Action{Key: ActionKey{HostID: workload.HostID, WorkloadID: workload.WorkloadID, DesiredGeneration: workload.DesiredGeneration, ExecutionID: input.Current.ExecutionID, ConnectionEpoch: observed.ConnectionEpoch, Action: ActionStart}, Snapshot: *input.Current}
	if _, inFlight := input.InFlight[action.Key]; inFlight {
		return PlanResult{}
	}
	return PlanResult{Actions: []Action{action}}
}

func startConflict(input PlanInput, known map[identity.ExecutionID]farmmodel.ResolvedExecutionSnapshot) (farmerr.Code, string) {
	claim := input.Current.Resources
	if !inventoryContains(input.Observed.Inventory, claim.DeviceIDs) {
		return farmerr.INCOMPATIBLE_HARDWARE, "fresh inventory does not contain every desired DeviceID"
	}
	for _, process := range input.Observed.UnmanagedProcesses {
		overlaps := process.CPU && claim.CPU
		for _, deviceID := range claim.DeviceIDs {
			if slices.Contains(process.DeviceIDs, deviceID) {
				overlaps = true
				break
			}
		}
		ambiguous := process.ResourceScope == model.ProcessResourcesUnknown && ((claim.CPU && process.CPURelevant) || (len(claim.DeviceIDs) != 0 && process.GPURelevant))
		if overlaps || ambiguous {
			return farmerr.UNMANAGED_PROCESS_CONFLICT, "fresh observation reports a conflicting unmanaged mining process"
		}
	}
	for _, execution := range input.Observed.Executions {
		if !blocksStartResources(execution.Status) {
			continue
		}
		if execution.ExecutionID == input.Current.ExecutionID && ownershipMatchesSnapshot(execution.Ownership, *input.Current) {
			continue
		}
		if snapshot, ok := known[execution.ExecutionID]; ok && ownershipMatchesSnapshot(execution.Ownership, snapshot) {
			if snapshot.WorkloadID == input.Workload.WorkloadID {
				return farmerr.CONFIG_CONFLICT, "obsolete owned execution must be absent before START"
			}
			if claimsOverlap(claim, snapshot.Resources) {
				return farmerr.CONFIG_CONFLICT, "another Controller-owned execution overlaps desired resources"
			}
			continue
		}
		if execution.Ownership == nil {
			return farmerr.UNMANAGED_PROCESS_CONFLICT, "unmanaged execution has an unknown resource claim"
		}
		other := farmmodel.ResourceClaim{CPU: execution.Ownership.CPU, DeviceIDs: execution.Ownership.DeviceIDs}
		if claimsOverlap(claim, other) {
			return farmerr.UNMANAGED_PROCESS_CONFLICT, "unmanaged execution overlaps desired resources"
		}
	}
	return "", ""
}

func ownershipMatchesSnapshot(ownership *model.WorkloadOwnership, snapshot farmmodel.ResolvedExecutionSnapshot) bool {
	return ownership != nil && ownership.WorkloadID == snapshot.WorkloadID && ownership.DesiredGeneration == snapshot.DesiredGeneration && ownership.ResolvedHash == snapshot.ResolvedHash && ownership.HostID == snapshot.HostID
}

func snapshotIdentityEqual(a, b farmmodel.ResolvedExecutionSnapshot) bool {
	return a.ExecutionID == b.ExecutionID && a.WorkloadID == b.WorkloadID && a.DesiredGeneration == b.DesiredGeneration && a.ResolvedHash == b.ResolvedHash
}

func claimMatches(ownership *model.WorkloadOwnership, claim farmmodel.ResourceClaim) bool {
	return ownership != nil && ownership.CPU == claim.CPU && slices.Equal(ownership.DeviceIDs, claim.DeviceIDs)
}

func claimsOverlap(a, b farmmodel.ResourceClaim) bool {
	if a.CPU && b.CPU {
		return true
	}
	for _, left := range a.DeviceIDs {
		if slices.Contains(b.DeviceIDs, left) {
			return true
		}
	}
	return false
}

func inventoryContains(inventory model.Inventory, devices []identity.DeviceID) bool {
	for _, wanted := range devices {
		found := false
		for _, gpu := range inventory.GPUs {
			if gpu.DeviceID == wanted {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func holdsResources(status model.ExecutionStatus) bool {
	return status != model.ExecutionStopped
}

func blocksStartResources(status model.ExecutionStatus) bool {
	return status != model.ExecutionStopped && status != model.ExecutionFailed
}
