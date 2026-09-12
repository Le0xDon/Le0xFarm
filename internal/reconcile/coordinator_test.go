package reconcile

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/le0xdon/le0xfarm/internal/controllernet"
	"github.com/le0xdon/le0xfarm/internal/controllerstate"
	"github.com/le0xdon/le0xfarm/internal/farmerr"
	"github.com/le0xdon/le0xfarm/internal/farmmodel"
	"github.com/le0xdon/le0xfarm/internal/identity"
	"github.com/le0xdon/le0xfarm/internal/model"
)

func TestCoordinatorShutdownJoinsAdmittedWorkersAndRejectsNewWork(t *testing.T) {
	coordinator := NewCoordinator(nil, nil, nil, nil)
	entered := make(chan struct{})
	release := make(chan struct{})
	var exited atomic.Bool
	if !coordinator.startWorker(func(ctx context.Context) {
		close(entered)
		<-release
		if ctx.Err() == nil {
			t.Error("Controller lifetime context was not cancelled")
		}
		exited.Store(true)
	}) {
		t.Fatal("initial worker was not admitted")
	}
	<-entered
	done := make(chan struct{})
	go func() {
		coordinator.Shutdown()
		close(done)
	}()
	deadline := time.After(time.Second)
	for {
		coordinator.workersMu.Lock()
		closing := coordinator.closing
		coordinator.workersMu.Unlock()
		if closing {
			break
		}
		select {
		case <-deadline:
			t.Fatal("Shutdown did not close worker admission")
		default:
		}
	}
	select {
	case <-done:
		t.Fatal("Shutdown returned while an admitted worker was still running")
	default:
	}
	if coordinator.startWorker(func(context.Context) { t.Error("worker ran after shutdown admission closed") }) {
		t.Fatal("worker was admitted after shutdown began")
	}
	close(release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Shutdown did not join released worker")
	}
	if !exited.Load() {
		t.Fatal("worker did not exit before Shutdown returned")
	}
	// The public shutdown operation is idempotent.
	coordinator.Shutdown()
}

func TestCoordinatorWorkerAdmissionRacesShutdownSafely(t *testing.T) {
	coordinator := NewCoordinator(nil, nil, nil, nil)
	start := make(chan struct{})
	var callers sync.WaitGroup
	for range 100 {
		callers.Add(1)
		go func() {
			defer callers.Done()
			<-start
			coordinator.startWorker(func(ctx context.Context) { <-ctx.Done() })
		}()
	}
	shutdownDone := make(chan struct{})
	go func() {
		<-start
		coordinator.Shutdown()
		close(shutdownDone)
	}()
	close(start)
	callers.Wait()
	select {
	case <-shutdownDone:
	case <-time.After(time.Second):
		t.Fatal("concurrent worker admission prevented shutdown drain")
	}
	if coordinator.startWorker(func(context.Context) {}) {
		t.Fatal("worker admitted after concurrent shutdown")
	}
}

type cancelReconcileStore struct {
	*fakeStore
	entered sync.Once
	started chan struct{}
	exited  chan struct{}
}

func (store *cancelReconcileStore) ListDesiredWorkloads(ctx context.Context) ([]farmmodel.DesiredWorkload, error) {
	store.entered.Do(func() { close(store.started) })
	<-ctx.Done()
	close(store.exited)
	return nil, ctx.Err()
}

func TestCoordinatorShutdownCancelsAndJoinsReconcileWorker(t *testing.T) {
	host := testHost(1)
	workloadID := testWorkload(1)
	snapshot := testSnapshot(workloadID, host, 1, testExecution(1), farmmodel.ResourceClaim{CPU: true})
	store := &cancelReconcileStore{
		fakeStore: newFakeStore(testWorkloadObject(workloadID, host, farmmodel.DesiredRunning, 1, snapshot.Resources), snapshot),
		started:   make(chan struct{}),
		exited:    make(chan struct{}),
	}
	observed := controllerstate.New()
	commander := newFakeCommander(host, 1)
	readyStore(observed, commander.info, nil)
	coordinator := NewCoordinator(store, observed, commander, nil)
	if !coordinator.scheduleReconcile(host) {
		t.Fatal("reconcile worker was not admitted")
	}
	select {
	case <-store.started:
	case <-time.After(time.Second):
		t.Fatal("reconcile worker did not enter persistent-state access")
	}
	coordinator.Shutdown()
	select {
	case <-store.exited:
	default:
		t.Fatal("Shutdown returned before reconcile persistent-state access exited")
	}
}

func TestCoordinatorOneInFlightStartAndLostResultReconnect(t *testing.T) {
	host := testHost(1)
	workloadID := testWorkload(1)
	snapshot := testSnapshot(workloadID, host, 1, testExecution(1), farmmodel.ResourceClaim{CPU: true})
	store := newFakeStore(testWorkloadObject(workloadID, host, farmmodel.DesiredRunning, 1, snapshot.Resources), snapshot)
	observed := controllerstate.New()
	commander := newFakeCommander(host, 1)
	coordinator := NewCoordinator(store, observed, commander, nil)
	readyStore(observed, commander.info, nil)
	for i := 0; i < 100; i++ {
		coordinator.ReconcileHost(context.Background(), host)
	}
	if got := commander.requestsCopy(); len(got) != 1 || got[0].Kind != controllernet.RuntimeStart {
		t.Fatalf("START requests=%+v", got)
	}
	// The Agent started, but the result was lost with the connection.
	coordinator.SessionDisconnected(commander.info)
	commander.setEpoch(2)
	readyStore(observed, commander.info, []model.ExecutionObservation{observedExecution(snapshot, model.ExecutionRunning)})
	coordinator.ReconcileHost(context.Background(), host)
	if got := commander.requestsCopy(); len(got) != 1 {
		t.Fatalf("lost START result caused duplicate: %+v", got)
	}
}

func TestUnmanagedGPUConflictBlocksThenFreshRemovalAllowsStart(t *testing.T) {
	host := testHost(1)
	device, _ := identity.ParseDeviceID("device_00000000000000000000000000000001")
	workloadID := testWorkload(1)
	snapshot := testSnapshot(workloadID, host, 1, testExecution(1), farmmodel.ResourceClaim{DeviceIDs: []identity.DeviceID{device}})
	store := newFakeStore(testWorkloadObject(workloadID, host, farmmodel.DesiredRunning, 1, snapshot.Resources), snapshot)
	observed := controllerstate.New()
	commander := newFakeCommander(host, 1)
	coordinator := NewCoordinator(store, observed, commander, nil)
	readyStoreWithInventory(observed, commander.info, nil, model.Inventory{Host: model.Host{HostID: host}, GPUs: []model.GPU{{DeviceID: device}}})
	conflict := model.UnmanagedProcessObservation{PID: 44, Executable: "gpu-miner", ProcessInstance: "linux-proc-start-ticks:9", GPURelevant: true, ResourceScope: model.ProcessResourcesExact, DeviceIDs: []identity.DeviceID{device}, ObservedAt: time.Now().UTC(), Evidence: []model.ProcessEvidence{{Kind: "KNOWN_ADAPTER_EXECUTABLE", Provider: "test"}}}
	observed.SetUnmanagedProcesses(host, 1, []model.UnmanagedProcessObservation{conflict}, time.Now())
	coordinator.ReconcileHost(context.Background(), host)
	if got := commander.requestsCopy(); len(got) != 0 {
		t.Fatalf("unmanaged conflict dispatched runtime action: %+v", got)
	}
	current, _ := observed.Get(host)
	if len(current.UnmanagedProcesses) != 1 || len(current.Executions) != 0 {
		t.Fatalf("unmanaged process was adopted: %+v", current)
	}
	observed.SetUnmanagedProcesses(host, 1, nil, time.Now())
	coordinator.ReconcileHost(context.Background(), host)
	if got := commander.requestsCopy(); len(got) != 1 || got[0].Kind != controllernet.RuntimeStart {
		t.Fatalf("fresh conflict removal did not permit START: %+v", got)
	}
}

