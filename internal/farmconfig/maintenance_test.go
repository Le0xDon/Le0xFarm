package farmconfig

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/le0xdon/le0xfarm/internal/farmerr"
	"github.com/le0xdon/le0xfarm/internal/identity"
)

func TestMaintenanceHoldPersistsAndUsesRevisions(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "controller")
	db, service := newService(t, dir, Options{})
	hostID, _ := identity.ParseHostID("host_0123456789abcdef0123456789abcdef")
	if _, exists, err := service.GetMaintenanceHold(ctx, hostID); err != nil || exists {
		t.Fatalf("absent hold exists=%t err=%v", exists, err)
	}
	active, err := service.SetMaintenanceHold(ctx, hostID, 0, true, "replace fan")
	if err != nil || !active.Active || active.Revision != 1 || active.Reason != "replace fan" {
		t.Fatalf("active hold=%+v err=%v", active, err)
	}
	idempotent, err := service.SetMaintenanceHold(ctx, hostID, 1, true, "replace fan")
	if err != nil || idempotent.Revision != 1 {
		t.Fatalf("idempotent hold=%+v err=%v", idempotent, err)
	}
	if _, err := service.SetMaintenanceHold(ctx, hostID, 0, false, ""); maintenanceCode(err) != farmerr.REVISION_CONFLICT {
		t.Fatalf("stale revision error=%v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, service = newService(t, dir, Options{})
	defer db.Close()
	persisted, exists, err := service.GetMaintenanceHold(ctx, hostID)
	if err != nil || !exists || !persisted.Active || persisted.Revision != 1 || persisted.Reason != "replace fan" {
		t.Fatalf("persisted hold=%+v exists=%t err=%v", persisted, exists, err)
	}
	released, err := service.SetMaintenanceHold(ctx, hostID, 1, false, "")
	if err != nil || released.Active || released.Revision != 2 {
		t.Fatalf("released hold=%+v err=%v", released, err)
	}
}

func TestMaintenanceHoldReasonIsBoundedAndSingleLine(t *testing.T) {
	db, service := newService(t, t.TempDir(), Options{})
	defer db.Close()
	hostID, _ := identity.NewHostID()
	for _, reason := range []string{"bad\nreason", string(make([]byte, 513))} {
		if _, err := service.SetMaintenanceHold(context.Background(), hostID, 0, true, reason); maintenanceCode(err) != farmerr.CONFIG_CONFLICT {
			t.Fatalf("reason accepted: error=%v", err)
		}
	}
}

func maintenanceCode(err error) farmerr.Code {
	code, _ := farmerr.CodeOf(err)
	return code
}
