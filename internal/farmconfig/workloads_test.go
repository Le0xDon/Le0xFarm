package farmconfig

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/le0xdon/le0xfarm/internal/farmerr"
	"github.com/le0xdon/le0xfarm/internal/farmmodel"
	"github.com/le0xdon/le0xfarm/internal/identity"
)

func TestDesiredWorkloadCRUDSnapshotAndReopen(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "controller")
	db, service := newService(t, dir, Options{})
	pool, wallet, profile := baseObjects(t, service)
	workload, err := service.CreateDesiredWorkload(ctx, workloadContent(profile.ProfileID, hostID(1), farmmodel.DesiredRunning, farmmodel.ResourceClaim{CPU: true}))
	if err != nil {
		t.Fatal(err)
	}
	if workload.Meta.Origin != farmmodel.OriginController || workload.Meta.Revision != 1 || workload.DesiredGeneration != 1 {
		t.Fatalf("created=%+v", workload)
	}
	snapshot, err := service.GetCurrentResolvedSnapshot(ctx, workload.WorkloadID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.ExecutionID.Validate() != nil || snapshot.Plan.ExecutionID != snapshot.ExecutionID || snapshot.PoolID != pool.PoolID || snapshot.WalletID != wallet.WalletID {
		t.Fatalf("snapshot=%+v", snapshot)
	}
	initialExecution := snapshot.ExecutionID
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, service = newService(t, dir, Options{})
	defer db.Close()
	loaded, err := service.GetDesiredWorkload(ctx, workload.WorkloadID)
	if err != nil {
		t.Fatal(err)
	}
	current, err := service.GetCurrentResolvedSnapshot(ctx, workload.WorkloadID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.DesiredGeneration != 1 || current.ExecutionID != initialExecution {
		t.Fatal("generation/execution changed across reopen")
	}
	items, err := service.ListDesiredWorkloads(ctx)
	if err != nil || len(items) != 1 {
		t.Fatalf("list=%v err=%v", items, err)
	}
	rename := loaded.DesiredWorkloadContent
	rename.Name = "renamed"
	updated, err := service.UpdateDesiredWorkload(ctx, loaded.WorkloadID, 1, rename)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Meta.Revision != 2 || updated.DesiredGeneration != 1 || updated.Meta.Origin != farmmodel.OriginController {
		t.Fatalf("rename=%+v", updated)
	}
	current, err = service.GetCurrentResolvedSnapshot(ctx, workload.WorkloadID)
	if err != nil || current.ExecutionID != initialExecution {
		t.Fatal("rename replaced execution")
	}
	_, err = service.UpdateDesiredWorkload(ctx, loaded.WorkloadID, 1, rename)
	assertCode(t, err, farmerr.REVISION_CONFLICT)
	assertCode(t, service.DeleteMiningProfile(ctx, profile.ProfileID, profile.Meta.Revision), farmerr.REFERENCE_IN_USE)
	assertCode(t, service.DeleteDesiredWorkload(ctx, workload.WorkloadID, 2), farmerr.REFERENCE_IN_USE)
}

func TestStoppedNeverRunningWorkloadCanBeDeleted(t *testing.T) {
	db, service := newService(t, t.TempDir(), Options{})
	defer db.Close()
	_, _, profile := baseObjects(t, service)
	workload, err := service.CreateDesiredWorkload(context.Background(), workloadContent(profile.ProfileID, hostID(1), farmmodel.DesiredStopped, farmmodel.ResourceClaim{CPU: true}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.GetCurrentResolvedSnapshot(context.Background(), workload.WorkloadID); code(err) != farmerr.NOT_FOUND {
		t.Fatalf("current snapshot err=%v", err)
	}
	if err := service.DeleteDesiredWorkload(context.Background(), workload.WorkloadID, 1); err != nil {
		t.Fatal(err)
	}
}

func TestDesiredWorkloadHostMoveIsRejected(t *testing.T) {
	db, service := newService(t, t.TempDir(), Options{})
	defer db.Close()
	_, _, profile := baseObjects(t, service)
	workload, err := service.CreateDesiredWorkload(context.Background(), workloadContent(profile.ProfileID, hostID(1), farmmodel.DesiredRunning, farmmodel.ResourceClaim{CPU: true}))
	if err != nil {
		t.Fatal(err)
	}
	before, err := service.GetCurrentResolvedSnapshot(context.Background(), workload.WorkloadID)
	if err != nil {
		t.Fatal(err)
	}
	moved := workload.DesiredWorkloadContent
	moved.HostID = hostID(2)
	if _, err := service.UpdateDesiredWorkload(context.Background(), workload.WorkloadID, workload.Meta.Revision, moved); code(err) != farmerr.CONFIG_CONFLICT {
		t.Fatalf("unsafe HostID move error=%v", err)
	}
	loaded, err := service.GetDesiredWorkload(context.Background(), workload.WorkloadID)
	if err != nil {
		t.Fatal(err)
	}
	after, err := service.GetCurrentResolvedSnapshot(context.Background(), workload.WorkloadID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.HostID != hostID(1) || loaded.Meta.Revision != workload.Meta.Revision || after.ExecutionID != before.ExecutionID {
		t.Fatalf("rejected move changed persisted ownership: workload=%+v snapshot=%+v", loaded, after)
	}
}

func TestBlockedGenerationPersistsAndExplicitRetryCreatesOneSnapshot(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, service := newService(t, dir, Options{})
	_, _, profile := baseObjects(t, service)
	workload, err := service.CreateDesiredWorkload(ctx, workloadContent(profile.ProfileID, hostID(1), farmmodel.DesiredRunning, farmmodel.ResourceClaim{CPU: true}))
	if err != nil {
		t.Fatal(err)
	}
	before, _ := service.GetCurrentResolvedSnapshot(ctx, workload.WorkloadID)
	if err := service.BlockDesiredGeneration(ctx, workload.WorkloadID, workload.DesiredGeneration+1, farmerr.PROCESS_CRASHED, "stale"); err != nil {
		t.Fatal(err)
	}
	if _, exists, err := service.GetWorkloadRuntimeBinding(ctx, workload.WorkloadID); err != nil || exists {
		t.Fatalf("stale generation was blocked: exists=%t err=%v", exists, err)
	}
	if err := service.BlockDesiredGeneration(ctx, workload.WorkloadID, workload.DesiredGeneration, farmerr.PROCESS_CRASHED, "terminal failure"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, service = newService(t, dir, Options{})
	defer db.Close()
	binding, exists, err := service.GetWorkloadRuntimeBinding(ctx, workload.WorkloadID)
	if err != nil || !exists || binding.BlockedGeneration != workload.DesiredGeneration || binding.ErrorCode != farmerr.PROCESS_CRASHED {
		t.Fatalf("blocked generation did not survive reopen: %+v exists=%t err=%v", binding, exists, err)
	}
	retried, err := service.RetryWorkload(ctx, workload.WorkloadID, workload.Meta.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if retried.Meta.Revision != workload.Meta.Revision+1 || retried.DesiredGeneration != workload.DesiredGeneration+1 || retried.EffectiveHash != workload.EffectiveHash || retried.Meta.ContentHash != workload.Meta.ContentHash {
		t.Fatalf("invalid retry mutation: before=%+v after=%+v", workload, retried)
	}
	after, err := service.GetCurrentResolvedSnapshot(ctx, workload.WorkloadID)
	if err != nil {
		t.Fatal(err)
	}
	if after.ExecutionID == before.ExecutionID || after.ResolvedHash != before.ResolvedHash || after.Plan.Ownership.DesiredGeneration != retried.DesiredGeneration || after.Plan.Ownership.WorkloadID != workload.WorkloadID {
		t.Fatalf("invalid retry snapshot: before=%+v after=%+v", before, after)
	}
	snapshots, _ := service.ListResolvedSnapshots(ctx, workload.WorkloadID)
	if len(snapshots) != 2 {
		t.Fatalf("retry created %d snapshots", len(snapshots))
	}
	if _, exists, err := service.GetWorkloadRuntimeBinding(ctx, workload.WorkloadID); err != nil || exists {
		t.Fatalf("retry did not clear blocked binding: exists=%t err=%v", exists, err)
	}
}

func TestRetirementStorageFinalizationPreservesHistoricalSnapshots(t *testing.T) {
	ctx := context.Background()
	db, service := newService(t, t.TempDir(), Options{})
	defer db.Close()
	_, _, profile := baseObjects(t, service)
	workload, err := service.CreateDesiredWorkload(ctx, workloadContent(profile.ProfileID, hostID(1), farmmodel.DesiredRunning, farmmodel.ResourceClaim{CPU: true}))
	if err != nil {
		t.Fatal(err)
	}
	stoppedContent := workload.DesiredWorkloadContent
	stoppedContent.RunState = farmmodel.DesiredStopped
	workload, err = service.UpdateDesiredWorkload(ctx, workload.WorkloadID, workload.Meta.Revision, stoppedContent)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.FinalizeRetiredDesiredWorkloadDeletion(ctx, workload.WorkloadID, workload.Meta.Revision); err != nil {
		t.Fatal(err)
	}
	if _, err := service.GetDesiredWorkload(ctx, workload.WorkloadID); code(err) != farmerr.NOT_FOUND {
		t.Fatalf("desired workload still exists: %v", err)
	}
	snapshots, err := service.ListResolvedSnapshots(ctx, workload.WorkloadID)
	if err != nil || len(snapshots) != 1 {
		t.Fatalf("historical snapshots were not retained: %v err=%v", snapshots, err)
	}
}

func TestRunStateTransitionsAllocateOnlyRunningExecution(t *testing.T) {
	db, service := newService(t, t.TempDir(), Options{})
	defer db.Close()
	_, _, profile := baseObjects(t, service)
	workload, err := service.CreateDesiredWorkload(context.Background(), workloadContent(profile.ProfileID, hostID(1), farmmodel.DesiredStopped, farmmodel.ResourceClaim{CPU: true}))
	if err != nil {
		t.Fatal(err)
	}
	running := workload.DesiredWorkloadContent
	running.RunState = farmmodel.DesiredRunning
	workload, err = service.UpdateDesiredWorkload(context.Background(), workload.WorkloadID, 1, running)
	if err != nil || workload.DesiredGeneration != 2 {
		t.Fatalf("RUNNING transition: %+v %v", workload, err)
	}
	first, err := service.GetCurrentResolvedSnapshot(context.Background(), workload.WorkloadID)
	if err != nil {
		t.Fatal(err)
	}
	stopped := workload.DesiredWorkloadContent
	stopped.RunState = farmmodel.DesiredStopped
	workload, err = service.UpdateDesiredWorkload(context.Background(), workload.WorkloadID, 2, stopped)
	if err != nil || workload.DesiredGeneration != 3 {
		t.Fatalf("STOPPED transition: %+v %v", workload, err)
	}
	snapshots, _ := service.ListResolvedSnapshots(context.Background(), workload.WorkloadID)
	if len(snapshots) != 1 || snapshots[0].ExecutionID != first.ExecutionID {
		t.Fatal("STOPPED transition changed immutable snapshot history")
	}
}

func TestResourceClaimConflicts(t *testing.T) {
	tests := []struct {
		name                  string
		firstHost, secondHost identity.HostID
		first, second         farmmodel.ResourceClaim
		firstState            farmmodel.DesiredRunState
		wantConflict          bool
	}{
		{"same CPU", hostID(1), hostID(1), farmmodel.ResourceClaim{CPU: true}, farmmodel.ResourceClaim{CPU: true}, farmmodel.DesiredRunning, true},
		{"same GPU", hostID(1), hostID(1), farmmodel.ResourceClaim{DeviceIDs: []identity.DeviceID{deviceID(1)}}, farmmodel.ResourceClaim{DeviceIDs: []identity.DeviceID{deviceID(1)}}, farmmodel.DesiredRunning, true},
		{"CPU and GPU", hostID(1), hostID(1), farmmodel.ResourceClaim{CPU: true}, farmmodel.ResourceClaim{DeviceIDs: []identity.DeviceID{deviceID(1)}}, farmmodel.DesiredRunning, false},
		{"different GPU", hostID(1), hostID(1), farmmodel.ResourceClaim{DeviceIDs: []identity.DeviceID{deviceID(1)}}, farmmodel.ResourceClaim{DeviceIDs: []identity.DeviceID{deviceID(2)}}, farmmodel.DesiredRunning, false},
		{"different hosts", hostID(1), hostID(2), farmmodel.ResourceClaim{CPU: true}, farmmodel.ResourceClaim{CPU: true}, farmmodel.DesiredRunning, false},
		{"stopped does not reserve", hostID(1), hostID(1), farmmodel.ResourceClaim{CPU: true}, farmmodel.ResourceClaim{CPU: true}, farmmodel.DesiredStopped, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			db, service := newService(t, t.TempDir(), Options{})
			defer db.Close()
			_, _, profile := baseObjects(t, service)
			if _, err := service.CreateDesiredWorkload(context.Background(), workloadContent(profile.ProfileID, test.firstHost, test.firstState, test.first)); err != nil {
				t.Fatal(err)
			}
			_, err := service.CreateDesiredWorkload(context.Background(), workloadContent(profile.ProfileID, test.secondHost, farmmodel.DesiredRunning, test.second))
			if test.wantConflict {
				assertCode(t, err, farmerr.CONFIG_CONFLICT)
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestReferencedUpdatesPropagateOnlyEffectiveChanges(t *testing.T) {
	ctx := context.Background()
	db, service := newService(t, t.TempDir(), Options{})
	defer db.Close()
	pool, wallet, profile := baseObjects(t, service)
	workload, err := service.CreateDesiredWorkload(ctx, workloadContent(profile.ProfileID, hostID(1), farmmodel.DesiredRunning, farmmodel.ResourceClaim{CPU: true}))
	if err != nil {
		t.Fatal(err)
	}
	first, _ := service.GetCurrentResolvedSnapshot(ctx, workload.WorkloadID)
	poolRename := pool.PoolContent
	poolRename.Name = "renamed"
	pool, err = service.UpdatePool(ctx, pool.PoolID, 1, poolRename)
	if err != nil {
		t.Fatal(err)
	}
	walletRename := wallet.WalletRefContent
	walletRename.Name = "renamed"
	wallet, err = service.UpdateWalletRef(ctx, wallet.WalletID, 1, walletRename)
	if err != nil {
		t.Fatal(err)
	}
	profileRename := profile.MiningProfileContent
	profileRename.Name = "renamed"
	profile, err = service.UpdateMiningProfile(ctx, profile.ProfileID, 1, profileRename)
	if err != nil {
		t.Fatal(err)
	}
	workload, _ = service.GetDesiredWorkload(ctx, workload.WorkloadID)
	if workload.DesiredGeneration != 1 {
		t.Fatalf("rename generation=%d", workload.DesiredGeneration)
	}
	snapshots, _ := service.ListResolvedSnapshots(ctx, workload.WorkloadID)
	if len(snapshots) != 1 || snapshots[0].ExecutionID != first.ExecutionID {
		t.Fatal("rename created snapshot")
	}
	poolRuntime := pool.PoolContent
	poolRuntime.Address = "pool.example:444"
	pool, err = service.UpdatePool(ctx, pool.PoolID, 2, poolRuntime)
	if err != nil {
		t.Fatal(err)
	}
	workload, _ = service.GetDesiredWorkload(ctx, workload.WorkloadID)
	if workload.DesiredGeneration != 2 {
		t.Fatalf("pool generation=%d", workload.DesiredGeneration)
	}
	second, _ := service.GetCurrentResolvedSnapshot(ctx, workload.WorkloadID)
	if second.ExecutionID == first.ExecutionID {
		t.Fatal("runtime change reused ExecutionID")
	}
	walletRuntime := wallet.WalletRefContent
	walletRuntime.Address = "new-public-address"
	wallet, err = service.UpdateWalletRef(ctx, wallet.WalletID, 2, walletRuntime)
	if err != nil {
		t.Fatal(err)
	}
	profileRuntime := profile.MiningProfileContent
	profileRuntime.Package.Version = "2.0"
	profile, err = service.UpdateMiningProfile(ctx, profile.ProfileID, 2, profileRuntime)
	if err != nil {
		t.Fatal(err)
	}
	workload, _ = service.GetDesiredWorkload(ctx, workload.WorkloadID)
	workerChange := workload.DesiredWorkloadContent
	workerChange.Worker = "worker-2"
	workload, err = service.UpdateDesiredWorkload(ctx, workload.WorkloadID, workload.Meta.Revision, workerChange)
	if err != nil {
		t.Fatal(err)
	}
	if workload.DesiredGeneration != 5 {
		t.Fatalf("final generation=%d", workload.DesiredGeneration)
	}
	snapshots, err = service.ListResolvedSnapshots(ctx, workload.WorkloadID)
	if err != nil || len(snapshots) != 5 {
		t.Fatalf("snapshots=%d err=%v", len(snapshots), err)
	}
	for i, snapshot := range snapshots {
		if snapshot.DesiredGeneration != uint64(i+1) {
			t.Fatal("history is not append-only")
		}
		if i > 0 && snapshot.ExecutionID == snapshots[i-1].ExecutionID {
			t.Fatal("generation reused ExecutionID")
		}
	}
	noOp, err := service.UpdateDesiredWorkload(ctx, workload.WorkloadID, workload.Meta.Revision, workload.DesiredWorkloadContent)
	if err != nil || noOp.DesiredGeneration != 5 {
		t.Fatal("no-op bumped generation")
	}
	stop := workload.DesiredWorkloadContent
	stop.RunState = farmmodel.DesiredStopped
	stopped, err := service.UpdateDesiredWorkload(ctx, workload.WorkloadID, workload.Meta.Revision, stop)
	if err != nil {
		t.Fatal(err)
	}
	if stopped.DesiredGeneration != 6 {
		t.Fatal("STOP transition did not bump generation")
	}
	snapshots, _ = service.ListResolvedSnapshots(ctx, workload.WorkloadID)
	if len(snapshots) != 5 {
		t.Fatal("STOP created execution snapshot")
	}
}

func TestReferencedUpdateRollbackIsAtomic(t *testing.T) {
	var calls atomic.Int32
	firstExecution, _ := identity.ParseExecutionID("execution_11111111111111111111111111111111")
	options := Options{NewExecutionID: func() (identity.ExecutionID, error) {
		if calls.Add(1) == 1 {
			return firstExecution, nil
		}
		return identity.ExecutionID{}, errors.New("injected entropy failure")
	}}
	db, service := newService(t, t.TempDir(), options)
	defer db.Close()
	pool, _, profile := baseObjects(t, service)
	workload, err := service.CreateDesiredWorkload(context.Background(), workloadContent(profile.ProfileID, hostID(1), farmmodel.DesiredRunning, farmmodel.ResourceClaim{CPU: true}))
	if err != nil {
		t.Fatal(err)
	}
	changed := pool.PoolContent
	changed.Address = "other.example:999"
	_, err = service.UpdatePool(context.Background(), pool.PoolID, 1, changed)
	assertCode(t, err, farmerr.INTERNAL_ERROR)
	loadedPool, err := service.GetPool(context.Background(), pool.PoolID)
	if err != nil {
		t.Fatal(err)
	}
	loadedWorkload, _ := service.GetDesiredWorkload(context.Background(), workload.WorkloadID)
	snapshots, _ := service.ListResolvedSnapshots(context.Background(), workload.WorkloadID)
	if loadedPool.Address != pool.Address || loadedPool.Meta.Revision != 1 || loadedWorkload.DesiredGeneration != 1 || len(snapshots) != 1 {
		t.Fatal("cross-object transaction did not roll back")
	}
}

func TestStoppedConfigChangesKeepLatestIntentWithoutSnapshots(t *testing.T) {
	db, service := newService(t, t.TempDir(), Options{})
	defer db.Close()
	pool, _, profile := baseObjects(t, service)
	workload, err := service.CreateDesiredWorkload(context.Background(), workloadContent(profile.ProfileID, hostID(1), farmmodel.DesiredStopped, farmmodel.ResourceClaim{CPU: true}))
	if err != nil {
		t.Fatal(err)
	}
	for revision, port := range []string{"444", "445", "446"} {
		content := pool.PoolContent
		content.Address = "pool.example:" + port
		pool, err = service.UpdatePool(context.Background(), pool.PoolID, uint64(revision+1), content)
		if err != nil {
			t.Fatal(err)
		}
	}
	workload, _ = service.GetDesiredWorkload(context.Background(), workload.WorkloadID)
	snapshots, _ := service.ListResolvedSnapshots(context.Background(), workload.WorkloadID)
	if workload.DesiredGeneration != 4 || len(snapshots) != 0 {
		t.Fatalf("generation=%d snapshots=%d", workload.DesiredGeneration, len(snapshots))
	}
	if pool.Address != "pool.example:446" {
		t.Fatal("latest intent not persisted")
	}
}

func TestDesiredWorkloadValidationBoundaries(t *testing.T) {
	db, service := newService(t, t.TempDir(), Options{})
	defer db.Close()
	_, _, profile := baseObjects(t, service)
	tests := []farmmodel.DesiredWorkloadContent{
		workloadContent(identity.ProfileID{}, hostID(1), farmmodel.DesiredRunning, farmmodel.ResourceClaim{CPU: true}),
		workloadContent(profile.ProfileID, identity.HostID{}, farmmodel.DesiredRunning, farmmodel.ResourceClaim{CPU: true}),
		workloadContent(profile.ProfileID, hostID(1), farmmodel.DesiredRunning, farmmodel.ResourceClaim{}),
		workloadContent(profile.ProfileID, hostID(1), farmmodel.DesiredRunning, farmmodel.ResourceClaim{DeviceIDs: []identity.DeviceID{deviceID(1), deviceID(1)}}),
	}
	for _, content := range tests {
		if _, err := service.CreateDesiredWorkload(context.Background(), content); err == nil {
			t.Fatalf("invalid workload accepted: %+v", content)
		}
	}
	missing, _ := identity.ParseProfileID("profile_99999999999999999999999999999999")
	_, err := service.CreateDesiredWorkload(context.Background(), workloadContent(missing, hostID(1), farmmodel.DesiredRunning, farmmodel.ResourceClaim{CPU: true}))
	assertCode(t, err, farmerr.INVALID_REFERENCE)
}

func TestSnapshotsAreDatabaseImmutable(t *testing.T) {
	db, service := newService(t, t.TempDir(), Options{})
	defer db.Close()
	_, _, profile := baseObjects(t, service)
	workload, err := service.CreateDesiredWorkload(context.Background(), workloadContent(profile.ProfileID, hostID(1), farmmodel.DesiredRunning, farmmodel.ResourceClaim{CPU: true}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().Exec("UPDATE resolved_execution_snapshots SET resolved_hash='tampered' WHERE workload_id=?", workload.WorkloadID.String()); err == nil {
		t.Fatal("immutable snapshot accepted UPDATE")
	}
	if _, err := db.SQL().Exec("DELETE FROM resolved_execution_snapshots WHERE workload_id=?", workload.WorkloadID.String()); err == nil {
		t.Fatal("append-only snapshot accepted DELETE")
	}
}

func TestStoredWorkloadAndSnapshotCorruptionFailsClosed(t *testing.T) {
	db, service := newService(t, t.TempDir(), Options{})
	defer db.Close()
	_, _, profile := baseObjects(t, service)
	workload, err := service.CreateDesiredWorkload(context.Background(), workloadContent(profile.ProfileID, hostID(1), farmmodel.DesiredRunning, farmmodel.ResourceClaim{CPU: true}))
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := service.GetCurrentResolvedSnapshot(context.Background(), workload.WorkloadID)
	if err != nil {
		t.Fatal(err)
	}
	tampered := snapshot
	tampered.Plan.Executable = "/tmp/not-controller-resolved"
	if err := validateSnapshot(tampered); code(err) != farmerr.CONFIG_CONFLICT {
		t.Fatalf("non-canonical snapshot plan accepted: %v", err)
	}
	if _, err := db.SQL().Exec("UPDATE desired_workloads SET effective_hash='tampered' WHERE workload_id=?", workload.WorkloadID.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := service.GetDesiredWorkload(context.Background(), workload.WorkloadID); code(err) != farmerr.CONFIG_CONFLICT {
		t.Fatalf("malformed effective hash accepted: %v", err)
	}
}

func TestPoolFanoutUpdatesEveryAffectedWorkloadAtomically(t *testing.T) {
	db, service := newService(t, t.TempDir(), Options{})
	defer db.Close()
	pool, _, profile := baseObjects(t, service)
	first, _ := service.CreateDesiredWorkload(context.Background(), workloadContent(profile.ProfileID, hostID(1), farmmodel.DesiredRunning, farmmodel.ResourceClaim{CPU: true}))
	second, _ := service.CreateDesiredWorkload(context.Background(), workloadContent(profile.ProfileID, hostID(2), farmmodel.DesiredRunning, farmmodel.ResourceClaim{CPU: true}))
	changed := pool.PoolContent
	changed.TLS = !changed.TLS
	if _, err := service.UpdatePool(context.Background(), pool.PoolID, 1, changed); err != nil {
		t.Fatal(err)
	}
	for _, id := range []identity.WorkloadID{first.WorkloadID, second.WorkloadID} {
		workload, err := service.GetDesiredWorkload(context.Background(), id)
		if err != nil || workload.DesiredGeneration != 2 {
			t.Fatalf("workload %s generation=%d err=%v", id, workload.DesiredGeneration, err)
		}
		snapshots, _ := service.ListResolvedSnapshots(context.Background(), id)
		if len(snapshots) != 2 {
			t.Fatalf("workload %s snapshots=%d", id, len(snapshots))
		}
	}
}

func TestProfileIDChangeBumpsGenerationEvenWithSameRuntimeFields(t *testing.T) {
	db, service := newService(t, t.TempDir(), Options{})
	defer db.Close()
	pool, wallet, profile := baseObjects(t, service)
	second, err := service.CreateMiningProfile(context.Background(), profileContent(pool.PoolID, wallet.WalletID, "same-runtime"))
	if err != nil {
		t.Fatal(err)
	}
	workload, err := service.CreateDesiredWorkload(context.Background(), workloadContent(profile.ProfileID, hostID(1), farmmodel.DesiredRunning, farmmodel.ResourceClaim{CPU: true}))
	if err != nil {
		t.Fatal(err)
	}
	content := workload.DesiredWorkloadContent
	content.ProfileID = second.ProfileID
	updated, err := service.UpdateDesiredWorkload(context.Background(), workload.WorkloadID, 1, content)
	if err != nil {
		t.Fatal(err)
	}
	if updated.DesiredGeneration != 2 {
		t.Fatalf("ProfileID change generation=%d", updated.DesiredGeneration)
	}
	snapshots, _ := service.ListResolvedSnapshots(context.Background(), workload.WorkloadID)
	if len(snapshots) != 2 || snapshots[0].ExecutionID == snapshots[1].ExecutionID {
		t.Fatal("ProfileID change did not create exactly one replacement snapshot")
	}
}

func TestConcurrentConflictingClaimsAllowOneRunningWorkload(t *testing.T) {
	db, service := newService(t, t.TempDir(), Options{})
	defer db.Close()
	_, _, profile := baseObjects(t, service)
	start := make(chan struct{})
	results := make(chan error, 2)
	var group sync.WaitGroup
	for range 2 {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			_, err := service.CreateDesiredWorkload(context.Background(), workloadContent(profile.ProfileID, hostID(1), farmmodel.DesiredRunning, farmmodel.ResourceClaim{CPU: true}))
			results <- err
		}()
	}
	close(start)
	group.Wait()
	close(results)
	success, conflicts := 0, 0
	for err := range results {
		if err == nil {
			success++
		} else if code(err) == farmerr.CONFIG_CONFLICT {
			conflicts++
		} else {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if success != 1 || conflicts != 1 {
		t.Fatalf("success=%d conflicts=%d", success, conflicts)
	}
}

func baseObjects(t *testing.T, service *Service) (farmmodel.Pool, farmmodel.WalletRef, farmmodel.MiningProfile) {
	t.Helper()
	ctx := context.Background()
	pool, err := service.CreatePool(ctx, poolContent("pool"))
	if err != nil {
		t.Fatal(err)
	}
	wallet, err := service.CreateWalletRef(ctx, walletContent("wallet"))
	if err != nil {
		t.Fatal(err)
	}
	profile, err := service.CreateMiningProfile(ctx, profileContent(pool.PoolID, wallet.WalletID, "profile"))
	if err != nil {
		t.Fatal(err)
	}
	return pool, wallet, profile
}
func workloadContent(profile identity.ProfileID, host identity.HostID, state farmmodel.DesiredRunState, claim farmmodel.ResourceClaim) farmmodel.DesiredWorkloadContent {
	return farmmodel.DesiredWorkloadContent{Name: "workload", HostID: host, ProfileID: profile, RunState: state, Worker: "worker-1", Resources: claim}
}
func hostID(n int) identity.HostID { id, _ := identity.ParseHostID("host_" + hexID(n)); return id }
func deviceID(n int) identity.DeviceID {
	id, _ := identity.ParseDeviceID("device_" + hexID(n))
	return id
}
func hexID(n int) string          { return fmt.Sprintf("%032x", n) }
func code(err error) farmerr.Code { value, _ := farmerr.CodeOf(err); return value }