func TestMaintenanceHoldPreservesDesiredStopsExactAndRequiresFreshExit(t *testing.T) {
	host := testHost(1)
	workloadID := testWorkload(1)
	snapshot := testSnapshot(workloadID, host, 1, testExecution(1), farmmodel.ResourceClaim{CPU: true})
	workload := testWorkloadObject(workloadID, host, farmmodel.DesiredRunning, 1, snapshot.Resources)
	store := newFakeStore(workload, snapshot)
	observed := controllerstate.New()
	commander := newFakeCommander(host, 1)
	coordinator := NewCoordinator(store, observed, commander, nil)
	readyStore(observed, commander.info, []model.ExecutionObservation{observedExecution(snapshot, model.ExecutionRunning)})

	hold, err := coordinator.SetMaintenanceHold(context.Background(), host, 0, true, "service hardware")
	if err != nil || !hold.Active || hold.Revision != 1 {
		t.Fatalf("enter hold=%+v err=%v", hold, err)
	}
	waitRequests(t, commander, 1)
	requests := commander.requestsCopy()
	if requests[0].Kind != controllernet.RuntimeStop || requests[0].ExecutionID != snapshot.ExecutionID {
		t.Fatalf("hold did not exact-stop managed execution: %+v", requests)
	}
	if current, _ := store.GetDesiredWorkload(context.Background(), workloadID); current.RunState != farmmodel.DesiredRunning || current.DesiredGeneration != workload.DesiredGeneration {
		t.Fatalf("hold rewrote Desired intent: %+v", current)
	}
	for range 10 {
		coordinator.ReconcileHost(context.Background(), host)
	}
	if len(commander.requestsCopy()) != 1 {
		t.Fatalf("hold caused duplicate STOP/START: %+v", commander.requestsCopy())
	}
	released, err := coordinator.SetMaintenanceHold(context.Background(), host, 1, false, "")
	if err != nil || released.Active || released.Revision != 2 {
		t.Fatalf("release hold=%+v err=%v", released, err)
	}
	coordinator.ReconcileHost(context.Background(), host)
	if len(commander.requestsCopy()) != 1 {
		t.Fatal("hold exit STARTed before fresh bootstrap")
	}
	coordinator.ExecutionsObserved(commander.info, nil, 1)
	coordinator.InventoryObserved(commander.info, model.Inventory{Host: model.Host{HostID: host}})
	coordinator.ProcessesObserved(commander.info, nil)
	coordinator.StatusObserved(commander.info, model.AgentStateIdle)
	waitRequests(t, commander, 2)
	if got := commander.requestsCopy()[1]; got.Kind != controllernet.RuntimeStart || got.ExecutionID != snapshot.ExecutionID {
		t.Fatalf("fresh hold exit did not resume normal reconcile: %+v", got)
	}
	if got := commander.holdsCopy(); len(got) != 2 || !got[0].active || got[0].revision != 1 || got[1].active || got[1].revision != 2 {
		t.Fatalf("Agent hold commands=%+v", got)
	}
}

func TestMaintenanceHoldSuppressesInitialAndReconnectReconcile(t *testing.T) {
	host := testHost(1)
	workloadID := testWorkload(1)
	snapshot := testSnapshot(workloadID, host, 1, testExecution(1), farmmodel.ResourceClaim{CPU: true})
	store := newFakeStore(testWorkloadObject(workloadID, host, farmmodel.DesiredRunning, 1, snapshot.Resources), snapshot)
	store.holds[host] = farmmodel.MaintenanceHold{HostID: host, Active: true, Revision: 4}
	observed := controllerstate.New()
	commander := newFakeCommander(host, 1)
	coordinator := NewCoordinator(store, observed, commander, nil)
	readyStore(observed, commander.info, nil)
	coordinator.ReconcileHost(context.Background(), host)
	coordinator.SessionDisconnected(commander.info)
	commander.setEpoch(2)
	readyStore(observed, commander.info, nil)
	coordinator.ReconcileHost(context.Background(), host)
	if got := commander.requestsCopy(); len(got) != 0 {
		t.Fatalf("hold allowed START across reconnect: %+v", got)
	}
}

func TestRestoreBarrierRequiresFreshBootstrapBeforeNormalStart(t *testing.T) {
	host := testHost(1)
	workloadID := testWorkload(1)
	snapshot := testSnapshot(workloadID, host, 1, testExecution(1), farmmodel.ResourceClaim{CPU: true})
	store := newFakeStore(testWorkloadObject(workloadID, host, farmmodel.DesiredRunning, 1, snapshot.Resources), snapshot)
	store.restoreBarriers[host] = testRestoreBarrier(host)
	observed := controllerstate.New()
	commander := newFakeCommander(host, 1)
	coordinator := NewCoordinator(store, observed, commander, nil)
	coordinator.SessionConnected(commander.info)
	coordinator.ReconcileHost(context.Background(), host)
	if len(commander.requestsCopy()) != 0 {
		t.Fatal("restore dispatched before fresh bootstrap")
	}
	if _, exists := store.restoreBarrier(host); !exists {
		t.Fatal("incomplete bootstrap cleared restore barrier")
	}
	readyStore(observed, commander.info, nil)
	coordinator.ReconcileHost(context.Background(), host)
	waitRequests(t, commander, 1)
	if got := commander.requestsCopy()[0]; got.Kind != controllernet.RuntimeStart || got.ExecutionID != snapshot.ExecutionID {
		t.Fatalf("safe post-comparison reconcile=%+v", got)
	}
}

func TestRestoreBarrierExactCurrentExecutionAvoidsDuplicateStart(t *testing.T) {
	host := testHost(1)
	workloadID := testWorkload(1)
	snapshot := testSnapshot(workloadID, host, 1, testExecution(1), farmmodel.ResourceClaim{CPU: true})
	store := newFakeStore(testWorkloadObject(workloadID, host, farmmodel.DesiredRunning, 1, snapshot.Resources), snapshot)
	store.restoreBarriers[host] = testRestoreBarrier(host)
	observed := controllerstate.New()
	commander := newFakeCommander(host, 1)
	coordinator := NewCoordinator(store, observed, commander, nil)
	readyStore(observed, commander.info, []model.ExecutionObservation{observedExecution(snapshot, model.ExecutionRunning)})
	coordinator.ReconcileHost(context.Background(), host)
	if _, exists := store.restoreBarrier(host); exists {
		t.Fatal("exact current execution did not clear restore barrier")
	}
	for range 10 {
		coordinator.ReconcileHost(context.Background(), host)
	}
	if got := commander.requestsCopy(); len(got) != 0 {
		t.Fatalf("exact current execution was duplicate-started: %+v", got)
	}
}

func TestRestoreBarrierNewerPhysicalRealityBlocksWithoutStopOrStart(t *testing.T) {
	host := testHost(1)
	workloadID := testWorkload(1)
	restored := testSnapshot(workloadID, host, 1, testExecution(1), farmmodel.ResourceClaim{CPU: true})
	newer := testSnapshot(workloadID, host, 2, testExecution(2), farmmodel.ResourceClaim{CPU: true})
	store := newFakeStore(testWorkloadObject(workloadID, host, farmmodel.DesiredRunning, 1, restored.Resources), restored)
	store.restoreBarriers[host] = testRestoreBarrier(host)
	observed := controllerstate.New()
	commander := newFakeCommander(host, 1)
	coordinator := NewCoordinator(store, observed, commander, nil)
	readyStore(observed, commander.info, []model.ExecutionObservation{observedExecution(newer, model.ExecutionRunning)})
	coordinator.ReconcileHost(context.Background(), host)
	barrier, _ := store.restoreBarrier(host)
	if barrier.Status != farmmodel.RestoreBarrierConflict || barrier.ReasonCode != farmerr.RESTORE_RECONCILIATION_REQUIRED {
		t.Fatalf("newer physical reality barrier=%+v", barrier)
	}
	if got := commander.requestsCopy(); len(got) != 0 {
		t.Fatalf("restore touched newer physical reality: %+v", got)
	}
}

func TestRestoreBarrierPreservesHoldAndUnmanagedStartSafety(t *testing.T) {
	for _, test := range []struct {
		name      string
		configure func(*fakeStore, *controllerstate.Store, identity.HostID)
	}{
		{"maintenance-hold", func(store *fakeStore, _ *controllerstate.Store, host identity.HostID) {
			store.holds[host] = farmmodel.MaintenanceHold{HostID: host, Active: true, Revision: 1}
		}},
		{"unmanaged-conflict", func(_ *fakeStore, observed *controllerstate.Store, host identity.HostID) {
			observed.SetUnmanagedProcesses(host, 1, []model.UnmanagedProcessObservation{{PID: 40, Executable: "known-miner", ProcessInstance: "instance:1", CPURelevant: true, CPU: true, ResourceScope: model.ProcessResourcesExact}}, time.Now().UTC())
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			host := testHost(1)
			workloadID := testWorkload(1)
			snapshot := testSnapshot(workloadID, host, 1, testExecution(1), farmmodel.ResourceClaim{CPU: true})
			store := newFakeStore(testWorkloadObject(workloadID, host, farmmodel.DesiredRunning, 1, snapshot.Resources), snapshot)
			store.restoreBarriers[host] = testRestoreBarrier(host)
			observed := controllerstate.New()
			commander := newFakeCommander(host, 1)
			readyStore(observed, commander.info, nil)
			test.configure(store, observed, host)
			coordinator := NewCoordinator(store, observed, commander, nil)
			for range 5 {
				coordinator.ReconcileHost(context.Background(), host)
			}
			if _, exists := store.restoreBarrier(host); exists {
				t.Fatal("safe comparison did not clear restore barrier")
			}
			if got := commander.requestsCopy(); len(got) != 0 {
				t.Fatalf("restored safety condition allowed START: %+v", got)
			}
		})
	}
}

