package reconcile

import (
	"context"
	"log"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/le0xdon/le0xfarm/internal/controllernet"
	"github.com/le0xdon/le0xfarm/internal/controllerstate"
	"github.com/le0xdon/le0xfarm/internal/farmerr"
	"github.com/le0xdon/le0xfarm/internal/farmmodel"
	"github.com/le0xdon/le0xfarm/internal/identity"
	"github.com/le0xdon/le0xfarm/internal/model"
)

type WorkloadStore interface {
	GetDesiredWorkload(context.Context, identity.WorkloadID) (farmmodel.DesiredWorkload, error)
	ListDesiredWorkloads(context.Context) ([]farmmodel.DesiredWorkload, error)
	DeleteDesiredWorkload(context.Context, identity.WorkloadID, uint64) error
	FinalizeRetiredDesiredWorkloadDeletion(context.Context, identity.WorkloadID, uint64) error
	GetCurrentResolvedSnapshot(context.Context, identity.WorkloadID) (farmmodel.ResolvedExecutionSnapshot, error)
	ListResolvedSnapshots(context.Context, identity.WorkloadID) ([]farmmodel.ResolvedExecutionSnapshot, error)
	ListAllResolvedSnapshots(context.Context) ([]farmmodel.ResolvedExecutionSnapshot, error)
	BlockDesiredGeneration(context.Context, identity.WorkloadID, uint64, farmerr.Code, string) error
	GetWorkloadRuntimeBinding(context.Context, identity.WorkloadID) (farmmodel.WorkloadRuntimeBinding, bool, error)
	RetryWorkload(context.Context, identity.WorkloadID, uint64) (farmmodel.DesiredWorkload, error)
}

type Commander interface {
	Session(identity.HostID) (controllernet.SessionInfo, bool)
	SendRuntime(context.Context, controllernet.SessionInfo, controllernet.RuntimeRequest) (uint64, error)
}

type inFlightAction struct {
	dispatchSequence uint64
}

type Coordinator struct {
	store       WorkloadStore
	observed    *controllerstate.Store
	commander   Commander
	output      *log.Logger
	mu          sync.Mutex
	inFlight    map[ActionKey]inFlightAction
	hostLocksMu sync.Mutex
	hostLocks   map[identity.HostID]*sync.Mutex
}

func NewCoordinator(store WorkloadStore, observed *controllerstate.Store, commander Commander, output *log.Logger) *Coordinator {
	if output == nil {
		output = log.Default()
	}
	return &Coordinator{store: store, observed: observed, commander: commander, output: output, inFlight: make(map[ActionKey]inFlightAction), hostLocks: make(map[identity.HostID]*sync.Mutex)}
}

func (coordinator *Coordinator) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = time.Second
	}
	coordinator.ReconcileAll(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			coordinator.ReconcileAll(ctx)
		}
	}
}

func (coordinator *Coordinator) ReconcileAll(ctx context.Context) {
	workloads, err := coordinator.store.ListDesiredWorkloads(ctx)
	if err != nil {
		coordinator.output.Printf("RECONCILE: cannot load desired workloads: %v", err)
		return
	}
	hosts := make(map[identity.HostID]struct{})
	for _, workload := range workloads {
		hosts[workload.HostID] = struct{}{}
	}
	for hostID := range hosts {
		hostID := hostID
		go coordinator.ReconcileHost(ctx, hostID)
	}
}

func (coordinator *Coordinator) ReconcileHost(ctx context.Context, hostID identity.HostID) {
	lock := coordinator.hostLock(hostID)
	lock.Lock()
	dispatch, ok := coordinator.prepareDispatchLocked(ctx, hostID)
	lock.Unlock()
	if !ok {
		return
	}

	// Re-read mutable desired/observed facts at the last application-layer
	// boundary before network dispatch. The reservation remains visible while
	// this check and the bounded send run.
	if !coordinator.actionStillCurrent(ctx, dispatch.action, dispatch.observed) {
		coordinator.clearReservedAction(dispatch.action.Key)
		return
	}
	sequence, err := coordinator.commander.SendRuntime(ctx, dispatch.session, dispatch.request)
	if err != nil {
		// Delivery is uncertain. The network boundary invalidates/revokes the
		// session; make the observed epoch unusable and let reconnect plus a fresh
		// GET_EXECUTIONS establish reality before any retry.
		coordinator.SessionDisconnected(dispatch.session)
		return
	}
	lock.Lock()
	coordinator.setDispatchSequence(dispatch.action.Key, sequence)
	lock.Unlock()
}

