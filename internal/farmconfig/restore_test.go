package farmconfig

import (
	"context"
	"testing"
	"time"

	"github.com/le0xdon/le0xfarm/internal/controllerdb"
	"github.com/le0xdon/le0xfarm/internal/farmerr"
	"github.com/le0xdon/le0xfarm/internal/farmmodel"
	"github.com/le0xdon/le0xfarm/internal/identity"
	"github.com/le0xdon/le0xfarm/internal/packagecatalog"
)

func TestRestoreBarrierLifecycleIsRevisionedAndDurable(t *testing.T) {
	ctx := context.Background()
	db, err := controllerdb.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	catalog, err := packagecatalog.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	service, err := New(db, catalog, Options{})
	if err != nil {
		t.Fatal(err)
	}
	host, _ := identity.ParseHostID("host_0123456789abcdef0123456789abcdef")
	controller, _ := identity.ParseControllerID("controller_0123456789abcdef0123456789abcdef")
	farm, _ := identity.ParseFarmID("farm_0123456789abcdef0123456789abcdef")
	backupID := "backup_0123456789abcdef0123456789abcdef"
	trustFingerprint := "SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	now := time.Now().UTC()
	if _, err := db.SQL().ExecContext(ctx, `INSERT INTO restore_host_barriers(host_id,backup_id,revision,status,reason_code,updated_at_ns) VALUES(?,?,?,?,?,?)`, host.String(), backupID, 1, farmmodel.RestoreBarrierPending, farmerr.RESTORE_RECONCILIATION_REQUIRED, now.UnixNano()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().ExecContext(ctx, `UPDATE controller_backup_state SET restore_required=1,restore_backup_id=?,source_controller_id=?,source_farm_id=?,source_trust_fingerprint=?,restored_at_ns=? WHERE singleton=1`, backupID, controller.String(), farm.String(), trustFingerprint, now.UnixNano()); err != nil {
		t.Fatal(err)
	}
	state, err := service.GetControllerRestoreState(ctx)
	if err != nil || !state.Required || state.BackupID != backupID || state.SourceControllerID != controller || state.SourceFarmID != farm || state.SourceTrustFingerprint != trustFingerprint {
		t.Fatalf("restore state=%+v err=%v", state, err)
	}
	barrier, exists, err := service.GetRestoreHostBarrier(ctx, host)
	if err != nil || !exists || barrier.Status != farmmodel.RestoreBarrierPending {
		t.Fatalf("pending barrier=%+v exists=%t err=%v", barrier, exists, err)
	}
	barrier, err = service.MarkRestoreHostConflict(ctx, host, backupID, 1)
	if err != nil || barrier.Status != farmmodel.RestoreBarrierConflict || barrier.Revision != 2 {
		t.Fatalf("conflict barrier=%+v err=%v", barrier, err)
	}
	if _, err := service.MarkRestoreHostConflict(ctx, host, backupID, 1); codeOf(err) != farmerr.REVISION_CONFLICT {
		t.Fatalf("stale barrier update error=%v", err)
	}
	if err := service.ClearRestoreHostBarrier(ctx, host, backupID, 2); err != nil {
		t.Fatal(err)
	}
	if _, exists, err := service.GetRestoreHostBarrier(ctx, host); err != nil || exists {
		t.Fatalf("cleared barrier exists=%t err=%v", exists, err)
	}
	state, err = service.GetControllerRestoreState(ctx)
	if err != nil || state.Required {
		t.Fatalf("cleared restore state=%+v err=%v", state, err)
	}
}

func codeOf(err error) farmerr.Code {
	code, _ := farmerr.CodeOf(err)
	return code
}
