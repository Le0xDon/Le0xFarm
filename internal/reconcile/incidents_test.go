package reconcile

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/le0xdon/le0xfarm/internal/controllerdb"
	"github.com/le0xdon/le0xfarm/internal/controllerstate"
	"github.com/le0xdon/le0xfarm/internal/farmconfig"
	"github.com/le0xdon/le0xfarm/internal/farmerr"
	"github.com/le0xdon/le0xfarm/internal/farmmodel"
	"github.com/le0xdon/le0xfarm/internal/identity"
	"github.com/le0xdon/le0xfarm/internal/incidents"
	"github.com/le0xdon/le0xfarm/internal/model"
	"github.com/le0xdon/le0xfarm/internal/packagecatalog"
)

func TestCoordinatorIncidentEvaluationUsesCurrentEpochAuthority(t *testing.T) {
	host := testHost(1)
	workloadID := testWorkload(1)
	snapshot := testSnapshot(workloadID, host, 1, testExecution(1), farmmodel.ResourceClaim{CPU: true})
	store := &incidentRecordingStore{fakeStore: newFakeStore(testWorkloadObject(workloadID, host, farmmodel.DesiredRunning, 1, snapshot.Resources), snapshot)}
	observed := controllerstate.New()
	commander := newFakeCommander(host, 1)
	coordinator := NewCoordinator(store, observed, commander, nil)

	coordinator.evaluateIncidents(context.Background(), host)
	requireRecordedType(t, store.snapshot(), farmmodel.IncidentAgentOffline)

	observed.Connect(commander.info.AgentID, host, commander.info.ConnectionEpoch)
	coordinator.evaluateIncidents(context.Background(), host)
	partial := store.snapshot()
	requireRecordedType(t, partial, farmmodel.IncidentMonitoringStale)
	if partial.resolvable[farmmodel.IncidentAgentOffline] {
		t.Fatal("reconnect without bootstrap could resolve offline incident")
	}

	running := observedExecution(snapshot, model.ExecutionRunning)
	running.UsefulWork = &model.UsefulWorkEvidence{Provider: "generic", Availability: model.TelemetryStale, UsefulWork: model.UsefulWorkConfirmed, FreshFor: time.Minute}
	readyStore(observed, commander.info, []model.ExecutionObservation{running})
	coordinator.evaluateIncidents(context.Background(), host)
	fresh := store.snapshot()
	requireRecordedType(t, fresh, farmmodel.IncidentUsefulWorkDegraded)
	if !fresh.resolvable[farmmodel.IncidentAgentOffline] {
		t.Fatal("fresh bootstrap could not resolve prior offline incident")
	}

	old := commander.info
	commander.setEpoch(2)
	healthy := observedExecution(snapshot, model.ExecutionRunning)
	healthy.UsefulWork = &model.UsefulWorkEvidence{Provider: "generic", Availability: model.TelemetryAvailable, UsefulWork: model.UsefulWorkConfirmed, FreshFor: time.Minute}
	readyStore(observed, commander.info, []model.ExecutionObservation{healthy})
	coordinator.SessionDisconnected(old)
	current, ok := observed.Get(host)
	if !ok || !current.Connected || current.ConnectionEpoch != 2 {
		t.Fatalf("old epoch disconnect damaged current incident authority: %+v", current)
	}
	coordinator.evaluateIncidents(context.Background(), host)
	currentEval := store.snapshot()
	if len(currentEval.conditions) != 0 || !currentEval.resolvable[farmmodel.IncidentUsefulWorkDegraded] {
		t.Fatalf("current healthy epoch did not resolve degraded incident: %+v", currentEval)
	}
	calls := currentEval.calls
	coordinator.evaluateIncidents(context.Background(), host)
	if after := store.snapshot().calls; after != calls {
		t.Fatalf("identical level-triggered evaluation rewrote persistence: before=%d after=%d", calls, after)
	}
}

