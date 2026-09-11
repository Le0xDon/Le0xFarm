package farmconfig

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/le0xdon/le0xfarm/internal/controllerstate"
	"github.com/le0xdon/le0xfarm/internal/farmerr"
	"github.com/le0xdon/le0xfarm/internal/farmmodel"
	"github.com/le0xdon/le0xfarm/internal/identity"
	"github.com/le0xdon/le0xfarm/internal/incidents"
	"github.com/le0xdon/le0xfarm/internal/model"
)

func TestIncidentLifecycleDedupRecoveryRecurrenceAndPersistence(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "controller")
	now := time.Date(2026, 9, 11, 1, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	db, service := newService(t, dir, Options{Clock: clock})
	host := incidentHost(1)
	workload := incidentWorkload(1)
	condition := incidents.NewCondition(farmmodel.IncidentUsefulWorkDegraded, farmmodel.IncidentSeverityWarning, host, &workload, nil, nil, farmerr.TELEMETRY_STALE)
	resolvable := map[farmmodel.IncidentType]bool{farmmodel.IncidentUsefulWorkDegraded: true}
	if err := reconcileIncidents(ctx, service, host, []farmmodel.IncidentCondition{condition}, resolvable); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	if err := reconcileIncidents(ctx, service, host, []farmmodel.IncidentCondition{condition}, resolvable); err != nil {
		t.Fatal(err)
	}
	active := mustActiveIncidents(t, service)
	if len(active) != 1 || active[0].OccurrenceCount != 1 || !active[0].LastObservedAt.Equal(now) {
		t.Fatalf("active dedup failed: %+v", active)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, service = newService(t, dir, Options{Clock: clock})
	defer db.Close()
	active = mustActiveIncidents(t, service)
	if len(active) != 1 || active[0].IncidentID != condition.IncidentID {
		t.Fatalf("incident did not persist: %+v", active)
	}
	now = now.Add(time.Minute)
	if err := reconcileIncidents(ctx, service, host, nil, resolvable); err != nil {
		t.Fatal(err)
	}
	resolved, err := service.ListIncidents(ctx, farmmodel.IncidentQuery{State: farmmodel.IncidentResolved})
	if err != nil || len(resolved) != 1 || resolved[0].ResolvedAt == nil {
		t.Fatalf("resolution failed: %+v err=%v", resolved, err)
	}
	now = now.Add(time.Minute)
	if err := reconcileIncidents(ctx, service, host, []farmmodel.IncidentCondition{condition}, resolvable); err != nil {
		t.Fatal(err)
	}
	active = mustActiveIncidents(t, service)
	if len(active) != 1 || active[0].IncidentID != condition.IncidentID || active[0].OccurrenceCount != 2 || active[0].ResolvedAt != nil {
		t.Fatalf("recurrence failed: %+v", active)
	}
}

func TestLostAuthorityDoesNotResolveIncident(t *testing.T) {
	db, service := newService(t, t.TempDir(), Options{})
	defer db.Close()
	host := incidentHost(1)
	condition := incidents.NewCondition(farmmodel.IncidentRuntimeError, farmmodel.IncidentSeverityError, host, nil, nil, nil, farmerr.PROCESS_CRASHED)
	if err := reconcileIncidents(context.Background(), service, host, []farmmodel.IncidentCondition{condition}, map[farmmodel.IncidentType]bool{farmmodel.IncidentRuntimeError: true}); err != nil {
		t.Fatal(err)
	}
	if err := reconcileIncidents(context.Background(), service, host, nil, map[farmmodel.IncidentType]bool{farmmodel.IncidentMaintenanceHold: true}); err != nil {
		t.Fatal(err)
	}
	if got := mustActiveIncidents(t, service); len(got) != 1 || got[0].Type != farmmodel.IncidentRuntimeError {
		t.Fatalf("missing evidence falsely resolved incident: %+v", got)
	}
}

func TestConcurrentIncidentEvaluationCreatesOneActiveIncident(t *testing.T) {
	db, service := newService(t, t.TempDir(), Options{})
	defer db.Close()
	host := incidentHost(1)
	condition := incidents.NewCondition(farmmodel.IncidentUnmanagedProcessConflict, farmmodel.IncidentSeverityWarning, host, nil, nil, nil, farmerr.UNMANAGED_PROCESS_CONFLICT)
	authority, err := service.IncidentAuthority(context.Background(), host)
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	errors := make(chan error, 24)
	var wait sync.WaitGroup
	for range 24 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			errors <- service.ReconcileIncidents(context.Background(), host, authority, []farmmodel.IncidentCondition{condition}, map[farmmodel.IncidentType]bool{farmmodel.IncidentUnmanagedProcessConflict: true})
		}()
	}
	close(start)
	wait.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := mustActiveIncidents(t, service); len(got) != 1 || got[0].OccurrenceCount != 1 {
		t.Fatalf("concurrent evaluations duplicated incident: %+v", got)
	}
}