func TestRestoreBarrierDoesNotMigrateMissingGPU(t *testing.T) {
	host := testHost(1)
	device, _ := identity.ParseDeviceID("device_0123456789abcdef0123456789abcdef")
	workloadID := testWorkload(1)
	snapshot := testSnapshot(workloadID, host, 1, testExecution(1), farmmodel.ResourceClaim{DeviceIDs: []identity.DeviceID{device}})
	store := newFakeStore(testWorkloadObject(workloadID, host, farmmodel.DesiredRunning, 1, snapshot.Resources), snapshot)
	store.restoreBarriers[host] = testRestoreBarrier(host)
	observed := controllerstate.New()
	commander := newFakeCommander(host, 1)
	readyStoreWithInventory(observed, commander.info, nil, model.Inventory{Host: model.Host{HostID: host}})
	coordinator := NewCoordinator(store, observed, commander, nil)
	for range 5 {
		coordinator.ReconcileHost(context.Background(), host)
	}
	if got := commander.requestsCopy(); len(got) != 0 {
		t.Fatalf("missing restored DeviceID was silently migrated or started: %+v", got)
	}
}

func testRestoreBarrier(host identity.HostID) farmmodel.RestoreHostBarrier {
	return farmmodel.RestoreHostBarrier{HostID: host, BackupID: "backup_0123456789abcdef0123456789abcdef", Revision: 1, Status: farmmodel.RestoreBarrierPending, ReasonCode: farmerr.RESTORE_RECONCILIATION_REQUIRED, UpdatedAt: time.Now().UTC()}
}

func TestRuntimeResultTransitionKeepsHostIneligibleUntilBlockPersists(t *testing.T) {
	host := testHost(1)
	workloadID := testWorkload(1)
	snapshot := testSnapshot(workloadID, host, 1, testExecution(1), farmmodel.ResourceClaim{CPU: true})
	store := newFakeStore(testWorkloadObject(workloadID, host, farmmodel.DesiredRunning, 1, snapshot.Resources), snapshot)
	store.blockEntered = make(chan struct{}, 1)
	store.blockRelease = make(chan struct{})
	observed := controllerstate.New()
	commander := newFakeCommander(host, 1)
	coordinator := NewCoordinator(store, observed, commander, nil)
	readyStore(observed, commander.info, nil)
	coordinator.ReconcileHost(context.Background(), host)
	request := commander.requestsCopy()[0]

	resultDone := make(chan struct{})
	go func() {
		failed := observedExecution(snapshot, model.ExecutionFailed)
		coordinator.RuntimeResult(commander.info, request, &failed, nil)
		close(resultDone)
	}()
	<-store.blockEntered

	reconcileDone := make(chan struct{})
	go func() {
		coordinator.ReconcileHost(context.Background(), host)
		close(reconcileDone)
	}()
	select {
	case <-reconcileDone:
		t.Fatal("reconcile crossed the result transition before the blocked binding persisted")
	case <-time.After(20 * time.Millisecond):
	}
	close(store.blockRelease)
	<-resultDone
	<-reconcileDone
	if len(commander.requestsCopy()) != 1 || !store.isBlocked(workloadID, 1) {
		t.Fatal("result transition released duplicate START eligibility")
	}
}

func TestDeleteCannotUseObservationPredatingUnresolvedStart(t *testing.T) {
	host := testHost(1)
	workloadID := testWorkload(1)
	snapshot := testSnapshot(workloadID, host, 1, testExecution(1), farmmodel.ResourceClaim{CPU: true})
	workload := testWorkloadObject(workloadID, host, farmmodel.DesiredRunning, 1, snapshot.Resources)
	store := newFakeStore(workload, snapshot)
	observed := controllerstate.New()
	base := newFakeCommander(host, 1)
	commander := &barrierCommander{fakeCommander: base, entered: make(chan struct{}, 1), release: make(chan struct{})}
	coordinator := NewCoordinator(store, observed, commander, nil)
	readyStore(observed, base.info, nil)

	reconcileDone := make(chan struct{})
	go func() {
		coordinator.ReconcileHost(context.Background(), host)
		close(reconcileDone)
	}()
	<-commander.entered
	stopped := workload
	stopped.RunState = farmmodel.DesiredStopped
	stopped.Meta.Revision++
	store.replace(stopped, snapshot)
	if err := coordinator.DeleteDesiredWorkload(context.Background(), workloadID, stopped.Meta.Revision); codeOfReconcile(err) != farmerr.REFERENCE_IN_USE {
		t.Fatalf("deletion crossed unresolved START: %v", err)
	}
	close(commander.release)
	<-reconcileDone

	started := observedExecution(snapshot, model.ExecutionRunning)
	coordinator.RuntimeResult(base.info, base.requestsCopy()[0], &started, nil)
	if err := coordinator.DeleteDesiredWorkload(context.Background(), workloadID, stopped.Meta.Revision); codeOfReconcile(err) != farmerr.REFERENCE_IN_USE {
		t.Fatalf("deletion orphaned completed START: %v", err)
	}
}

func TestMalformedRuntimeResultRequiresCausallyLaterReality(t *testing.T) {
	host := testHost(1)
	workloadID := testWorkload(1)
	snapshot := testSnapshot(workloadID, host, 1, testExecution(1), farmmodel.ResourceClaim{CPU: true})
	store := newFakeStore(testWorkloadObject(workloadID, host, farmmodel.DesiredRunning, 1, snapshot.Resources), snapshot)
	observed := controllerstate.New()
	commander := newFakeCommander(host, 1)
	coordinator := NewCoordinator(store, observed, commander, nil)
	readyStore(observed, commander.info, nil)
	coordinator.ReconcileHost(context.Background(), host)
	request := commander.requestsCopy()[0]
	malformed := &farmerr.Error{Code: farmerr.INTERNAL_ERROR, HumanMessage: "malformed runtime result"}
	coordinator.RuntimeResult(commander.info, request, nil, malformed)
	for range 10 {
		coordinator.ReconcileHost(context.Background(), host)
	}
	if len(commander.requestsCopy()) != 1 {
		t.Fatal("malformed result caused blind START retry")
	}

	// This snapshot was ordered before dispatch sequence 1 and cannot release it.
	coordinator.ExecutionsObserved(commander.info, nil, 0)
	coordinator.ReconcileHost(context.Background(), host)
	if len(commander.requestsCopy()) != 1 {
		t.Fatal("pre-dispatch observation released uncertain START")
	}

	// A causally later empty snapshot proves that exact ExecutionID absent, so
	// level-triggered retry of the same persisted execution is safe.
	coordinator.ExecutionsObserved(commander.info, nil, 1)
	waitRequests(t, commander, 2)
	if got := commander.requestsCopy(); got[1].Kind != controllernet.RuntimeStart || got[1].ExecutionID != snapshot.ExecutionID {
		t.Fatalf("fresh reality recovery=%+v", got)
	}
}

func TestPreDispatchSnapshotCannotEraseIncorporatedRuntimeResult(t *testing.T) {
	host := testHost(1)
	workloadID := testWorkload(1)
	snapshot := testSnapshot(workloadID, host, 1, testExecution(1), farmmodel.ResourceClaim{CPU: true})
	store := newFakeStore(testWorkloadObject(workloadID, host, farmmodel.DesiredRunning, 1, snapshot.Resources), snapshot)
	observed := controllerstate.New()
	commander := newFakeCommander(host, 1)
	coordinator := NewCoordinator(store, observed, commander, nil)
	readyStore(observed, commander.info, nil)
	coordinator.ReconcileHost(context.Background(), host)
	request := commander.requestsCopy()[0]
	started := observedExecution(snapshot, model.ExecutionRunning)
	coordinator.RuntimeResult(commander.info, request, &started, nil)

	// This GET_EXECUTIONS request was issued before dispatch sequence 1. Its
	// empty response must not erase the newer START result and enable a duplicate.
	coordinator.ExecutionsObserved(commander.info, nil, 0)
	for range 10 {
		coordinator.ReconcileHost(context.Background(), host)
	}
	if requests := commander.requestsCopy(); len(requests) != 1 {
		t.Fatalf("pre-dispatch snapshot enabled duplicate START: %+v", requests)
	}
	current, _ := observed.Get(host)
	if len(current.Executions) != 1 || current.Executions[0].ExecutionID != snapshot.ExecutionID {
		t.Fatalf("incorporated runtime fact was erased: %+v", current.Executions)
	}
}