type preparedDispatch struct {
	action   Action
	request  controllernet.RuntimeRequest
	session  controllernet.SessionInfo
	observed controllerstate.HostObservation
}

// prepareDispatchLocked plans and reserves at most one Host action. The caller
// holds the Host transition lock. No network operation occurs here.
func (coordinator *Coordinator) prepareDispatchLocked(ctx context.Context, hostID identity.HostID) (preparedDispatch, bool) {
	observed, ok := coordinator.observed.Get(hostID)
	if !ok || !observed.Connected || !observed.Ready || !observed.Fresh {
		return preparedDispatch{}, false
	}
	// M4.3 deliberately serializes runtime convergence per Host. In particular,
	// a newer generation must not START while an older START outcome is unknown.
	if coordinator.hostHasInFlight(hostID, observed.ConnectionEpoch) {
		return preparedDispatch{}, false
	}
	allSnapshots, err := coordinator.store.ListAllResolvedSnapshots(ctx)
	if err != nil {
		coordinator.output.Printf("RECONCILE: cannot load snapshots for HostID %s", hostID)
		return preparedDispatch{}, false
	}
	byExecution := make(map[identity.ExecutionID]farmmodel.ResolvedExecutionSnapshot, len(allSnapshots))
	for _, snapshot := range allSnapshots {
		byExecution[snapshot.ExecutionID] = snapshot
	}
	workloads, err := coordinator.store.ListDesiredWorkloads(ctx)
	if err != nil {
		coordinator.output.Printf("RECONCILE: cannot load workloads for HostID %s", hostID)
		return preparedDispatch{}, false
	}
	slices.SortFunc(workloads, func(a, b farmmodel.DesiredWorkload) int {
		return strings.Compare(a.WorkloadID.String(), b.WorkloadID.String())
	})
	for _, workload := range workloads {
		if workload.HostID != hostID {
			continue
		}
		snapshots, err := coordinator.store.ListResolvedSnapshots(ctx, workload.WorkloadID)
		if err != nil {
			coordinator.output.Printf("RECONCILE: cannot load snapshots for WorkloadID %s", workload.WorkloadID)
			continue
		}
		var current *farmmodel.ResolvedExecutionSnapshot
		if workload.RunState == farmmodel.DesiredRunning {
			value, err := coordinator.store.GetCurrentResolvedSnapshot(ctx, workload.WorkloadID)
			if err != nil {
				coordinator.output.Printf("RECONCILE: current snapshot unavailable for WorkloadID %s", workload.WorkloadID)
				continue
			}
			current = &value
		}
		binding, blocked, err := coordinator.store.GetWorkloadRuntimeBinding(ctx, workload.WorkloadID)
		if err != nil {
			coordinator.output.Printf("RECONCILE: runtime binding unavailable for WorkloadID %s", workload.WorkloadID)
			continue
		}
		var bindingPtr *farmmodel.WorkloadRuntimeBinding
		if blocked {
			bindingPtr = &binding
		}
		planned := Plan(PlanInput{Workload: workload, Current: current, Snapshots: snapshots, Binding: bindingPtr, Observed: observed, InFlight: coordinator.inFlightSnapshot(), OtherOwned: byExecution})
		if planned.Conflict != "" {
			coordinator.output.Printf("RECONCILE: WorkloadID %s blocked pending safe convergence: %s", workload.WorkloadID, planned.Conflict)
			continue
		}
		if len(planned.Actions) == 0 {
			continue
		}
		action := planned.Actions[0]
		if action.Key.Action == ActionBlock {
			if err := coordinator.store.BlockDesiredGeneration(ctx, action.Key.WorkloadID, action.Key.DesiredGeneration, action.Code, action.Message); err != nil {
				coordinator.output.Printf("RECONCILE: cannot block failed WorkloadID %s", action.Key.WorkloadID)
			}
			continue
		}
		if !coordinator.actionStillCurrent(ctx, action, observed) {
			return preparedDispatch{}, false
		}
		request := controllernet.RuntimeRequest{WorkloadID: action.Key.WorkloadID, DesiredGeneration: action.Key.DesiredGeneration, ExecutionID: action.Key.ExecutionID, ResolvedHash: action.Snapshot.ResolvedHash, Plan: action.Snapshot.Plan}
		if action.Key.Action == ActionStart {
			request.Kind = controllernet.RuntimeStart
		} else {
			request.Kind = controllernet.RuntimeStop
		}
		session, ok := coordinator.commander.Session(hostID)
		if !ok || session.ConnectionEpoch != observed.ConnectionEpoch || !session.Ready {
			return preparedDispatch{}, false
		}
		if !coordinator.markInFlight(action.Key) {
			return preparedDispatch{}, false
		}
		return preparedDispatch{action: action, request: request, session: session, observed: observed}, true
	}
	return preparedDispatch{}, false
}

