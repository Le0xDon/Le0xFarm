package farmconfig

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/le0xdon/le0xfarm/internal/farmerr"
	"github.com/le0xdon/le0xfarm/internal/farmmodel"
	"github.com/le0xdon/le0xfarm/internal/identity"
)

const maintenanceSelect = `SELECT host_id,active,revision,reason,created_at_ns,updated_at_ns FROM maintenance_holds`

func (service *Service) GetMaintenanceHold(ctx context.Context, hostID identity.HostID) (farmmodel.MaintenanceHold, bool, error) {
	if err := hostID.Validate(); err != nil {
		return farmmodel.MaintenanceHold{}, false, typed(farmerr.CONFIG_CONFLICT, "invalid Maintenance Hold HostID", err)
	}
	hold, err := scanMaintenanceHold(service.db.QueryRowContext(ctx, maintenanceSelect+" WHERE host_id=?", hostID.String()))
	if errors.Is(err, sql.ErrNoRows) {
		return farmmodel.MaintenanceHold{HostID: hostID}, false, nil
	}
	if err != nil {
		return farmmodel.MaintenanceHold{}, false, wrapDB("get Maintenance Hold", err)
	}
	return hold, true, nil
}

func (service *Service) ListMaintenanceHolds(ctx context.Context) ([]farmmodel.MaintenanceHold, error) {
	rows, err := service.db.QueryContext(ctx, maintenanceSelect+" ORDER BY host_id")
	if err != nil {
		return nil, wrapDB("list Maintenance Holds", err)
	}
	defer rows.Close()
	var result []farmmodel.MaintenanceHold
	for rows.Next() {
		hold, err := scanMaintenanceHold(rows)
		if err != nil {
			return nil, wrapDB("list Maintenance Holds", err)
		}
		result = append(result, hold)
	}
	return result, wrapDB("list Maintenance Holds", rows.Err())
}

// SetMaintenanceHold uses optimistic revision zero for an absent row. An
// idempotent update returns the existing revision unchanged.
func (service *Service) SetMaintenanceHold(ctx context.Context, hostID identity.HostID, expectedRevision uint64, active bool, reason string) (farmmodel.MaintenanceHold, error) {
	if err := hostID.Validate(); err != nil {
		return farmmodel.MaintenanceHold{}, typed(farmerr.CONFIG_CONFLICT, "invalid Maintenance Hold HostID", err)
	}
	if err := farmmodel.ValidateMaintenanceReason(reason); err != nil {
		return farmmodel.MaintenanceHold{}, err
	}
	var result farmmodel.MaintenanceHold
	err := service.writeTx(ctx, func(tx *sql.Tx) error {
		current, err := scanMaintenanceHold(tx.QueryRowContext(ctx, maintenanceSelect+" WHERE host_id=?", hostID.String()))
		if errors.Is(err, sql.ErrNoRows) {
			if expectedRevision != 0 {
				return revisionConflict("MaintenanceHold", expectedRevision, 0)
			}
			now := service.clock().UTC()
			result = farmmodel.MaintenanceHold{HostID: hostID, Active: active, Revision: 1, Reason: reason, CreatedAt: now, UpdatedAt: now}
			_, err = tx.ExecContext(ctx, `INSERT INTO maintenance_holds(host_id,active,revision,reason,created_at_ns,updated_at_ns) VALUES(?,?,?,?,?,?)`, hostID.String(), active, result.Revision, reason, now.UnixNano(), now.UnixNano())
			return err
		}
		if err != nil {
			return err
		}
		if current.Revision != expectedRevision {
			return revisionConflict("MaintenanceHold", expectedRevision, current.Revision)
		}
		if current.Active == active && current.Reason == reason {
			result = current
			return nil
		}
		current.Active = active
		current.Reason = reason
		current.Revision++
		current.UpdatedAt = service.clock().UTC()
		if _, err := tx.ExecContext(ctx, `UPDATE maintenance_holds SET active=?,revision=?,reason=?,updated_at_ns=? WHERE host_id=?`, active, current.Revision, reason, current.UpdatedAt.UnixNano(), hostID.String()); err != nil {
			return err
		}
		result = current
		return nil
	})
	if err != nil {
		return farmmodel.MaintenanceHold{}, wrapDB("set Maintenance Hold", err)
	}
	return result, nil
}

func scanMaintenanceHold(row scanner) (farmmodel.MaintenanceHold, error) {
	var hold farmmodel.MaintenanceHold
	var hostID string
	var active bool
	var created, updated int64
	if err := row.Scan(&hostID, &active, &hold.Revision, &hold.Reason, &created, &updated); err != nil {
		return farmmodel.MaintenanceHold{}, err
	}
	var err error
	if hold.HostID, err = identity.ParseHostID(hostID); err != nil {
		return farmmodel.MaintenanceHold{}, corrupt("stored Maintenance Hold HostID is invalid", err)
	}
	if hold.Revision < 1 {
		return farmmodel.MaintenanceHold{}, corrupt("stored Maintenance Hold revision is invalid", nil)
	}
	if err := farmmodel.ValidateMaintenanceReason(hold.Reason); err != nil {
		return farmmodel.MaintenanceHold{}, corrupt("stored Maintenance Hold reason is invalid", err)
	}
	hold.Active = active
	hold.CreatedAt = time.Unix(0, created).UTC()
	hold.UpdatedAt = time.Unix(0, updated).UTC()
	if hold.CreatedAt.IsZero() || hold.UpdatedAt.Before(hold.CreatedAt) {
		return farmmodel.MaintenanceHold{}, corrupt("stored Maintenance Hold timestamps are invalid", nil)
	}
	return hold, nil
}