func TestIncidentEvaluationRetriesWhenBlockedBindingClearsBeforePersistence(t *testing.T) {
	host := testHost(1)
	workloadID := testWorkload(1)
	snapshot := testSnapshot(workloadID, host, 1, testExecution(1), farmmodel.ResourceClaim{CPU: true})
	base := newFakeStore(testWorkloadObject(workloadID, host, farmmodel.DesiredRunning, 1, snapshot.Resources), snapshot)
	base.bindings[workloadID] = farmmodel.WorkloadRuntimeBinding{WorkloadID: workloadID, BlockedGeneration: 1, ErrorCode: farmerr.PACKAGE_NOT_INSTALLED}
	store := newBarrierIncidentStore(base)
	observed := controllerstate.New()
	commander := newFakeCommander(host, 1)
	coordinator := NewCoordinator(store, observed, commander, nil)
	readyStore(observed, commander.info, []model.ExecutionObservation{observedExecution(snapshot, model.ExecutionRunning)})

	done := make(chan struct{})
	go func() {
		coordinator.evaluateIncidents(context.Background(), host)
		close(done)
	}()
	<-store.reconcileEntered
	base.mu.Lock()
	delete(base.bindings, workloadID)
	base.mu.Unlock()
	close(store.reconcileRelease)
	<-done
	latest := waitIncidentRecord(t, store)
	for _, condition := range latest.conditions {
		if condition.Type == farmmodel.IncidentConfigurationBlocked {
			t.Fatalf("stale blocked binding was published: %+v", latest.conditions)
		}
	}
}

func TestIncidentEvaluationRetriesWhenBindingAppearsBeforeRecoveryPersistence(t *testing.T) {
	host := testHost(1)
	workloadID := testWorkload(1)
	snapshot := testSnapshot(workloadID, host, 1, testExecution(1), farmmodel.ResourceClaim{CPU: true})
	base := newFakeStore(testWorkloadObject(workloadID, host, farmmodel.DesiredRunning, 1, snapshot.Resources), snapshot)
	store := newBarrierIncidentStore(base)
	prior := incidents.NewCondition(farmmodel.IncidentConfigurationBlocked, farmmodel.IncidentSeverityError, host, &workloadID, nil, nil, farmerr.PACKAGE_NOT_INSTALLED)
	store.last = incidentEvaluationRecord{conditions: []farmmodel.IncidentCondition{prior}, calls: 1}
	observed := controllerstate.New()
	commander := newFakeCommander(host, 1)
	coordinator := NewCoordinator(store, observed, commander, nil)
	readyStore(observed, commander.info, []model.ExecutionObservation{observedExecution(snapshot, model.ExecutionRunning)})

	done := make(chan struct{})
	go func() {
		coordinator.evaluateIncidents(context.Background(), host)
		close(done)
	}()
	<-store.reconcileEntered
	base.mu.Lock()
	base.bindings[workloadID] = farmmodel.WorkloadRuntimeBinding{WorkloadID: workloadID, BlockedGeneration: 1, ErrorCode: farmerr.PACKAGE_NOT_INSTALLED}
	base.mu.Unlock()
	close(store.reconcileRelease)
	<-done
	latest := waitIncidentRecord(t, store)
	requireRecordedType(t, latest, farmmodel.IncidentConfigurationBlocked)
}