func TestIncidentPersistenceRejectsUntrustedTextAsCode(t *testing.T) {
	db, service := newService(t, t.TempDir(), Options{})
	defer db.Close()
	host := incidentHost(1)
	condition := incidents.NewCondition(farmmodel.IncidentConfigurationBlocked, farmmodel.IncidentSeverityError, host, nil, nil, nil, farmerr.CONFIG_CONFLICT)
	condition.ReasonCode = farmerr.Code("wallet-seed\nforged-log")
	authority, authorityErr := service.IncidentAuthority(context.Background(), host)
	if authorityErr != nil {
		t.Fatal(authorityErr)
	}
	err := service.ReconcileIncidents(context.Background(), host, authority, []farmmodel.IncidentCondition{condition}, nil)
	assertCode(t, err, farmerr.CONFIG_CONFLICT)
	if got := mustActiveIncidents(t, service); len(got) != 0 {
		t.Fatalf("unsafe incident persisted: %+v", got)
	}
}

func TestIncidentAuthorityRejectsStaleBindingActivationAndResolution(t *testing.T) {
	db, service := newService(t, t.TempDir(), Options{})
	defer db.Close()
	_, _, profile := baseObjects(t, service)
	host := hostID(1)
	workload, err := service.CreateDesiredWorkload(context.Background(), workloadContent(profile.ProfileID, host, farmmodel.DesiredRunning, farmmodel.ResourceClaim{CPU: true}))
	if err != nil {
		t.Fatal(err)
	}
	if err := service.BlockDesiredGeneration(context.Background(), workload.WorkloadID, workload.DesiredGeneration, farmerr.PACKAGE_NOT_INSTALLED, "safe typed block"); err != nil {
		t.Fatal(err)
	}
	blockedAuthority, err := service.IncidentAuthority(context.Background(), host)
	if err != nil {
		t.Fatal(err)
	}
	condition := incidents.NewCondition(farmmodel.IncidentConfigurationBlocked, farmmodel.IncidentSeverityError, host, &workload.WorkloadID, nil, nil, farmerr.PACKAGE_NOT_INSTALLED)
	if _, err := service.RetryWorkload(context.Background(), workload.WorkloadID, workload.Meta.Revision); err != nil {
		t.Fatal(err)
	}
	err = service.ReconcileIncidents(context.Background(), host, blockedAuthority, []farmmodel.IncidentCondition{condition}, map[farmmodel.IncidentType]bool{farmmodel.IncidentConfigurationBlocked: true})
	assertCode(t, err, farmerr.REVISION_CONFLICT)
	if got := mustActiveIncidents(t, service); len(got) != 0 {
		t.Fatalf("stale binding activation was persisted: %+v", got)
	}

	currentAuthority, err := service.IncidentAuthority(context.Background(), host)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.ReconcileIncidents(context.Background(), host, currentAuthority, []farmmodel.IncidentCondition{condition}, map[farmmodel.IncidentType]bool{farmmodel.IncidentConfigurationBlocked: true}); err != nil {
		t.Fatal(err)
	}
	loaded, err := service.GetDesiredWorkload(context.Background(), workload.WorkloadID)
	if err != nil {
		t.Fatal(err)
	}
	renamed := loaded.DesiredWorkloadContent
	renamed.Name = "authority changed"
	if _, err := service.UpdateDesiredWorkload(context.Background(), loaded.WorkloadID, loaded.Meta.Revision, renamed); err != nil {
		t.Fatal(err)
	}
	err = service.ReconcileIncidents(context.Background(), host, currentAuthority, nil, map[farmmodel.IncidentType]bool{farmmodel.IncidentConfigurationBlocked: true})
	assertCode(t, err, farmerr.REVISION_CONFLICT)
	if got := mustActiveIncidents(t, service); len(got) != 1 || got[0].IncidentID != condition.IncidentID {
		t.Fatalf("stale recovery resolved current incident: %+v", got)
	}
}