func (coordinator *Coordinator) actionStillCurrent(ctx context.Context, action Action, observed controllerstate.HostObservation) bool {
	latest, err := coordinator.store.GetDesiredWorkload(ctx, action.Key.WorkloadID)
	if err != nil || latest.HostID != action.Key.HostID {
		return false
	}
	currentObserved, ok := coordinator.observed.Get(action.Key.HostID)
	if !ok || !currentObserved.Connected || !currentObserved.Ready || !currentObserved.Fresh || currentObserved.ConnectionEpoch != observed.ConnectionEpoch || currentObserved.Revision != observed.Revision {
		return false
	}
	if action.Key.Action == ActionStart {
		if latest.RunState != farmmodel.DesiredRunning || latest.DesiredGeneration != action.Key.DesiredGeneration {
			return false
		}
		snapshot, err := coordinator.store.GetCurrentResolvedSnapshot(ctx, latest.WorkloadID)
		return err == nil && snapshot.ExecutionID == action.Key.ExecutionID && snapshot.ResolvedHash == action.Snapshot.ResolvedHash
	}
	return true
}

func (coordinator *Coordinator) SessionConnected(info controllernet.SessionInfo) {
	lock := coordinator.hostLock(info.HostID)
	lock.Lock()
	defer lock.Unlock()
	coordinator.observed.Connect(info.AgentID, info.HostID, info.ConnectionEpoch)
}

func (coordinator *Coordinator) SessionDisconnected(info controllernet.SessionInfo) {
	lock := coordinator.hostLock(info.HostID)
	lock.Lock()
	defer lock.Unlock()
	coordinator.observed.Disconnect(info.HostID, info.ConnectionEpoch)
	coordinator.mu.Lock()
	for key := range coordinator.inFlight {
		if key.HostID == info.HostID && key.ConnectionEpoch == info.ConnectionEpoch {
			delete(coordinator.inFlight, key)
		}
	}
	coordinator.mu.Unlock()
}

func (coordinator *Coordinator) ExecutionsObserved(info controllernet.SessionInfo, executions []model.ExecutionObservation, runtimeSequence uint64) {
	lock := coordinator.hostLock(info.HostID)
	lock.Lock()
	updated := coordinator.observed.SetExecutions(info.HostID, info.ConnectionEpoch, executions, time.Now().UTC(), runtimeSequence)
	if updated {
		coordinator.resolveInFlightThroughObservation(info.HostID, info.ConnectionEpoch, runtimeSequence)
	}
	lock.Unlock()
	if updated {
		go coordinator.ReconcileHost(context.Background(), info.HostID)
	}
}

func (coordinator *Coordinator) InventoryObserved(info controllernet.SessionInfo, inventory model.Inventory) {
	lock := coordinator.hostLock(info.HostID)
	lock.Lock()
	defer lock.Unlock()
	coordinator.observed.SetInventory(info.HostID, info.ConnectionEpoch, inventory)
}

