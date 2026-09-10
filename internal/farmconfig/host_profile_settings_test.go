package farmconfig

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/le0xdon/le0xfarm/internal/farmerr"
	"github.com/le0xdon/le0xfarm/internal/farmmodel"
	"github.com/le0xdon/le0xfarm/internal/model"
)

func TestHostProfileSettingsPersistenceRevisionUniquenessAndReset(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "controller")
	db, service := newService(t, dir, Options{})
	_, _, profile := baseObjects(t, service)
	host := hostID(1)
	threads := uint32(30)
	created, err := service.CreateHostProfileSettings(ctx, host, profile.ProfileID, farmmodel.HostProfileSettingsContent{CPUThreads: &threads})
	if err != nil {
		t.Fatal(err)
	}
	if created.Meta.Revision != 1 || created.Meta.Origin != farmmodel.OriginController || *created.CPUThreads != 30 {
		t.Fatalf("created=%+v", created)
	}
	_, err = service.CreateHostProfileSettings(ctx, host, profile.ProfileID, farmmodel.HostProfileSettingsContent{CPUThreads: &threads})
	assertCode(t, err, farmerr.ALREADY_EXISTS)
	zero := uint32(0)
	_, err = service.CreateHostProfileSettings(ctx, hostID(2), profile.ProfileID, farmmodel.HostProfileSettingsContent{CPUThreads: &zero})
	assertCode(t, err, farmerr.CONFIG_CONFLICT)
	_, err = service.CreateHostProfileSettings(ctx, hostID(2), profile.ProfileID, farmmodel.HostProfileSettingsContent{})
	assertCode(t, err, farmerr.CONFIG_CONFLICT)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, service = newService(t, dir, Options{})
	defer db.Close()
	loaded, err := service.GetHostProfileSettings(ctx, host, profile.ProfileID)
	if err != nil || loaded.Meta.Revision != 1 || loaded.CPUThreads == nil || *loaded.CPUThreads != 30 {
		t.Fatalf("reloaded=%+v err=%v", loaded, err)
	}
	if items, err := service.ListHostProfileSettings(ctx); err != nil || len(items) != 1 {
		t.Fatalf("list=%+v err=%v", items, err)
	}
	assertCode(t, service.DeleteMiningProfile(ctx, profile.ProfileID, profile.Meta.Revision), farmerr.REFERENCE_IN_USE)
	threads = 28
	updated, err := service.UpdateHostProfileSettings(ctx, host, profile.ProfileID, 1, farmmodel.HostProfileSettingsContent{CPUThreads: &threads})
	if err != nil || updated.Meta.Revision != 2 {
		t.Fatalf("updated=%+v err=%v", updated, err)
	}
	_, err = service.UpdateHostProfileSettings(ctx, host, profile.ProfileID, 1, farmmodel.HostProfileSettingsContent{CPUThreads: &threads})
	assertCode(t, err, farmerr.REVISION_CONFLICT)
	if err := service.DeleteHostProfileSettings(ctx, host, profile.ProfileID, 2); err != nil {
		t.Fatal(err)
	}
	_, err = service.GetHostProfileSettings(ctx, host, profile.ProfileID)
	assertCode(t, err, farmerr.NOT_FOUND)
}