func TestCausalRealityArrivingBeforeSendReturnsReleasesGuard(t *testing.T) {
	host := testHost(1)
	workloadID := testWorkload(1)
	snapshot := testSnapshot(workloadID, host, 1, testExecution(1), farmmodel.ResourceClaim{CPU: true})
	store := newFakeStore(testWorkloadObject(workloadID, host, farmmodel.DesiredRunning, 1, snapshot.Resources), snapshot)
	observed := controllerstate.New()
	base := newFakeCommander(host, 1)
	commander := &barrierCommander{fakeCommander: base, entered: make(chan struct{}, 1), release: make(chan struct{})}
	coordinator := NewCoordinator(store, observed, commander, nil)
	readyStore(observed, base.info, nil)

	done := make(chan struct{})
	go func() {
		coordinator.ReconcileHost(context.Background(), host)
		close(done)
	}()
	<-commander.entered
	// The Agent's causally later empty reality is processed before the bounded
	// SendRuntime call returns to the coordinator.
	coordinator.ExecutionsObserved(base.info, nil, 1)
	close(commander.release)
	<-done
	coordinator.ReconcileHost(context.Background(), host)
	if requests := base.requestsCopy(); len(requests) != 2 || requests[1].ExecutionID != snapshot.ExecutionID {
		t.Fatalf("causal reality did not recover unresolved dispatch: %+v", requests)
	}
}

func TestPlannedActionIsRevalidatedImmediatelyBeforeDispatch(t *testing.T) {
	host := testHost(1)
	workloadID := testWorkload(1)
	snapshot := testSnapshot(workloadID, host, 1, testExecution(1), farmmodel.ResourceClaim{CPU: true})
	workload := testWorkloadObject(workloadID, host, farmmodel.DesiredRunning, 1, snapshot.Resources)
	store := newFakeStore(workload, snapshot)
	observed := controllerstate.New()
	base := newFakeCommander(host, 1)
	commander := &mutatingSessionCommander{fakeCommander: base, mutate: func() {
		stopped := workload
		stopped.RunState = farmmodel.DesiredStopped
		stopped.Meta.Revision++
		store.replace(stopped, snapshot)
	}}
	coordinator := NewCoordinator(store, observed, commander, nil)
	readyStore(observed, base.info, nil)
	coordinator.ReconcileHost(context.Background(), host)
	if requests := base.requestsCopy(); len(requests) != 0 {
		t.Fatalf("stale planned action dispatched after desired mutation: %+v", requests)
	}
}

func TestChangedObservationInvalidatesActionBeforeDispatch(t *testing.T) {
	host := testHost(1)
	workloadID := testWorkload(1)
	snapshot := testSnapshot(workloadID, host, 1, testExecution(1), farmmodel.ResourceClaim{CPU: true})
	store := newFakeStore(testWorkloadObject(workloadID, host, farmmodel.DesiredRunning, 1, snapshot.Resources), snapshot)
	observed := controllerstate.New()
	base := newFakeCommander(host, 1)
	commander := &mutatingSessionCommander{fakeCommander: base, mutate: func() {
		unknown := unmanagedObservation(host, testWorkload(9), testExecution(9), farmmodel.ResourceClaim{CPU: true})
		observed.SetExecutions(host, base.info.ConnectionEpoch, []model.ExecutionObservation{unknown}, time.Now(), 0)
	}}
	coordinator := NewCoordinator(store, observed, commander, nil)
	readyStore(observed, base.info, nil)
	coordinator.ReconcileHost(context.Background(), host)
	if requests := base.requestsCopy(); len(requests) != 0 {
		t.Fatalf("action dispatched after conflicting observation changed: %+v", requests)
	}
}

func TestChangedInventoryInvalidatesStartBeforeDispatch(t *testing.T) {
	host := testHost(1)
	workloadID := testWorkload(1)
	snapshot := testSnapshot(workloadID, host, 1, testExecution(1), farmmodel.ResourceClaim{CPU: true})
	store := newFakeStore(testWorkloadObject(workloadID, host, farmmodel.DesiredRunning, 1, snapshot.Resources), snapshot)
	observed := controllerstate.New()
	base := newFakeCommander(host, 1)
	commander := &mutatingSessionCommander{fakeCommander: base, mutate: func() {
		observed.SetInventory(host, base.info.ConnectionEpoch, model.Inventory{
			Host: model.Host{HostID: host},
			CPU:  model.CPU{Threads: 8},
		})
	}}
	coordinator := NewCoordinator(store, observed, commander, nil)
	readyStore(observed, base.info, nil)
	coordinator.ReconcileHost(context.Background(), host)
	if requests := base.requestsCopy(); len(requests) != 0 {
		t.Fatalf("START dispatched after validated inventory changed: %+v", requests)
	}
}

func TestEquivalentObservationRefreshDoesNotStarveDispatch(t *testing.T) {
	host := testHost(1)
	workloadID := testWorkload(1)
	snapshot := testSnapshot(workloadID, host, 1, testExecution(1), farmmodel.ResourceClaim{CPU: true})
	store := newFakeStore(testWorkloadObject(workloadID, host, farmmodel.DesiredRunning, 1, snapshot.Resources), snapshot)
	observed := controllerstate.New()
	base := newFakeCommander(host, 1)
	commander := &mutatingSessionCommander{fakeCommander: base, mutate: func() {
		observed.SetExecutions(host, base.info.ConnectionEpoch, nil, time.Now(), 0)
		observed.SetInventory(host, base.info.ConnectionEpoch, model.Inventory{Host: model.Host{HostID: host}})
		observed.SetAgentState(host, base.info.ConnectionEpoch, model.AgentStateIdle)
	}}
	coordinator := NewCoordinator(store, observed, commander, nil)
	readyStore(observed, base.info, nil)
	coordinator.ReconcileHost(context.Background(), host)
	if requests := base.requestsCopy(); len(requests) != 1 || requests[0].Kind != controllernet.RuntimeStart {
		t.Fatalf("equivalent refresh starved dispatch: %+v", requests)
	}
}

func TestCoordinatorLostStopResultProceedsOnlyAfterFreshAbsence(t *testing.T) {
	host := testHost(1)
	workloadID := testWorkload(1)
	old := testSnapshot(workloadID, host, 1, testExecution(1), farmmodel.ResourceClaim{CPU: true})
	current := testSnapshot(workloadID, host, 2, testExecution(2), farmmodel.ResourceClaim{CPU: true})
	store := newFakeStore(testWorkloadObject(workloadID, host, farmmodel.DesiredRunning, 2, current.Resources), old, current)
	observed := controllerstate.New()
	commander := newFakeCommander(host, 1)
	coordinator := NewCoordinator(store, observed, commander, nil)
	readyStore(observed, commander.info, []model.ExecutionObservation{observedExecution(old, model.ExecutionRunning)})
	coordinator.ReconcileHost(context.Background(), host)
	requests := commander.requestsCopy()
	if len(requests) != 1 || requests[0].Kind != controllernet.RuntimeStop || requests[0].ExecutionID != old.ExecutionID {
		t.Fatalf("old execution was not stopped first: %+v", requests)
	}
	coordinator.SessionDisconnected(commander.info)
	commander.setEpoch(2)
	readyStore(observed, commander.info, nil)
	coordinator.ReconcileHost(context.Background(), host)
	requests = commander.requestsCopy()
	if len(requests) != 2 || requests[1].Kind != controllernet.RuntimeStart || requests[1].ExecutionID != current.ExecutionID {
		t.Fatalf("fresh absence did not start replacement: %+v", requests)
	}
}