func TestConcurrentStaleActivateCannotOverrideCurrentResolution(t *testing.T) {
	db, service := newService(t, t.TempDir(), Options{})
	defer db.Close()
	_, _, profile := baseObjects(t, service)
	host := hostID(1)
	workload, err := service.CreateDesiredWorkload(context.Background(), workloadContent(profile.ProfileID, host, farmmodel.DesiredStopped, farmmodel.ResourceClaim{CPU: true}))
	if err != nil {
		t.Fatal(err)
	}
	condition := incidents.NewCondition(farmmodel.IncidentConfigurationBlocked, farmmodel.IncidentSeverityError, host, &workload.WorkloadID, nil, nil, farmerr.CONFIG_CONFLICT)
	if err := reconcileIncidents(context.Background(), service, host, []farmmodel.IncidentCondition{condition}, map[farmmodel.IncidentType]bool{farmmodel.IncidentConfigurationBlocked: true}); err != nil {
		t.Fatal(err)
	}
	staleAuthority, err := service.IncidentAuthority(context.Background(), host)
	if err != nil {
		t.Fatal(err)
	}
	renamed := workload.DesiredWorkloadContent
	renamed.Name = "newest authority"
	if _, err := service.UpdateDesiredWorkload(context.Background(), workload.WorkloadID, workload.Meta.Revision, renamed); err != nil {
		t.Fatal(err)
	}
	currentAuthority, err := service.IncidentAuthority(context.Background(), host)
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	errors := make(chan error, 2)
	go func() {
		<-start
		errors <- service.ReconcileIncidents(context.Background(), host, staleAuthority, []farmmodel.IncidentCondition{condition}, map[farmmodel.IncidentType]bool{farmmodel.IncidentConfigurationBlocked: true})
	}()
	go func() {
		<-start
		errors <- service.ReconcileIncidents(context.Background(), host, currentAuthority, nil, map[farmmodel.IncidentType]bool{farmmodel.IncidentConfigurationBlocked: true})
	}()
	close(start)
	var revisionConflicts, successes int
	for range 2 {
		err := <-errors
		if err == nil {
			successes++
		} else if code, _ := farmerr.CodeOf(err); code == farmerr.REVISION_CONFLICT {
			revisionConflicts++
		} else {
			t.Fatal(err)
		}
	}
	if successes != 1 || revisionConflicts != 1 {
		t.Fatalf("activate/resolve results successes=%d revision_conflicts=%d", successes, revisionConflicts)
	}
	active := mustActiveIncidents(t, service)
	resolved, err := service.ListIncidents(context.Background(), farmmodel.IncidentQuery{State: farmmodel.IncidentResolved})
	if err != nil || len(active) != 0 || len(resolved) != 1 || resolved[0].IncidentID != condition.IncidentID {
		t.Fatalf("newest authority did not win: active=%+v resolved=%+v err=%v", active, resolved, err)
	}
}

func TestWorkloadDeletionAtomicallyResolvesOnlyWorkloadIncidentsAcrossReopen(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "controller")
	db, service := newService(t, dir, Options{})
	_, _, profile := baseObjects(t, service)
	host := hostID(1)
	workload, err := service.CreateDesiredWorkload(context.Background(), workloadContent(profile.ProfileID, host, farmmodel.DesiredStopped, farmmodel.ResourceClaim{CPU: true}))
	if err != nil {
		t.Fatal(err)
	}
	workloadIncident := incidents.NewCondition(farmmodel.IncidentConfigurationBlocked, farmmodel.IncidentSeverityError, host, &workload.WorkloadID, nil, nil, farmerr.CONFIG_CONFLICT)
	hostIncident := incidents.NewCondition(farmmodel.IncidentAgentOffline, farmmodel.IncidentSeverityError, host, nil, nil, nil, farmerr.SERVICE_NOT_READY)
	if err := reconcileIncidents(context.Background(), service, host, []farmmodel.IncidentCondition{workloadIncident, hostIncident}, map[farmmodel.IncidentType]bool{}); err != nil {
		t.Fatal(err)
	}
	if err := service.DeleteDesiredWorkload(context.Background(), workload.WorkloadID, workload.Meta.Revision); err != nil {
		t.Fatal(err)
	}
	assertDeletedIncidentLifecycle(t, service, workloadIncident.IncidentID, hostIncident.IncidentID)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, service = newService(t, dir, Options{})
	defer db.Close()
	assertDeletedIncidentLifecycle(t, service, workloadIncident.IncidentID, hostIncident.IncidentID)
}