func TestHostProfileSettingsEffectiveFanoutAndHostIsolation(t *testing.T) {
	ctx := context.Background()
	db, service := newService(t, t.TempDir(), Options{})
	defer db.Close()
	_, _, profile := baseObjects(t, service)
	hostA, hostB := hostID(1), hostID(2)
	workA, err := service.CreateDesiredWorkload(ctx, workloadContent(profile.ProfileID, hostA, farmmodel.DesiredRunning, farmmodel.ResourceClaim{CPU: true}))
	if err != nil {
		t.Fatal(err)
	}
	workB, err := service.CreateDesiredWorkload(ctx, workloadContent(profile.ProfileID, hostB, farmmodel.DesiredRunning, farmmodel.ResourceClaim{CPU: true}))
	if err != nil {
		t.Fatal(err)
	}
	beforeA, _ := service.GetCurrentResolvedSnapshot(ctx, workA.WorkloadID)
	beforeB, _ := service.GetCurrentResolvedSnapshot(ctx, workB.WorkloadID)
	threads := uint32(30)
	settings, err := service.CreateHostProfileSettings(ctx, hostA, profile.ProfileID, farmmodel.HostProfileSettingsContent{CPUThreads: &threads})
	if err != nil {
		t.Fatal(err)
	}
	afterA, _ := service.GetCurrentResolvedSnapshot(ctx, workA.WorkloadID)
	afterB, _ := service.GetCurrentResolvedSnapshot(ctx, workB.WorkloadID)
	if afterA.ExecutionID == beforeA.ExecutionID || afterA.Plan.CPUThreads == nil || *afterA.Plan.CPUThreads != 30 || afterA.HostProfileSettingsRevision != 1 {
		t.Fatalf("Host A did not get override replacement: %+v", afterA)
	}
	if afterB.ExecutionID != beforeB.ExecutionID || afterB.Plan.CPUThreads == nil || *afterB.Plan.CPUThreads != 2 {
		t.Fatalf("Host B inherited Host A settings: %+v", afterB)
	}
	if err := service.DeleteHostProfileSettings(ctx, hostA, profile.ProfileID, settings.Meta.Revision); err != nil {
		t.Fatal(err)
	}
	loadedA, _ := service.GetDesiredWorkload(ctx, workA.WorkloadID)
	if loadedA.DesiredGeneration != 3 {
		t.Fatalf("removing override did not restore default with one generation: %d", loadedA.DesiredGeneration)
	}
	currentA, _ := service.GetCurrentResolvedSnapshot(ctx, workA.WorkloadID)
	if currentA.Plan.CPUThreads == nil || *currentA.Plan.CPUThreads != 2 {
		t.Fatal("effective default was not restored")
	}
	threads = 2
	settings, err = service.CreateHostProfileSettings(ctx, hostA, profile.ProfileID, farmmodel.HostProfileSettingsContent{CPUThreads: &threads})
	if err != nil {
		t.Fatal(err)
	}
	unchangedA, _ := service.GetDesiredWorkload(ctx, workA.WorkloadID)
	if unchangedA.DesiredGeneration != loadedA.DesiredGeneration {
		t.Fatal("creating override equal to inherited default restarted workload")
	}
	if err := service.DeleteHostProfileSettings(ctx, hostA, profile.ProfileID, settings.Meta.Revision); err != nil {
		t.Fatal(err)
	}
	stillUnchangedA, _ := service.GetDesiredWorkload(ctx, workA.WorkloadID)
	if stillUnchangedA.DesiredGeneration != loadedA.DesiredGeneration {
		t.Fatal("removing override equal to inherited default restarted workload")
	}
}

func TestValidateSnapshotForStartUsesFreshCapacityAndCurrentOverrides(t *testing.T) {
	ctx := context.Background()
	db, service := newService(t, t.TempDir(), Options{})
	defer db.Close()
	_, _, profile := baseObjects(t, service)
	host := hostID(1)
	threads := uint32(30)
	if _, err := service.CreateHostProfileSettings(ctx, host, profile.ProfileID, farmmodel.HostProfileSettingsContent{CPUThreads: &threads}); err != nil {
		t.Fatal(err)
	}
	workload, err := service.CreateDesiredWorkload(ctx, workloadContent(profile.ProfileID, host, farmmodel.DesiredRunning, farmmodel.ResourceClaim{CPU: true}))
	if err != nil {
		t.Fatal(err)
	}
	snapshot, _ := service.GetCurrentResolvedSnapshot(ctx, workload.WorkloadID)
	if err := service.ValidateResolvedSnapshotForStart(ctx, snapshot, model.Inventory{Host: model.Host{HostID: host}, CPU: model.CPU{Threads: 32}}); err != nil {
		t.Fatal(err)
	}
	err = service.ValidateResolvedSnapshotForStart(ctx, snapshot, model.Inventory{Host: model.Host{HostID: host}, CPU: model.CPU{Threads: 16}})
	assertCode(t, err, farmerr.INCOMPATIBLE_HARDWARE)
	if snapshot.Plan.CPUThreads == nil || *snapshot.Plan.CPUThreads != 30 {
		t.Fatal("validation silently clamped immutable plan")
	}
}

func TestMalformedStoredHostProfileSettingsFailsClosed(t *testing.T) {
	db, service := newService(t, t.TempDir(), Options{})
	defer db.Close()
	_, _, profile := baseObjects(t, service)
	host := hostID(1)
	threads := uint32(3)
	if _, err := service.CreateHostProfileSettings(context.Background(), host, profile.ProfileID, farmmodel.HostProfileSettingsContent{CPUThreads: &threads}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().Exec("UPDATE host_profile_settings SET content_hash='tampered' WHERE host_id=? AND profile_id=?", host.String(), profile.ProfileID.String()); err != nil {
		t.Fatal(err)
	}
	_, err := service.GetHostProfileSettings(context.Background(), host, profile.ProfileID)
	assertCode(t, err, farmerr.CONFIG_CONFLICT)
}