func TestHostSettingsReplacementStopsOldBeforeStartingNew(t *testing.T) {
	host := testHost(1)
	workloadID := testWorkload(1)
	old := testSnapshot(workloadID, host, 1, testExecution(1), farmmodel.ResourceClaim{CPU: true})
	current := testSnapshot(workloadID, host, 2, testExecution(2), farmmodel.ResourceClaim{CPU: true})
	oldThreads, newThreads := uint32(30), uint32(28)
	old.Plan.CPUThreads = &oldThreads
	current.Plan.CPUThreads = &newThreads
	store := newFakeStore(testWorkloadObject(workloadID, host, farmmodel.DesiredRunning, 2, current.Resources), old, current)
	observed := controllerstate.New()
	commander := newFakeCommander(host, 1)
	coordinator := NewCoordinator(store, observed, commander, nil)
	readyStore(observed, commander.info, []model.ExecutionObservation{observedExecution(old, model.ExecutionRunning)})
	coordinator.ReconcileHost(context.Background(), host)
	requests := commander.requestsCopy()
	if len(requests) != 1 || requests[0].Kind != controllernet.RuntimeStop || requests[0].ExecutionID != old.ExecutionID {
		t.Fatalf("settings replacement did not STOP old exactly: %+v", requests)
	}
	for range 20 {
		coordinator.ReconcileHost(context.Background(), host)
	}
	if len(commander.requestsCopy()) != 1 {
		t.Fatal("new execution overlapped unresolved old STOP")
	}
	stopped := observedExecution(old, model.ExecutionStopped)
	coordinator.RuntimeResult(commander.info, requests[0], &stopped, nil)
	coordinator.ExecutionsObserved(commander.info, nil, requests[0].DispatchSequence)
	waitRequests(t, commander, 2)
	requests = commander.requestsCopy()
	if requests[1].Kind != controllernet.RuntimeStart || requests[1].ExecutionID != current.ExecutionID {
		t.Fatalf("confirmed absence did not START new exactly: %+v", requests)
	}
}

func TestGPUAssignmentChangeStopsOldBeforeStartingNew(t *testing.T) {
	host := testHost(1)
	workloadID := testWorkload(1)
	oldDevice, newDevice := testDevice(1), testDevice(2)
	old := testSnapshot(workloadID, host, 1, testExecution(1), farmmodel.ResourceClaim{DeviceIDs: []identity.DeviceID{oldDevice}})
	current := testSnapshot(workloadID, host, 2, testExecution(2), farmmodel.ResourceClaim{DeviceIDs: []identity.DeviceID{newDevice}})
	store := newFakeStore(testWorkloadObject(workloadID, host, farmmodel.DesiredRunning, 2, current.Resources), old, current)
	observed := controllerstate.New()
	commander := newFakeCommander(host, 1)
	coordinator := NewCoordinator(store, observed, commander, nil)
	readyStore(observed, commander.info, []model.ExecutionObservation{observedExecution(old, model.ExecutionRunning)})
	// Current inventory contains only the explicitly selected replacement GPU;
	// it is never substituted for the old claim by the planner.
	observed.SetInventory(host, commander.info.ConnectionEpoch, model.Inventory{Host: model.Host{HostID: host}, GPUs: []model.GPU{{DeviceID: newDevice, UUID: "gpu-new", PCIBusID: "02:00.0"}}})
	coordinator.ReconcileHost(context.Background(), host)
	requests := commander.requestsCopy()
	if len(requests) != 1 || requests[0].Kind != controllernet.RuntimeStop || requests[0].ExecutionID != old.ExecutionID {
		t.Fatalf("GPU replacement did not STOP old exactly: %+v", requests)
	}
	for range 20 {
		coordinator.ReconcileHost(context.Background(), host)
	}
	if len(commander.requestsCopy()) != 1 {
		t.Fatal("GPU replacement overlapped unresolved old STOP")
	}
	stopped := observedExecution(old, model.ExecutionStopped)
	coordinator.RuntimeResult(commander.info, requests[0], &stopped, nil)
	coordinator.ExecutionsObserved(commander.info, nil, requests[0].DispatchSequence)
	waitRequests(t, commander, 2)
	requests = commander.requestsCopy()
	if requests[1].Kind != controllernet.RuntimeStart || requests[1].ExecutionID != current.ExecutionID {
		t.Fatalf("confirmed GPU absence did not START new exactly: %+v", requests)
	}
}

func TestFreshHostValidationBlocksAndCanRecover(t *testing.T) {
	host := testHost(1)
	workloadID := testWorkload(1)
	snapshot := testSnapshot(workloadID, host, 1, testExecution(1), farmmodel.ResourceClaim{CPU: true})
	store := newFakeStore(testWorkloadObject(workloadID, host, farmmodel.DesiredRunning, 1, snapshot.Resources), snapshot)
	store.validationErr = farmerr.Error{Code: farmerr.INCOMPATIBLE_HARDWARE, HumanMessage: "requested 30, supported 16"}
	observed := controllerstate.New()
	commander := newFakeCommander(host, 1)
	coordinator := NewCoordinator(store, observed, commander, nil)
	readyStore(observed, commander.info, nil)
	for range 20 {
		coordinator.ReconcileHost(context.Background(), host)
	}
	if len(commander.requestsCopy()) != 0 || store.isBlocked(workloadID, 1) {
		t.Fatal("incompatible hardware dispatched or became terminally latched")
	}
	store.mu.Lock()
	store.validationErr = nil
	store.mu.Unlock()
	coordinator.ReconcileHost(context.Background(), host)
	if requests := commander.requestsCopy(); len(requests) != 1 || requests[0].Kind != controllernet.RuntimeStart {
		t.Fatalf("corrected validation did not recover: %+v", requests)
	}
}

func TestFreshExecutionsButStaleInventoryCannotStart(t *testing.T) {
	now := time.Unix(4_000, 0).UTC()
	host := testHost(1)
	workloadID := testWorkload(1)
	snapshot := testSnapshot(workloadID, host, 1, testExecution(1), farmmodel.ResourceClaim{CPU: true})
	store := newFakeStore(testWorkloadObject(workloadID, host, farmmodel.DesiredRunning, 1, snapshot.Resources), snapshot)
	observed := controllerstate.NewWithOptions(controllerstate.StoreOptions{Now: func() time.Time { return now }, FreshnessTimeout: 45 * time.Second})
	commander := newFakeCommander(host, 1)
	coordinator := NewCoordinator(store, observed, commander, nil)
	observed.Connect(commander.info.AgentID, host, 1)
	observed.SetExecutions(host, 1, nil, now, 0)
	observed.SetInventory(host, 1, model.Inventory{Host: model.Host{HostID: host}, CPU: model.CPU{Threads: 32}})
	observed.SetUnmanagedProcesses(host, 1, nil, now)
	observed.SetAgentState(host, 1, model.AgentStateIdle)
	observed.MarkReady(host, 1)
	now = now.Add(40 * time.Second)
	observed.SetExecutions(host, 1, nil, now, 0)
	now = now.Add(6 * time.Second)
	coordinator.ReconcileHost(context.Background(), host)
	if len(commander.requestsCopy()) != 0 {
		t.Fatal("START used stale inventory")
	}
	observed.SetInventory(host, 1, model.Inventory{Host: model.Host{HostID: host}, CPU: model.CPU{Threads: 16}})
	observed.SetUnmanagedProcesses(host, 1, nil, now)
	coordinator.ReconcileHost(context.Background(), host)
	if requests := commander.requestsCopy(); len(requests) != 1 || requests[0].Kind != controllernet.RuntimeStart {
		t.Fatalf("fresh inventory did not restore reconciliation: %+v", requests)
	}
}

func TestReconnectWithChangedHardwareRevalidatesBeforeStart(t *testing.T) {
	host := testHost(1)
	workloadID := testWorkload(1)
	snapshot := testSnapshot(workloadID, host, 1, testExecution(1), farmmodel.ResourceClaim{CPU: true})
	requested := uint32(30)
	snapshot.Plan.CPUThreads = &requested
	store := newFakeStore(testWorkloadObject(workloadID, host, farmmodel.DesiredRunning, 1, snapshot.Resources), snapshot)
	store.validationFunc = func(inventory model.Inventory) error {
		if inventory.CPU.Threads < requested {
			return farmerr.Error{Code: farmerr.INCOMPATIBLE_HARDWARE, HumanMessage: "fresh CPU capacity changed"}
		}
		return nil
	}
	observed := controllerstate.New()
	commander := newFakeCommander(host, 1)
	coordinator := NewCoordinator(store, observed, commander, nil)
	readyStoreWithInventory(observed, commander.info, nil, model.Inventory{Host: model.Host{HostID: host}, CPU: model.CPU{Threads: 32}})
	coordinator.ReconcileHost(context.Background(), host)
	if requests := commander.requestsCopy(); len(requests) != 1 {
		t.Fatalf("initial compatible START=%+v", requests)
	}
	coordinator.SessionDisconnected(commander.info)
	commander.setEpoch(2)
	readyStoreWithInventory(observed, commander.info, nil, model.Inventory{Host: model.Host{HostID: host}, CPU: model.CPU{Threads: 16}})
	for range 20 {
		coordinator.ReconcileHost(context.Background(), host)
	}
	if requests := commander.requestsCopy(); len(requests) != 1 {
		t.Fatalf("changed reconnect hardware allowed START: %+v", requests)
	}
}