func TestMaintenanceHoldKeepsConflictActiveUntilFreshConflictRecovery(t *testing.T) {
	db, service := newService(t, t.TempDir(), Options{})
	defer db.Close()
	host := incidentHost(1)
	workloadID := incidentWorkload(1)
	executionID, _ := identity.ParseExecutionID("execution_00000000000000000000000000000001")
	deviceID, _ := identity.ParseDeviceID("device_00000000000000000000000000000001")
	hash := fmt.Sprintf("sha256:%064x", 1)
	claim := farmmodel.ResourceClaim{DeviceIDs: []identity.DeviceID{deviceID}}
	ownership := model.WorkloadOwnership{WorkloadID: workloadID, DesiredGeneration: 1, ResolvedHash: hash, HostID: host, DeviceIDs: []identity.DeviceID{deviceID}}
	snapshot := farmmodel.ResolvedExecutionSnapshot{WorkloadID: workloadID, DesiredGeneration: 1, ExecutionID: executionID, HostID: host, Resources: claim, ResolvedHash: hash}
	input := incidents.Input{
		HostID: host,
		Workloads: []incidents.WorkloadFacts{{
			Workload: farmmodel.DesiredWorkload{WorkloadID: workloadID, DesiredGeneration: 1, DesiredWorkloadContent: farmmodel.DesiredWorkloadContent{HostID: host, RunState: farmmodel.DesiredRunning, Resources: claim}},
			Current:  &snapshot,
		}},
		AllSnapshots: []farmmodel.ResolvedExecutionSnapshot{snapshot},
		HasObserved:  true,
		Observed: controllerstate.HostObservation{
			HostID: host, ConnectionEpoch: 1, Connected: true, Ready: true, Fresh: true, InventoryFresh: true, ProcessesFresh: true,
			Inventory:          model.Inventory{Host: model.Host{HostID: host}, GPUs: []model.GPU{{DeviceID: deviceID}}},
			Executions:         []model.ExecutionObservation{{ExecutionID: executionID, Ownership: &ownership, Status: model.ExecutionStopped}},
			UnmanagedProcesses: []model.UnmanagedProcessObservation{{PID: 42, ProcessInstance: "ticks:7", GPURelevant: true, ResourceScope: model.ProcessResourcesExact, DeviceIDs: []identity.DeviceID{deviceID}}},
		},
	}
	active := incidents.Evaluate(input)
	if err := reconcileIncidents(context.Background(), service, host, active.Conditions, active.ResolvableTypes); err != nil {
		t.Fatal(err)
	}
	values := mustActiveIncidents(t, service)
	if len(values) != 1 || values[0].Type != farmmodel.IncidentUnmanagedProcessConflict {
		t.Fatalf("initial unmanaged incident=%+v", values)
	}
	hold, err := service.SetMaintenanceHold(context.Background(), host, 0, true, "")
	if err != nil {
		t.Fatal(err)
	}
	input.Hold = &hold
	held := incidents.Evaluate(input)
	if err := reconcileIncidents(context.Background(), service, host, held.Conditions, held.ResolvableTypes); err != nil {
		t.Fatal(err)
	}
	values = mustActiveIncidents(t, service)
	if len(values) != 2 || !hasIncidentType(values, farmmodel.IncidentMaintenanceHold) || !hasIncidentType(values, farmmodel.IncidentUnmanagedProcessConflict) {
		t.Fatalf("Hold hid active unmanaged conflict: %+v", values)
	}
	input.Observed.UnmanagedProcesses = nil
	recovered := incidents.Evaluate(input)
	if err := reconcileIncidents(context.Background(), service, host, recovered.Conditions, recovered.ResolvableTypes); err != nil {
		t.Fatal(err)
	}
	values = mustActiveIncidents(t, service)
	if len(values) != 1 || values[0].Type != farmmodel.IncidentMaintenanceHold {
		t.Fatalf("fresh conflict recovery during Hold was not truthful: %+v", values)
	}
	resolved, err := service.ListIncidents(context.Background(), farmmodel.IncidentQuery{State: farmmodel.IncidentResolved})
	if err != nil || len(resolved) != 1 || resolved[0].Type != farmmodel.IncidentUnmanagedProcessConflict {
		t.Fatalf("conflict lifecycle after recovery=%+v err=%v", resolved, err)
	}
}