func (coordinator *Coordinator) StatusObserved(info controllernet.SessionInfo, state model.AgentState) {
	lock := coordinator.hostLock(info.HostID)
	lock.Lock()
	defer lock.Unlock()
	coordinator.observed.SetAgentState(info.HostID, info.ConnectionEpoch, state)
}

func (coordinator *Coordinator) SessionReady(info controllernet.SessionInfo) {
	lock := coordinator.hostLock(info.HostID)
	lock.Lock()
	ready := coordinator.observed.MarkReady(info.HostID, info.ConnectionEpoch)
	lock.Unlock()
	if ready {
		go coordinator.ReconcileHost(context.Background(), info.HostID)
	}
}

func (coordinator *Coordinator) RuntimeResult(info controllernet.SessionInfo, request controllernet.RuntimeRequest, execution *model.ExecutionObservation, agentError *farmerr.Error) {
	lock := coordinator.hostLock(info.HostID)
	lock.Lock()
	actionKind := ActionStop
	if request.Kind == controllernet.RuntimeStart {
		actionKind = ActionStart
	}
	key := ActionKey{HostID: info.HostID, WorkloadID: request.WorkloadID, DesiredGeneration: request.DesiredGeneration, ExecutionID: request.ExecutionID, ConnectionEpoch: info.ConnectionEpoch, Action: actionKind}
	observed, ok := coordinator.observed.Get(info.HostID)
	if !ok || !observed.Connected || !observed.Fresh || observed.ConnectionEpoch != info.ConnectionEpoch {
		coordinator.clearInFlight(key)
		lock.Unlock()
		return
	}
	if agentError != nil {
		if request.Kind == controllernet.RuntimeStart && permanentRejection(agentError.Code) && coordinator.requestStillCurrent(context.Background(), request) {
			if err := coordinator.store.BlockDesiredGeneration(context.Background(), request.WorkloadID, request.DesiredGeneration, agentError.Code, agentError.HumanMessage); err != nil {
				coordinator.output.Printf("RECONCILE: cannot persist rejected WorkloadID %s generation %d", request.WorkloadID, request.DesiredGeneration)
				lock.Unlock()
				return
			}
			coordinator.clearInFlight(key)
		}
		// A malformed/transient result is an uncertain outcome. Keep the guard
		// until a causally later execution snapshot proves current reality.
		lock.Unlock()
		return
	}
	if execution == nil || execution.ExecutionID != request.ExecutionID {
		lock.Unlock()
		return
	}
	ownership := execution.Ownership
	if ownership == nil || ownership.WorkloadID != request.WorkloadID || ownership.DesiredGeneration != request.DesiredGeneration || ownership.ResolvedHash != request.ResolvedHash || ownership.HostID != info.HostID {
		lock.Unlock()
		return
	}
	updated := coordinator.observed.UpsertExecution(info.HostID, info.ConnectionEpoch, *execution, time.Now().UTC(), request.DispatchSequence)
	if updated {
		if request.Kind == controllernet.RuntimeStart && execution.Status == model.ExecutionFailed && coordinator.requestStillCurrent(context.Background(), request) {
			if err := coordinator.store.BlockDesiredGeneration(context.Background(), request.WorkloadID, request.DesiredGeneration, farmerr.PROCESS_CRASHED, "execution reached terminal FAILED state"); err != nil {
				coordinator.output.Printf("RECONCILE: cannot persist failed WorkloadID %s generation %d", request.WorkloadID, request.DesiredGeneration)
				lock.Unlock()
				return
			}
		}
		// Observed/runtime fact is visible before the action guard is released.
		coordinator.clearInFlight(key)
	}
	lock.Unlock()
	if updated {
		go coordinator.ReconcileHost(context.Background(), info.HostID)
	}
}

func (coordinator *Coordinator) requestStillCurrent(ctx context.Context, request controllernet.RuntimeRequest) bool {
	workload, err := coordinator.store.GetDesiredWorkload(ctx, request.WorkloadID)
	if err != nil || workload.RunState != farmmodel.DesiredRunning || workload.DesiredGeneration != request.DesiredGeneration {
		return false
	}
	snapshot, err := coordinator.store.GetCurrentResolvedSnapshot(ctx, request.WorkloadID)
	return err == nil && snapshot.ExecutionID == request.ExecutionID && snapshot.ResolvedHash == request.ResolvedHash
}