func TestControllerRestartRecognizesRunningExecutionWithoutDuplicate(t *testing.T) {
	host := testHost(1)
	workloadID := testWorkload(1)
	snapshot := testSnapshot(workloadID, host, 1, testExecution(1), farmmodel.ResourceClaim{CPU: true})
	store := newFakeStore(testWorkloadObject(workloadID, host, farmmodel.DesiredRunning, 1, snapshot.Resources), snapshot)
	// New observed state/coordinator represents a restarted Controller. Only the
	// persisted desired object and snapshot survive.
	observed := controllerstate.New()
	commander := newFakeCommander(host, 1)
	coordinator := NewCoordinator(store, observed, commander, nil)
	readyStore(observed, commander.info, []model.ExecutionObservation{observedExecution(snapshot, model.ExecutionRunning)})
	for i := 0; i < 100; i++ {
		coordinator.ReconcileHost(context.Background(), host)
	}
	if len(commander.requestsCopy()) != 0 {
		t.Fatalf("Controller restart duplicated known execution: %+v", commander.requestsCopy())
	}
}

func TestCoordinatorMultipleIndependentWorkloadsDoNotStopEachOther(t *testing.T) {
	host := testHost(1)
	cpuID, gpuID := testWorkload(1), testWorkload(2)
	device := testDevice(1)
	cpuSnapshot := testSnapshot(cpuID, host, 1, testExecution(1), farmmodel.ResourceClaim{CPU: true})
	gpuSnapshot := testSnapshot(gpuID, host, 1, testExecution(2), farmmodel.ResourceClaim{DeviceIDs: []identity.DeviceID{device}})
	store := newFakeStore(testWorkloadObject(cpuID, host, farmmodel.DesiredRunning, 1, cpuSnapshot.Resources), cpuSnapshot, gpuSnapshot)
	store.addWorkload(testWorkloadObject(gpuID, host, farmmodel.DesiredRunning, 1, gpuSnapshot.Resources))
	observed := controllerstate.New()
	commander := newFakeCommander(host, 1)
	coordinator := NewCoordinator(store, observed, commander, nil)
	readyStore(observed, commander.info, []model.ExecutionObservation{observedExecution(gpuSnapshot, model.ExecutionRunning)})
	coordinator.ReconcileHost(context.Background(), host)
	requests := commander.requestsCopy()
	if len(requests) != 1 || requests[0].Kind != controllernet.RuntimeStart || requests[0].ExecutionID != cpuSnapshot.ExecutionID {
		t.Fatalf("independent workload affected: %+v", requests)
	}
}

func TestCoordinatorStaleGenerationAndEpochResultsAreSafe(t *testing.T) {
	host := testHost(1)
	workloadID := testWorkload(1)
	first := testSnapshot(workloadID, host, 1, testExecution(1), farmmodel.ResourceClaim{CPU: true})
	second := testSnapshot(workloadID, host, 2, testExecution(2), farmmodel.ResourceClaim{CPU: true})
	store := newFakeStore(testWorkloadObject(workloadID, host, farmmodel.DesiredRunning, 1, first.Resources), first)
	observed := controllerstate.New()
	commander := newFakeCommander(host, 1)
	coordinator := NewCoordinator(store, observed, commander, nil)
	readyStore(observed, commander.info, nil)
	coordinator.ReconcileHost(context.Background(), host)
	request := commander.requestsCopy()[0]
	store.replace(testWorkloadObject(workloadID, host, farmmodel.DesiredRunning, 2, second.Resources), first, second)
	coordinator.ReconcileHost(context.Background(), host)
	if len(commander.requestsCopy()) != 1 {
		t.Fatal("new generation START overlapped an uncertain old-generation START")
	}
	result := observedExecution(first, model.ExecutionRunning)
	coordinator.RuntimeResult(commander.info, request, &result, nil)
	waitRequests(t, commander, 2)
	requests := commander.requestsCopy()
	if requests[1].Kind != controllernet.RuntimeStop || requests[1].ExecutionID != first.ExecutionID {
		t.Fatalf("stale generation result affected newer intent unsafely: %+v", requests)
	}
	coordinator.SessionDisconnected(commander.info)
	oldInfo := commander.info
	commander.setEpoch(2)
	readyStore(observed, commander.info, nil)
	coordinator.RuntimeResult(oldInfo, request, &result, nil)
	currentObservation, _ := observed.Get(host)
	if len(currentObservation.Executions) != 0 || currentObservation.ConnectionEpoch != 2 {
		t.Fatalf("stale epoch result mutated current observation: %+v", currentObservation)
	}
}

func TestCoordinatorTerminalFailureAndDefiniteRejectionBlockGeneration(t *testing.T) {
	host := testHost(1)
	workloadID := testWorkload(1)
	snapshot := testSnapshot(workloadID, host, 1, testExecution(1), farmmodel.ResourceClaim{CPU: true})
	workload := testWorkloadObject(workloadID, host, farmmodel.DesiredRunning, 1, snapshot.Resources)
	t.Run("terminal failed", func(t *testing.T) {
		store := newFakeStore(workload, snapshot)
		observed := controllerstate.New()
		commander := newFakeCommander(host, 1)
		coordinator := NewCoordinator(store, observed, commander, nil)
		readyStore(observed, commander.info, []model.ExecutionObservation{observedExecution(snapshot, model.ExecutionFailed)})
		for i := 0; i < 100; i++ {
			coordinator.ReconcileHost(context.Background(), host)
		}
		if len(commander.requestsCopy()) != 0 || !store.isBlocked(workloadID, 1) {
			t.Fatal("terminal FAILED was retried or not blocked")
		}
	})
	t.Run("definite rejection", func(t *testing.T) {
		store := newFakeStore(workload, snapshot)
		observed := controllerstate.New()
		commander := newFakeCommander(host, 1)
		coordinator := NewCoordinator(store, observed, commander, nil)
		readyStore(observed, commander.info, nil)
		coordinator.ReconcileHost(context.Background(), host)
		request := commander.requestsCopy()[0]
		rejection := &farmerr.Error{Code: farmerr.CONFIG_CONFLICT, HumanMessage: "plan rejected"}
		coordinator.RuntimeResult(commander.info, request, nil, rejection)
		for i := 0; i < 100; i++ {
			coordinator.ReconcileHost(context.Background(), host)
		}
		if len(commander.requestsCopy()) != 1 || !store.isBlocked(workloadID, 1) {
			t.Fatal("definite rejection caused retry loop")
		}
	})
}

func TestCoordinatorSafeDeleteRequiresFreshStoppedProof(t *testing.T) {
	host := testHost(1)
	workloadID := testWorkload(1)
	snapshot := testSnapshot(workloadID, host, 1, testExecution(1), farmmodel.ResourceClaim{CPU: true})
	workload := testWorkloadObject(workloadID, host, farmmodel.DesiredStopped, 2, snapshot.Resources)
	workload.Meta.Revision = 2
	store := newFakeStore(workload, snapshot)
	observed := controllerstate.New()
	commander := newFakeCommander(host, 1)
	coordinator := NewCoordinator(store, observed, commander, nil)
	if err := coordinator.DeleteDesiredWorkload(context.Background(), workloadID, 2); codeOfReconcile(err) != farmerr.SERVICE_NOT_READY {
		t.Fatalf("offline historical workload deletion error=%v", err)
	}
	readyStore(observed, commander.info, []model.ExecutionObservation{observedExecution(snapshot, model.ExecutionRunning)})
	if err := coordinator.DeleteDesiredWorkload(context.Background(), workloadID, 2); codeOfReconcile(err) != farmerr.REFERENCE_IN_USE {
		t.Fatalf("active owned execution did not block deletion: %v", err)
	}
	observed.SetExecutions(host, commander.info.ConnectionEpoch, nil, time.Now(), 0)
	if err := coordinator.DeleteDesiredWorkload(context.Background(), workloadID, 2); err != nil {
		t.Fatal(err)
	}
	if store.hasWorkload(workloadID) || len(store.snapshotCopy()) != 1 {
		t.Fatal("safe deletion removed history or retained desired workload")
	}
}