func TestDistinctTelemetryStatesPersistAsDistinctDiagnostics(t *testing.T) {
	db, service := newService(t, t.TempDir(), Options{})
	defer db.Close()
	host := incidentHost(1)
	workload := incidentWorkload(1)
	execution, _ := identity.ParseExecutionID("execution_00000000000000000000000000000001")
	conditions := []farmmodel.IncidentCondition{
		incidents.NewCondition(farmmodel.IncidentTelemetryUnknown, farmmodel.IncidentSeverityWarning, host, &workload, &execution, nil, farmerr.TELEMETRY_UNKNOWN),
		incidents.NewCondition(farmmodel.IncidentTelemetryUnavailable, farmmodel.IncidentSeverityWarning, host, &workload, &execution, nil, farmerr.TELEMETRY_UNAVAILABLE),
		incidents.NewCondition(farmmodel.IncidentUsefulWorkDegraded, farmmodel.IncidentSeverityWarning, host, &workload, &execution, nil, farmerr.TELEMETRY_STALE),
		incidents.NewCondition(farmmodel.IncidentUsefulWorkDegraded, farmmodel.IncidentSeverityWarning, host, &workload, &execution, nil, farmerr.ZERO_HASHRATE),
	}
	if err := reconcileIncidents(context.Background(), service, host, conditions, map[farmmodel.IncidentType]bool{}); err != nil {
		t.Fatal(err)
	}
	active := mustActiveIncidents(t, service)
	if len(active) != len(conditions) {
		t.Fatalf("telemetry diagnostics collapsed: %+v", active)
	}
	seen := make(map[identity.IncidentID]struct{}, len(active))
	for _, incident := range active {
		seen[incident.IncidentID] = struct{}{}
	}
	if len(seen) != len(conditions) {
		t.Fatalf("distinct telemetry diagnostics shared identity: %+v", active)
	}
}

func hasIncidentType(values []farmmodel.Incident, kind farmmodel.IncidentType) bool {
	for _, value := range values {
		if value.Type == kind {
			return true
		}
	}
	return false
}

func assertDeletedIncidentLifecycle(t *testing.T, service *Service, workloadIncident, hostIncident identity.IncidentID) {
	t.Helper()
	active := mustActiveIncidents(t, service)
	resolved, err := service.ListIncidents(context.Background(), farmmodel.IncidentQuery{State: farmmodel.IncidentResolved})
	if err != nil || len(active) != 1 || active[0].IncidentID != hostIncident || len(resolved) != 1 || resolved[0].IncidentID != workloadIncident {
		t.Fatalf("deleted workload lifecycle active=%+v resolved=%+v err=%v", active, resolved, err)
	}
}

func reconcileIncidents(ctx context.Context, service *Service, host identity.HostID, conditions []farmmodel.IncidentCondition, resolvable map[farmmodel.IncidentType]bool) error {
	authority, err := service.IncidentAuthority(ctx, host)
	if err != nil {
		return err
	}
	return service.ReconcileIncidents(ctx, host, authority, conditions, resolvable)
}

func mustActiveIncidents(t *testing.T, service *Service) []farmmodel.Incident {
	t.Helper()
	values, err := service.ListActiveIncidents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return values
}

func incidentHost(n int) identity.HostID {
	id, _ := identity.ParseHostID(fmt.Sprintf("host_%032x", n))
	return id
}
func incidentWorkload(n int) identity.WorkloadID {
	id, _ := identity.ParseWorkloadID(fmt.Sprintf("workload_%032x", n))
	return id
}
