package farmconfig

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/le0xdon/le0xfarm/internal/farmerr"
	"github.com/le0xdon/le0xfarm/internal/farmmodel"
	"github.com/le0xdon/le0xfarm/internal/identity"
)

func (service *Service) GetControllerRestoreState(ctx context.Context) (farmmodel.ControllerRestoreState, error) {
	var state farmmodel.ControllerRestoreState
	var required bool
	var backupID, controllerID, farmID, trustFingerprint string
	var restoredAt int64
	err := service.db.QueryRowContext(ctx, `SELECT restore_required,restore_backup_id,source_controller_id,source_farm_id,source_trust_fingerprint,restored_at_ns FROM controller_backup_state WHERE singleton=1`).Scan(&required, &backupID, &controllerID, &farmID, &trustFingerprint, &restoredAt)
	if err != nil {
		return state, wrapDB("read Controller restore state", err)
	}
	state.Required = required
	state.BackupID = backupID
	if backupID == "" {
		if required || controllerID != "" || farmID != "" || trustFingerprint != "" || restoredAt != 0 {
			return farmmodel.ControllerRestoreState{}, corrupt("stored Controller restore state is invalid", nil)
		}
		return state, nil
	}
	if !validBackupID(backupID) || restoredAt <= 0 {
		return farmmodel.ControllerRestoreState{}, corrupt("stored Controller restore metadata is invalid", nil)
	}
	var parseErr error
	if state.SourceControllerID, parseErr = identity.ParseControllerID(controllerID); parseErr != nil {
		return farmmodel.ControllerRestoreState{}, corrupt("stored restore ControllerID is invalid", parseErr)
	}
	if state.SourceFarmID, parseErr = identity.ParseFarmID(farmID); parseErr != nil {
		return farmmodel.ControllerRestoreState{}, corrupt("stored restore FarmID is invalid", parseErr)
	}
	if !validTrustFingerprint(trustFingerprint) {
		return farmmodel.ControllerRestoreState{}, corrupt("stored restore trust fingerprint is invalid", nil)
	}
	state.SourceTrustFingerprint = trustFingerprint
	state.RestoredAt = time.Unix(0, restoredAt).UTC()
	return state, nil
}

func (service *Service) GetRestoreHostBarrier(ctx context.Context, hostID identity.HostID) (farmmodel.RestoreHostBarrier, bool, error) {
	if err := hostID.Validate(); err != nil {
		return farmmodel.RestoreHostBarrier{}, false, typed(farmerr.CONFIG_CONFLICT, "invalid restore HostID", err)
	}
	barrier, err := scanRestoreBarrier(service.db.QueryRowContext(ctx, `SELECT host_id,backup_id,revision,status,reason_code,updated_at_ns FROM restore_host_barriers WHERE host_id=?`, hostID.String()))
	if errors.Is(err, sql.ErrNoRows) {
		return farmmodel.RestoreHostBarrier{}, false, nil
	}
	if err != nil {
		return farmmodel.RestoreHostBarrier{}, false, wrapDB("read restore Host barrier", err)
	}
	return barrier, true, nil
}

func (service *Service) ListRestoreHostBarriers(ctx context.Context) ([]farmmodel.RestoreHostBarrier, error) {
	rows, err := service.db.QueryContext(ctx, `SELECT host_id,backup_id,revision,status,reason_code,updated_at_ns FROM restore_host_barriers ORDER BY host_id`)
	if err != nil {
		return nil, wrapDB("list restore Host barriers", err)
	}
	defer rows.Close()
	var result []farmmodel.RestoreHostBarrier
	for rows.Next() {
		barrier, err := scanRestoreBarrier(rows)
		if err != nil {
			return nil, wrapDB("list restore Host barriers", err)
		}
		result = append(result, barrier)
	}
	return result, wrapDB("list restore Host barriers", rows.Err())
}