func TestControllerIncidentLifecycleSurvivesRestartThenFreshRecovery(t *testing.T) {
	host := testHost(1)
	dir := filepath.Join(t.TempDir(), "controller")
	catalog, err := packagecatalog.NewStatic([]farmmodel.PackageRelease{{Ref: farmmodel.PackageRef{PackageID: integrationPackageID, Version: "1"}, AdapterIDs: []string{integrationAdapterID}, Runtime: farmmodel.RuntimeCapabilities{CPU: true}}})
	if err != nil {
		t.Fatal(err)
	}
	db, err := controllerdb.Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	store, err := farmconfig.New(db, catalog, farmconfig.Options{})
	if err != nil {
		t.Fatal(err)
	}
	pool, err := store.CreatePool(context.Background(), farmmodel.PoolContent{Name: "local", Address: "127.0.0.1:1", Auth: farmmodel.PoolAuth{Kind: farmmodel.PoolAuthNone}})
	if err != nil {
		t.Fatal(err)
	}
	wallet, err := store.CreateWalletRef(context.Background(), farmmodel.WalletRefContent{Name: "public", Coin: "TEST", Address: "public-payout"})
	if err != nil {
		t.Fatal(err)
	}
	profile, err := store.CreateMiningProfile(context.Background(), farmmodel.MiningProfileContent{Name: "test", AdapterID: integrationAdapterID, Package: farmmodel.PackageRef{PackageID: integrationPackageID, Version: "1"}, Mode: farmmodel.ProfileModeMining, Coin: "TEST", PoolID: pool.PoolID, WalletID: wallet.WalletID, LoginPolicy: farmmodel.LoginPolicy{UserTemplate: "${wallet}", WorkerPlacement: farmmodel.WorkerNone}})
	if err != nil {
		t.Fatal(err)
	}
	workload, err := store.CreateDesiredWorkload(context.Background(), farmmodel.DesiredWorkloadContent{Name: "test", HostID: host, ProfileID: profile.ProfileID, RunState: farmmodel.DesiredRunning, Resources: farmmodel.ResourceClaim{CPU: true}})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.GetCurrentResolvedSnapshot(context.Background(), workload.WorkloadID)
	if err != nil {
		t.Fatal(err)
	}
	commander := newFakeCommander(host, 1)
	observed := controllerstate.New()
	stale := observedExecution(snapshot, model.ExecutionRunning)
	stale.UsefulWork = &model.UsefulWorkEvidence{Provider: "generic", Availability: model.TelemetryStale, UsefulWork: model.UsefulWorkConfirmed, ReasonCode: farmerr.TELEMETRY_STALE, FreshFor: time.Minute}
	readyStore(observed, commander.info, []model.ExecutionObservation{stale})
	NewCoordinator(store, observed, commander, nil).evaluateIncidents(context.Background(), host)
	active, err := store.ListActiveIncidents(context.Background())
	if err != nil || len(active) != 1 || active[0].Type != farmmodel.IncidentUsefulWorkDegraded {
		t.Fatalf("initial Controller incident=%+v err=%v", active, err)
	}
	incidentID := active[0].IncidentID
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = controllerdb.Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store, err = farmconfig.New(db, catalog, farmconfig.Options{})
	if err != nil {
		t.Fatal(err)
	}
	active, err = store.ListActiveIncidents(context.Background())
	if err != nil || len(active) != 1 || active[0].IncidentID != incidentID {
		t.Fatalf("restarted Controller lost incident=%+v err=%v", active, err)
	}
	commander = newFakeCommander(host, 2)
	observed = controllerstate.New()
	healthy := observedExecution(snapshot, model.ExecutionRunning)
	healthy.UsefulWork = &model.UsefulWorkEvidence{Provider: "generic", Availability: model.TelemetryAvailable, UsefulWork: model.UsefulWorkConfirmed, CollectedAt: time.Now().UTC(), FreshFor: time.Minute}
	readyStore(observed, commander.info, []model.ExecutionObservation{healthy})
	NewCoordinator(store, observed, commander, nil).evaluateIncidents(context.Background(), host)
	active, err = store.ListActiveIncidents(context.Background())
	if err != nil || len(active) != 0 {
		t.Fatalf("fresh recovery did not resolve incident=%+v err=%v", active, err)
	}
	resolved, err := store.ListIncidents(context.Background(), farmmodel.IncidentQuery{State: farmmodel.IncidentResolved})
	if err != nil || len(resolved) != 1 || resolved[0].IncidentID != incidentID {
		t.Fatalf("resolved lifecycle after Controller restart=%+v err=%v", resolved, err)
	}
}

type incidentEvaluationRecord struct {
	conditions []farmmodel.IncidentCondition
	resolvable map[farmmodel.IncidentType]bool
	calls      int
}

type cancelIncidentStore struct {
	*fakeStore
	entered chan struct{}
	exited  chan struct{}
}

func (store *cancelIncidentStore) IncidentAuthority(context.Context, identity.HostID) (string, error) {
	return "shutdown-authority", nil
}

func (store *cancelIncidentStore) ReconcileIncidents(ctx context.Context, _ identity.HostID, _ string, _ []farmmodel.IncidentCondition, _ map[farmmodel.IncidentType]bool) error {
	close(store.entered)
	<-ctx.Done()
	close(store.exited)
	return ctx.Err()
}