func permanentRejection(code farmerr.Code) bool {
	switch code {
	case farmerr.MISSING_DEPENDENCY, farmerr.MISSING_COMMAND, farmerr.MISSING_WALLET, farmerr.MISSING_POOL,
		farmerr.PACKAGE_NOT_INSTALLED, farmerr.PACKAGE_HASH_MISMATCH, farmerr.INCOMPATIBLE_HARDWARE,
		farmerr.INSUFFICIENT_DISK, farmerr.PERMISSION_DENIED, farmerr.PORT_CONFLICT, farmerr.CONFIG_CONFLICT,
		farmerr.SIGNATURE_INVALID:
		return true
	default:
		return false
	}
}

func (coordinator *Coordinator) RetryWorkload(ctx context.Context, id identity.WorkloadID, expectedRevision uint64) (farmmodel.DesiredWorkload, error) {
	workload, err := coordinator.store.RetryWorkload(ctx, id, expectedRevision)
	if err == nil {
		go coordinator.ReconcileHost(context.Background(), workload.HostID)
	}
	return workload, err
}

func (coordinator *Coordinator) DeleteDesiredWorkload(ctx context.Context, id identity.WorkloadID, expectedRevision uint64) error {
	workload, err := coordinator.store.GetDesiredWorkload(ctx, id)
	if err != nil {
		return err
	}
	lock := coordinator.hostLock(workload.HostID)
	lock.Lock()
	defer lock.Unlock()
	// Re-read under the same Host transition boundary used by dispatch/result.
	workload, err = coordinator.store.GetDesiredWorkload(ctx, id)
	if err != nil {
		return err
	}
	if workload.Meta.Revision != expectedRevision {
		return farmerr.Error{Code: farmerr.REVISION_CONFLICT, HumanMessage: "DesiredWorkload revision changed before retirement"}
	}
	if coordinator.workloadHasInFlight(workload.HostID, id) {
		return farmerr.Error{Code: farmerr.REFERENCE_IN_USE, HumanMessage: "DesiredWorkload has unresolved runtime activity"}
	}
	snapshots, err := coordinator.store.ListResolvedSnapshots(ctx, id)
	if err != nil {
		return err
	}
	if len(snapshots) == 0 {
		return coordinator.store.DeleteDesiredWorkload(ctx, id, expectedRevision)
	}
	if workload.RunState != farmmodel.DesiredStopped {
		return farmerr.Error{Code: farmerr.REFERENCE_IN_USE, HumanMessage: "DesiredWorkload must be STOPPED before deletion"}
	}
	observed, ok := coordinator.observed.Get(workload.HostID)
	if !ok || !observed.Connected || !observed.Ready || !observed.Fresh {
		return farmerr.Error{Code: farmerr.SERVICE_NOT_READY, HumanMessage: "fresh Agent retirement proof is required"}
	}
	byExecution := make(map[identity.ExecutionID]farmmodel.ResolvedExecutionSnapshot, len(snapshots))
	for _, snapshot := range snapshots {
		byExecution[snapshot.ExecutionID] = snapshot
	}
	for _, execution := range observed.Executions {
		if execution.Status == model.ExecutionStopped {
			continue
		}
		if snapshot, exists := byExecution[execution.ExecutionID]; exists && ownershipMatchesSnapshot(execution.Ownership, snapshot) {
			return farmerr.Error{Code: farmerr.REFERENCE_IN_USE, HumanMessage: "DesiredWorkload still owns an Agent execution"}
		}
	}
	return coordinator.store.FinalizeRetiredDesiredWorkloadDeletion(ctx, id, expectedRevision)
}

type UnmanagedExecution struct {
	HostID        identity.HostID
	AgentID       identity.AgentID
	ExecutionID   identity.ExecutionID
	State         model.ExecutionStatus
	ResourceClaim *farmmodel.ResourceClaim
	MinerStatus   *model.MinerTelemetry
}