func TestCoordinatorHalfOpenObservationCannotDriveRuntimeAction(t *testing.T) {
	now := time.Unix(2_000, 0).UTC()
	host := testHost(1)
	workloadID := testWorkload(1)
	snapshot := testSnapshot(workloadID, host, 1, testExecution(1), farmmodel.ResourceClaim{CPU: true})
	workload := testWorkloadObject(workloadID, host, farmmodel.DesiredRunning, 1, snapshot.Resources)
	store := newFakeStore(workload, snapshot)
	observed := controllerstate.NewWithOptions(controllerstate.StoreOptions{Now: func() time.Time { return now }, FreshnessTimeout: 45 * time.Second})
	commander := newFakeCommander(host, 1)
	coordinator := NewCoordinator(store, observed, commander, nil)
	observed.Connect(commander.info.AgentID, host, commander.info.ConnectionEpoch)
	observed.SetExecutions(host, commander.info.ConnectionEpoch, []model.ExecutionObservation{observedExecution(snapshot, model.ExecutionRunning)}, now, 0)
	observed.SetInventory(host, commander.info.ConnectionEpoch, model.Inventory{Host: model.Host{HostID: host}})
	observed.SetUnmanagedProcesses(host, commander.info.ConnectionEpoch, nil, now)
	observed.SetAgentState(host, commander.info.ConnectionEpoch, model.AgentStateMining)
	if !observed.MarkReady(host, commander.info.ConnectionEpoch) {
		t.Fatal("bootstrap did not become READY")
	}

	// The stream remains logically connected, but no heartbeat-triggered
	// execution refresh arrives. A desired STOP would require a STOP if the old
	// fact were still trusted.
	now = now.Add(46 * time.Second)
	stopped := workload
	stopped.RunState = farmmodel.DesiredStopped
	store.replace(stopped, snapshot)
	for range 100 {
		coordinator.ReconcileHost(context.Background(), host)
	}
	if requests := commander.requestsCopy(); len(requests) != 0 {
		t.Fatalf("stale half-open observation drove runtime actions: %+v", requests)
	}
	if got, _ := observed.Get(host); got.Fresh || !got.Connected || !got.Ready {
		t.Fatalf("stale observation state=%+v", got)
	}

	// A valid current-epoch GET_EXECUTIONS result re-establishes the fact and
	// permits the exact STOP required by current desired state.
	coordinator.ExecutionsObserved(commander.info, []model.ExecutionObservation{observedExecution(snapshot, model.ExecutionRunning)}, 0)
	coordinator.ReconcileHost(context.Background(), host)
	requests := commander.requestsCopy()
	if len(requests) != 1 || requests[0].Kind != controllernet.RuntimeStop || requests[0].ExecutionID != snapshot.ExecutionID {
		t.Fatalf("fresh recovery did not produce one exact STOP: %+v", requests)
	}
}

func TestUnknownExecutionIsReportedButNeverAdoptedOrStopped(t *testing.T) {
	host := testHost(1)
	workloadID := testWorkload(1)
	snapshot := testSnapshot(workloadID, host, 1, testExecution(1), farmmodel.ResourceClaim{CPU: true})
	workload := testWorkloadObject(workloadID, host, farmmodel.DesiredStopped, 2, snapshot.Resources)
	store := newFakeStore(workload, snapshot)
	observed := controllerstate.New()
	commander := newFakeCommander(host, 1)
	coordinator := NewCoordinator(store, observed, commander, nil)
	unknown := unmanagedObservation(host, testWorkload(9), testExecution(9), farmmodel.ResourceClaim{CPU: true})
	unknown.MinerTelemetry = &model.MinerTelemetry{Health: model.MinerHealthHealthy}
	readyStore(observed, commander.info, []model.ExecutionObservation{unknown})
	coordinator.ReconcileHost(context.Background(), host)
	if len(commander.requestsCopy()) != 0 {
		t.Fatalf("unmanaged execution received a runtime action: %+v", commander.requestsCopy())
	}
	items, err := coordinator.UnmanagedExecutions(context.Background(), host)
	if err != nil || len(items) != 1 || items[0].ExecutionID != unknown.ExecutionID || items[0].ResourceClaim == nil || !items[0].ResourceClaim.CPU || items[0].MinerStatus == nil {
		t.Fatalf("unmanaged report=%+v err=%v", items, err)
	}
}

func readyStore(store *controllerstate.Store, info controllernet.SessionInfo, executions []model.ExecutionObservation) {
	readyStoreWithInventory(store, info, executions, model.Inventory{Host: model.Host{HostID: info.HostID}})
}

func readyStoreWithInventory(store *controllerstate.Store, info controllernet.SessionInfo, executions []model.ExecutionObservation, inventory model.Inventory) {
	store.Connect(info.AgentID, info.HostID, info.ConnectionEpoch)
	store.SetExecutions(info.HostID, info.ConnectionEpoch, executions, time.Now(), 0)
	store.SetInventory(info.HostID, info.ConnectionEpoch, inventory)
	store.SetUnmanagedProcesses(info.HostID, info.ConnectionEpoch, nil, time.Now())
	store.SetAgentState(info.HostID, info.ConnectionEpoch, model.AgentStateIdle)
	store.MarkReady(info.HostID, info.ConnectionEpoch)
}

func observedExecution(snapshot farmmodel.ResolvedExecutionSnapshot, state model.ExecutionStatus) model.ExecutionObservation {
	owner := snapshot.Plan.Ownership
	return model.ExecutionObservation{ExecutionID: snapshot.ExecutionID, Ownership: &owner, Status: state}
}

type fakeCommander struct {
	mu       sync.Mutex
	info     controllernet.SessionInfo
	requests []controllernet.RuntimeRequest
	holds    []fakeHoldCommand
}

type fakeHoldCommand struct {
	active   bool
	revision uint64
}

type barrierCommander struct {
	*fakeCommander
	entered chan struct{}
	release chan struct{}
}

type mutatingSessionCommander struct {
	*fakeCommander
	mutate func()
	once   sync.Once
}

func (commander *mutatingSessionCommander) Session(host identity.HostID) (controllernet.SessionInfo, bool) {
	info, ok := commander.fakeCommander.Session(host)
	commander.once.Do(commander.mutate)
	return info, ok
}