func TestCoordinatorShutdownCancelsAndJoinsIncidentWorker(t *testing.T) {
	host := testHost(1)
	workloadID := testWorkload(1)
	snapshot := testSnapshot(workloadID, host, 1, testExecution(1), farmmodel.ResourceClaim{CPU: true})
	store := &cancelIncidentStore{
		fakeStore: newFakeStore(testWorkloadObject(workloadID, host, farmmodel.DesiredRunning, 1, snapshot.Resources), snapshot),
		entered:   make(chan struct{}),
		exited:    make(chan struct{}),
	}
	coordinator := NewCoordinator(store, controllerstate.New(), newFakeCommander(host, 1), nil)
	coordinator.queueIncidentEvaluation(host)
	select {
	case <-store.entered:
	case <-time.After(time.Second):
		t.Fatal("incident worker did not reach persistence")
	}
	coordinator.Shutdown()
	select {
	case <-store.exited:
	default:
		t.Fatal("Shutdown returned before incident worker observed cancellation")
	}
}

type incidentRecordingStore struct {
	*fakeStore
	incidentMu       sync.Mutex
	last             incidentEvaluationRecord
	reconcileEntered chan struct{}
	reconcileRelease chan struct{}
	barrierOnce      sync.Once
	recorded         chan incidentEvaluationRecord
}

func newBarrierIncidentStore(store *fakeStore) *incidentRecordingStore {
	return &incidentRecordingStore{fakeStore: store, reconcileEntered: make(chan struct{}, 1), reconcileRelease: make(chan struct{}), recorded: make(chan incidentEvaluationRecord, 2)}
}

func (store *incidentRecordingStore) IncidentAuthority(context.Context, identity.HostID) (string, error) {
	store.fakeStore.mu.Lock()
	defer store.fakeStore.mu.Unlock()
	return fmt.Sprintf("%#v|%#v|%#v", store.fakeStore.workloads, store.fakeStore.snapshots, store.fakeStore.bindings), nil
}

func (store *incidentRecordingStore) ReconcileIncidents(_ context.Context, host identity.HostID, authority string, conditions []farmmodel.IncidentCondition, resolvable map[farmmodel.IncidentType]bool) error {
	if store.reconcileEntered != nil {
		store.barrierOnce.Do(func() {
			store.reconcileEntered <- struct{}{}
			<-store.reconcileRelease
		})
	}
	current, _ := store.IncidentAuthority(context.Background(), host)
	if current != authority {
		return farmerr.Error{Code: farmerr.REVISION_CONFLICT}
	}
	store.incidentMu.Lock()
	defer store.incidentMu.Unlock()
	store.last.conditions = append([]farmmodel.IncidentCondition(nil), conditions...)
	store.last.calls++
	store.last.resolvable = make(map[farmmodel.IncidentType]bool, len(resolvable))
	for kind, value := range resolvable {
		store.last.resolvable[kind] = value
	}
	if store.recorded != nil {
		store.recorded <- store.last
	}
	return nil
}

func (store *incidentRecordingStore) snapshot() incidentEvaluationRecord {
	store.incidentMu.Lock()
	defer store.incidentMu.Unlock()
	result := incidentEvaluationRecord{conditions: append([]farmmodel.IncidentCondition(nil), store.last.conditions...), resolvable: make(map[farmmodel.IncidentType]bool, len(store.last.resolvable)), calls: store.last.calls}
	for kind, value := range store.last.resolvable {
		result.resolvable[kind] = value
	}
	return result
}

func requireRecordedType(t *testing.T, record incidentEvaluationRecord, kind farmmodel.IncidentType) {
	t.Helper()
	for _, condition := range record.conditions {
		if condition.Type == kind {
			return
		}
	}
	t.Fatalf("missing recorded incident %s in %+v", kind, record.conditions)
}

func waitIncidentRecord(t *testing.T, store *incidentRecordingStore) incidentEvaluationRecord {
	t.Helper()
	select {
	case record := <-store.recorded:
		return record
	case <-time.After(time.Second):
		t.Fatal("incident retry did not publish current authority")
		return incidentEvaluationRecord{}
	}
}