func (service *Service) MarkRestoreHostConflict(ctx context.Context, hostID identity.HostID, backupID string, expectedRevision uint64) (farmmodel.RestoreHostBarrier, error) {
	if err := hostID.Validate(); err != nil || !validBackupID(backupID) || expectedRevision == 0 {
		return farmmodel.RestoreHostBarrier{}, typed(farmerr.CONFIG_CONFLICT, "invalid restore barrier transition", err)
	}
	var result farmmodel.RestoreHostBarrier
	err := service.writeTx(ctx, func(tx *sql.Tx) error {
		current, err := scanRestoreBarrier(tx.QueryRowContext(ctx, `SELECT host_id,backup_id,revision,status,reason_code,updated_at_ns FROM restore_host_barriers WHERE host_id=?`, hostID.String()))
		if err != nil {
			return err
		}
		if current.BackupID != backupID || current.Revision != expectedRevision {
			return revisionConflict("RestoreHostBarrier", expectedRevision, current.Revision)
		}
		if current.Status == farmmodel.RestoreBarrierConflict && current.ReasonCode == farmerr.RESTORE_RECONCILIATION_REQUIRED {
			result = current
			return nil
		}
		current.Status = farmmodel.RestoreBarrierConflict
		current.ReasonCode = farmerr.RESTORE_RECONCILIATION_REQUIRED
		current.Revision++
		current.UpdatedAt = service.clock().UTC()
		if _, err := tx.ExecContext(ctx, `UPDATE restore_host_barriers SET revision=?,status=?,reason_code=?,updated_at_ns=? WHERE host_id=?`, current.Revision, current.Status, current.ReasonCode, current.UpdatedAt.UnixNano(), hostID.String()); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE controller_backup_state SET persistent_revision=persistent_revision+1 WHERE singleton=1`); err != nil {
			return err
		}
		result = current
		return nil
	})
	return result, wrapDB("mark restore Host conflict", err)
}

func (service *Service) ClearRestoreHostBarrier(ctx context.Context, hostID identity.HostID, backupID string, expectedRevision uint64) error {
	if err := hostID.Validate(); err != nil || !validBackupID(backupID) || expectedRevision == 0 {
		return typed(farmerr.CONFIG_CONFLICT, "invalid restore barrier transition", err)
	}
	err := service.writeTx(ctx, func(tx *sql.Tx) error {
		current, err := scanRestoreBarrier(tx.QueryRowContext(ctx, `SELECT host_id,backup_id,revision,status,reason_code,updated_at_ns FROM restore_host_barriers WHERE host_id=?`, hostID.String()))
		if err != nil {
			return err
		}
		if current.BackupID != backupID || current.Revision != expectedRevision {
			return revisionConflict("RestoreHostBarrier", expectedRevision, current.Revision)
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM restore_host_barriers WHERE host_id=?", hostID.String()); err != nil {
			return err
		}
		var remaining int
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM restore_host_barriers").Scan(&remaining); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE controller_backup_state SET persistent_revision=persistent_revision+1,restore_required=? WHERE singleton=1`, remaining != 0); err != nil {
			return err
		}
		return nil
	})
	return wrapDB("clear restore Host barrier", err)
}

func scanRestoreBarrier(row scanner) (farmmodel.RestoreHostBarrier, error) {
	var value farmmodel.RestoreHostBarrier
	var hostID string
	var updated int64
	if err := row.Scan(&hostID, &value.BackupID, &value.Revision, &value.Status, &value.ReasonCode, &updated); err != nil {
		return farmmodel.RestoreHostBarrier{}, err
	}
	var err error
	if value.HostID, err = identity.ParseHostID(hostID); err != nil || !validBackupID(value.BackupID) || value.Revision == 0 || updated <= 0 {
		return farmmodel.RestoreHostBarrier{}, corrupt("stored restore Host barrier is invalid", err)
	}
	if value.Status != farmmodel.RestoreBarrierPending && value.Status != farmmodel.RestoreBarrierConflict || value.ReasonCode != farmerr.RESTORE_RECONCILIATION_REQUIRED {
		return farmmodel.RestoreHostBarrier{}, corrupt("stored restore Host barrier classification is invalid", nil)
	}
	value.UpdatedAt = time.Unix(0, updated).UTC()
	return value, nil
}

func validBackupID(value string) bool {
	if len(value) != len("backup_")+32 || !strings.HasPrefix(value, "backup_") {
		return false
	}
	for _, char := range value[len("backup_"):] {
		if char < '0' || char > '9' && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

func validTrustFingerprint(value string) bool {
	if len(value) != len("SHA256:")+64 || !strings.HasPrefix(value, "SHA256:") {
		return false
	}
	for _, char := range value[len("SHA256:"):] {
		if char < '0' || char > '9' && (char < 'A' || char > 'F') {
			return false
		}
	}
	return true
}