func (commander *barrierCommander) SendRuntime(ctx context.Context, info controllernet.SessionInfo, request controllernet.RuntimeRequest) (uint64, error) {
	sequence, err := commander.fakeCommander.SendRuntime(ctx, info, request)
	if err != nil {
		return 0, err
	}
	commander.entered <- struct{}{}
	select {
	case <-commander.release:
		return sequence, nil
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

func newFakeCommander(host identity.HostID, epoch controllerstate.ConnectionEpoch) *fakeCommander {
	return &fakeCommander{info: controllernet.SessionInfo{AgentID: testAgent(1), HostID: host, ConnectionEpoch: epoch, Authenticated: true, Ready: true}}
}
func (commander *fakeCommander) Session(host identity.HostID) (controllernet.SessionInfo, bool) {
	commander.mu.Lock()
	defer commander.mu.Unlock()
	return commander.info, commander.info.HostID == host
}
func (commander *fakeCommander) SendRuntime(_ context.Context, info controllernet.SessionInfo, request controllernet.RuntimeRequest) (uint64, error) {
	commander.mu.Lock()
	defer commander.mu.Unlock()
	if info.ConnectionEpoch != commander.info.ConnectionEpoch || !commander.info.Ready {
		return 0, farmerr.Error{Code: farmerr.SERVICE_NOT_READY}
	}
	sequence := uint64(len(commander.requests) + 1)
	request.DispatchSequence = sequence
	commander.requests = append(commander.requests, request)
	return sequence, nil
}
func (commander *fakeCommander) SendMaintenanceHold(_ context.Context, info controllernet.SessionInfo, active bool, revision uint64) error {
	commander.mu.Lock()
	defer commander.mu.Unlock()
	if info.ConnectionEpoch != commander.info.ConnectionEpoch {
		return farmerr.Error{Code: farmerr.SERVICE_NOT_READY}
	}
	commander.holds = append(commander.holds, fakeHoldCommand{active: active, revision: revision})
	return nil
}
func (commander *fakeCommander) setEpoch(epoch controllerstate.ConnectionEpoch) {
	commander.mu.Lock()
	commander.info.ConnectionEpoch = epoch
	commander.mu.Unlock()
}
func (commander *fakeCommander) requestsCopy() []controllernet.RuntimeRequest {
	commander.mu.Lock()
	defer commander.mu.Unlock()
	return append([]controllernet.RuntimeRequest(nil), commander.requests...)
}
func (commander *fakeCommander) holdsCopy() []fakeHoldCommand {
	commander.mu.Lock()
	defer commander.mu.Unlock()
	return append([]fakeHoldCommand(nil), commander.holds...)
}

type fakeStore struct {
	mu              sync.Mutex
	workloads       map[identity.WorkloadID]farmmodel.DesiredWorkload
	snapshots       []farmmodel.ResolvedExecutionSnapshot
	bindings        map[identity.WorkloadID]farmmodel.WorkloadRuntimeBinding
	blockEntered    chan struct{}
	blockRelease    chan struct{}
	validationErr   error
	validationFunc  func(model.Inventory) error
	holds           map[identity.HostID]farmmodel.MaintenanceHold
	restoreBarriers map[identity.HostID]farmmodel.RestoreHostBarrier
}

func newFakeStore(workload farmmodel.DesiredWorkload, snapshots ...farmmodel.ResolvedExecutionSnapshot) *fakeStore {
	return &fakeStore{workloads: map[identity.WorkloadID]farmmodel.DesiredWorkload{workload.WorkloadID: workload}, snapshots: append([]farmmodel.ResolvedExecutionSnapshot(nil), snapshots...), bindings: make(map[identity.WorkloadID]farmmodel.WorkloadRuntimeBinding), holds: make(map[identity.HostID]farmmodel.MaintenanceHold), restoreBarriers: make(map[identity.HostID]farmmodel.RestoreHostBarrier)}
}
func (store *fakeStore) replace(workload farmmodel.DesiredWorkload, snapshots ...farmmodel.ResolvedExecutionSnapshot) {
	store.mu.Lock()
	store.workloads[workload.WorkloadID] = workload
	store.snapshots = append([]farmmodel.ResolvedExecutionSnapshot(nil), snapshots...)
	store.mu.Unlock()
}
func (store *fakeStore) addWorkload(workload farmmodel.DesiredWorkload) {
	store.mu.Lock()
	store.workloads[workload.WorkloadID] = workload
	store.mu.Unlock()
}
func (store *fakeStore) GetDesiredWorkload(_ context.Context, id identity.WorkloadID) (farmmodel.DesiredWorkload, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	value, ok := store.workloads[id]
	if !ok {
		return value, farmerr.Error{Code: farmerr.NOT_FOUND}
	}
	return value, nil
}
func (store *fakeStore) ListDesiredWorkloads(context.Context) ([]farmmodel.DesiredWorkload, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	result := make([]farmmodel.DesiredWorkload, 0, len(store.workloads))
	for _, value := range store.workloads {
		result = append(result, value)
	}
	return result, nil
}
func (store *fakeStore) DeleteDesiredWorkload(_ context.Context, id identity.WorkloadID, _ uint64) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	delete(store.workloads, id)
	return nil
}
func (store *fakeStore) FinalizeRetiredDesiredWorkloadDeletion(ctx context.Context, id identity.WorkloadID, revision uint64) error {
	return store.DeleteDesiredWorkload(ctx, id, revision)
}
func (store *fakeStore) GetCurrentResolvedSnapshot(_ context.Context, id identity.WorkloadID) (farmmodel.ResolvedExecutionSnapshot, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	workload := store.workloads[id]
	for _, snapshot := range store.snapshots {
		if snapshot.WorkloadID == id && snapshot.DesiredGeneration == workload.DesiredGeneration {
			return snapshot, nil
		}
	}
	return farmmodel.ResolvedExecutionSnapshot{}, farmerr.Error{Code: farmerr.NOT_FOUND}
}
func (store *fakeStore) RefreshResolvedSnapshotForInventory(_ context.Context, _ identity.WorkloadID, _ model.Inventory) error {
	return nil
}
func (store *fakeStore) ListResolvedSnapshots(_ context.Context, id identity.WorkloadID) ([]farmmodel.ResolvedExecutionSnapshot, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	var result []farmmodel.ResolvedExecutionSnapshot
	for _, snapshot := range store.snapshots {
		if snapshot.WorkloadID == id {
			result = append(result, snapshot)
		}
	}
	return result, nil
}
func (store *fakeStore) ListAllResolvedSnapshots(context.Context) ([]farmmodel.ResolvedExecutionSnapshot, error) {
	return store.snapshotCopy(), nil
}
func (store *fakeStore) BlockDesiredGeneration(_ context.Context, id identity.WorkloadID, generation uint64, code farmerr.Code, message string) error {
	if store.blockEntered != nil {
		store.blockEntered <- struct{}{}
		<-store.blockRelease
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.workloads[id].DesiredGeneration == generation {
		store.bindings[id] = farmmodel.WorkloadRuntimeBinding{WorkloadID: id, BlockedGeneration: generation, ErrorCode: code, HumanMessage: message}
	}
	return nil
}
func (store *fakeStore) GetWorkloadRuntimeBinding(_ context.Context, id identity.WorkloadID) (farmmodel.WorkloadRuntimeBinding, bool, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	value, ok := store.bindings[id]
	return value, ok, nil
}
func (store *fakeStore) RetryWorkload(context.Context, identity.WorkloadID, uint64) (farmmodel.DesiredWorkload, error) {
	return farmmodel.DesiredWorkload{}, farmerr.Error{Code: farmerr.CONFIG_CONFLICT}
}
func (store *fakeStore) ValidateResolvedSnapshotForStart(_ context.Context, _ farmmodel.ResolvedExecutionSnapshot, inventory model.Inventory) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.validationFunc != nil {
		return store.validationFunc(inventory)
	}
	return store.validationErr
}
func (store *fakeStore) GetMaintenanceHold(_ context.Context, hostID identity.HostID) (farmmodel.MaintenanceHold, bool, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	hold, ok := store.holds[hostID]
	return hold, ok, nil
}
func (store *fakeStore) SetMaintenanceHold(_ context.Context, hostID identity.HostID, expected uint64, active bool, reason string) (farmmodel.MaintenanceHold, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	current, exists := store.holds[hostID]
	if (!exists && expected != 0) || (exists && current.Revision != expected) {
		return farmmodel.MaintenanceHold{}, farmerr.Error{Code: farmerr.REVISION_CONFLICT}
	}
	if exists && current.Active == active && current.Reason == reason {
		return current, nil
	}
	if !exists {
		current = farmmodel.MaintenanceHold{HostID: hostID, Revision: 1}
	} else {
		current.Revision++
	}
	current.Active, current.Reason = active, reason
	store.holds[hostID] = current
	return current, nil
}
func (store *fakeStore) GetRestoreHostBarrier(_ context.Context, hostID identity.HostID) (farmmodel.RestoreHostBarrier, bool, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	barrier, ok := store.restoreBarriers[hostID]
	return barrier, ok, nil
}
func (store *fakeStore) ListRestoreHostBarriers(context.Context) ([]farmmodel.RestoreHostBarrier, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	result := make([]farmmodel.RestoreHostBarrier, 0, len(store.restoreBarriers))
	for _, barrier := range store.restoreBarriers {
		result = append(result, barrier)
	}
	return result, nil
}
func (store *fakeStore) restoreBarrier(hostID identity.HostID) (farmmodel.RestoreHostBarrier, bool) {
	store.mu.Lock()
	defer store.mu.Unlock()
	barrier, ok := store.restoreBarriers[hostID]
	return barrier, ok
}
func (store *fakeStore) MarkRestoreHostConflict(_ context.Context, hostID identity.HostID, backupID string, revision uint64) (farmmodel.RestoreHostBarrier, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	barrier, ok := store.restoreBarriers[hostID]
	if !ok || barrier.BackupID != backupID || barrier.Revision != revision {
		return farmmodel.RestoreHostBarrier{}, farmerr.Error{Code: farmerr.REVISION_CONFLICT}
	}
	if barrier.Status != farmmodel.RestoreBarrierConflict {
		barrier.Status = farmmodel.RestoreBarrierConflict
		barrier.ReasonCode = farmerr.RESTORE_RECONCILIATION_REQUIRED
		barrier.Revision++
		store.restoreBarriers[hostID] = barrier
	}
	return barrier, nil
}
func (store *fakeStore) ClearRestoreHostBarrier(_ context.Context, hostID identity.HostID, backupID string, revision uint64) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	barrier, ok := store.restoreBarriers[hostID]
	if !ok || barrier.BackupID != backupID || barrier.Revision != revision {
		return farmerr.Error{Code: farmerr.REVISION_CONFLICT}
	}
	delete(store.restoreBarriers, hostID)
	return nil
}
func (store *fakeStore) isBlocked(id identity.WorkloadID, generation uint64) bool {
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.bindings[id].BlockedGeneration == generation
}
func (store *fakeStore) hasWorkload(id identity.WorkloadID) bool {
	store.mu.Lock()
	defer store.mu.Unlock()
	_, ok := store.workloads[id]
	return ok
}
func (store *fakeStore) snapshotCopy() []farmmodel.ResolvedExecutionSnapshot {
	store.mu.Lock()
	defer store.mu.Unlock()
	return append([]farmmodel.ResolvedExecutionSnapshot(nil), store.snapshots...)
}

func waitRequests(t *testing.T, commander *fakeCommander, count int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if len(commander.requestsCopy()) >= count {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("got %d requests, want %d", len(commander.requestsCopy()), count)
}

func codeOfReconcile(err error) farmerr.Code { code, _ := farmerr.CodeOf(err); return code }