func (coordinator *Coordinator) UnmanagedExecutions(ctx context.Context, hostID identity.HostID) ([]UnmanagedExecution, error) {
	observed, ok := coordinator.observed.Get(hostID)
	if !ok {
		return nil, nil
	}
	snapshots, err := coordinator.store.ListAllResolvedSnapshots(ctx)
	if err != nil {
		return nil, err
	}
	known := make(map[identity.ExecutionID]farmmodel.ResolvedExecutionSnapshot, len(snapshots))
	for _, snapshot := range snapshots {
		known[snapshot.ExecutionID] = snapshot
	}
	var result []UnmanagedExecution
	for _, execution := range observed.Executions {
		if snapshot, exists := known[execution.ExecutionID]; exists && ownershipMatchesSnapshot(execution.Ownership, snapshot) {
			continue
		}
		item := UnmanagedExecution{HostID: hostID, AgentID: observed.AgentID, ExecutionID: execution.ExecutionID, State: execution.Status, MinerStatus: execution.MinerTelemetry}
		if execution.Ownership != nil {
			claim := farmmodel.ResourceClaim{CPU: execution.Ownership.CPU, DeviceIDs: append([]identity.DeviceID(nil), execution.Ownership.DeviceIDs...)}
			item.ResourceClaim = &claim
		}
		result = append(result, item)
	}
	return result, nil
}

func (coordinator *Coordinator) markInFlight(key ActionKey) bool {
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	if _, exists := coordinator.inFlight[key]; exists {
		return false
	}
	coordinator.inFlight[key] = inFlightAction{}
	return true
}

func (coordinator *Coordinator) setDispatchSequence(key ActionKey, sequence uint64) {
	observed, _ := coordinator.observed.Get(key.HostID)
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	if action, ok := coordinator.inFlight[key]; ok {
		if observed.ConnectionEpoch == key.ConnectionEpoch && observed.RuntimeSequence >= sequence {
			delete(coordinator.inFlight, key)
			return
		}
		action.dispatchSequence = sequence
		coordinator.inFlight[key] = action
	}
}

func (coordinator *Coordinator) clearReservedAction(key ActionKey) {
	lock := coordinator.hostLock(key.HostID)
	lock.Lock()
	coordinator.clearInFlight(key)
	lock.Unlock()
}

func (coordinator *Coordinator) resolveInFlightThroughObservation(hostID identity.HostID, epoch controllerstate.ConnectionEpoch, runtimeSequence uint64) {
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	for key, action := range coordinator.inFlight {
		if key.HostID == hostID && key.ConnectionEpoch == epoch && action.dispatchSequence > 0 && action.dispatchSequence <= runtimeSequence {
			delete(coordinator.inFlight, key)
		}
	}
}

func (coordinator *Coordinator) clearInFlight(key ActionKey) {
	coordinator.mu.Lock()
	delete(coordinator.inFlight, key)
	coordinator.mu.Unlock()
}

func (coordinator *Coordinator) inFlightSnapshot() map[ActionKey]struct{} {
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	result := make(map[ActionKey]struct{}, len(coordinator.inFlight))
	for key := range coordinator.inFlight {
		result[key] = struct{}{}
	}
	return result
}

func (coordinator *Coordinator) workloadHasInFlight(hostID identity.HostID, workloadID identity.WorkloadID) bool {
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	for key := range coordinator.inFlight {
		if key.HostID == hostID && key.WorkloadID == workloadID {
			return true
		}
	}
	return false
}

func (coordinator *Coordinator) hostHasInFlight(hostID identity.HostID, epoch controllerstate.ConnectionEpoch) bool {
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	for key := range coordinator.inFlight {
		if key.HostID == hostID && key.ConnectionEpoch == epoch {
			return true
		}
	}
	return false
}

func (coordinator *Coordinator) hostLock(hostID identity.HostID) *sync.Mutex {
	coordinator.hostLocksMu.Lock()
	defer coordinator.hostLocksMu.Unlock()
	if lock := coordinator.hostLocks[hostID]; lock != nil {
		return lock
	}
	lock := &sync.Mutex{}
	coordinator.hostLocks[hostID] = lock
	return lock
}

var _ controllernet.SessionHandler = (*Coordinator)(nil)
