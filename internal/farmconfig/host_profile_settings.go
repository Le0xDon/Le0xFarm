package farmconfig

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"slices"
	"time"

	"github.com/le0xdon/le0xfarm/internal/farmerr"
	"github.com/le0xdon/le0xfarm/internal/farmmodel"
	"github.com/le0xdon/le0xfarm/internal/identity"
)

const hostProfileSettingsSelect = `SELECT host_id,profile_id,object_schema_version,revision,content_hash,origin,created_at_ns,updated_at_ns,cpu_threads,huge_pages,msr FROM host_profile_settings`

func (service *Service) CreateHostProfileSettings(ctx context.Context, hostID identity.HostID, profileID identity.ProfileID, content farmmodel.HostProfileSettingsContent) (farmmodel.HostProfileSettings, error) {
	if err := farmmodel.ValidateHostProfileSettings(hostID, profileID, content); err != nil {
		return farmmodel.HostProfileSettings{}, err
	}
	hash, err := farmmodel.HostProfileSettingsHash(content)
	if err != nil {
		return farmmodel.HostProfileSettings{}, typed(farmerr.INTERNAL_ERROR, "cannot hash HostProfileSettings", err)
	}
	now := service.clock().UTC()
	object := farmmodel.HostProfileSettings{HostID: hostID, ProfileID: profileID, Meta: newMeta(farmmodel.OriginController, hash, now), HostProfileSettingsContent: content}
	err = service.writeTx(ctx, func(tx *sql.Tx) error {
		profile, err := scanProfile(tx.QueryRowContext(ctx, profileSelect+" WHERE profile_id=?", profileID.String()))
		if err != nil {
			if code, _ := farmerr.CodeOf(err); code == farmerr.NOT_FOUND {
				return typed(farmerr.INVALID_REFERENCE, "HostProfileSettings references a missing MiningProfile", nil)
			}
			return err
		}
		if err := service.validateSettingsForProfile(ctx, profile, content); err != nil {
			return err
		}
		var exists int
		if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM host_profile_settings WHERE host_id=? AND profile_id=?)", hostID.String(), profileID.String()).Scan(&exists); err != nil {
			return err
		}
		if exists != 0 {
			return typed(farmerr.ALREADY_EXISTS, "HostProfileSettings already exists for this HostID and ProfileID", nil)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO host_profile_settings(host_id,profile_id,object_schema_version,revision,content_hash,origin,created_at_ns,updated_at_ns,cpu_threads,huge_pages,msr) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, hostProfileSettingsArgs(object)...); err != nil {
			return err
		}
		return service.propagateHostProfileWorkloads(ctx, tx, hostID, profileID)
	})
	if err != nil {
		return farmmodel.HostProfileSettings{}, wrapDB("create HostProfileSettings", err)
	}
	return object, nil
}

func (service *Service) GetHostProfileSettings(ctx context.Context, hostID identity.HostID, profileID identity.ProfileID) (farmmodel.HostProfileSettings, error) {
	if err := hostID.Validate(); err != nil {
		return farmmodel.HostProfileSettings{}, typed(farmerr.CONFIG_CONFLICT, "invalid HostID", err)
	}
	if err := profileID.Validate(); err != nil {
		return farmmodel.HostProfileSettings{}, typed(farmerr.CONFIG_CONFLICT, "invalid ProfileID", err)
	}
	return scanHostProfileSettings(service.db.QueryRowContext(ctx, hostProfileSettingsSelect+" WHERE host_id=? AND profile_id=?", hostID.String(), profileID.String()))
}

func (service *Service) ListHostProfileSettings(ctx context.Context) ([]farmmodel.HostProfileSettings, error) {
	rows, err := service.db.QueryContext(ctx, hostProfileSettingsSelect+" ORDER BY host_id,profile_id")
	if err != nil {
		return nil, wrapDB("list HostProfileSettings", err)
	}
	defer rows.Close()
	var result []farmmodel.HostProfileSettings
	for rows.Next() {
		object, err := scanHostProfileSettings(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, object)
	}
	return result, wrapDB("list HostProfileSettings", rows.Err())
}

func (service *Service) UpdateHostProfileSettings(ctx context.Context, hostID identity.HostID, profileID identity.ProfileID, expectedRevision uint64, content farmmodel.HostProfileSettingsContent) (farmmodel.HostProfileSettings, error) {
	if err := farmmodel.ValidateHostProfileSettings(hostID, profileID, content); err != nil {
		return farmmodel.HostProfileSettings{}, err
	}
	hash, err := farmmodel.HostProfileSettingsHash(content)
	if err != nil {
		return farmmodel.HostProfileSettings{}, typed(farmerr.INTERNAL_ERROR, "cannot hash HostProfileSettings", err)
	}
	var result farmmodel.HostProfileSettings
	err = service.writeTx(ctx, func(tx *sql.Tx) error {
		current, err := scanHostProfileSettings(tx.QueryRowContext(ctx, hostProfileSettingsSelect+" WHERE host_id=? AND profile_id=?", hostID.String(), profileID.String()))
		if err != nil {
			return err
		}
		if current.Meta.Revision != expectedRevision {
			return revisionConflict("HostProfileSettings", expectedRevision, current.Meta.Revision)
		}
		profile, err := scanProfile(tx.QueryRowContext(ctx, profileSelect+" WHERE profile_id=?", profileID.String()))
		if err != nil {
			return err
		}
		if err := service.validateSettingsForProfile(ctx, profile, content); err != nil {
			return err
		}
		if current.Meta.ContentHash == hash {
			result = current
			return nil
		}
		current.Meta.Revision++
		current.Meta.ContentHash = hash
		current.Meta.UpdatedAt = service.clock().UTC()
		current.HostProfileSettingsContent = content
		if _, err := tx.ExecContext(ctx, `UPDATE host_profile_settings SET revision=?,content_hash=?,updated_at_ns=?,cpu_threads=?,huge_pages=?,msr=? WHERE host_id=? AND profile_id=?`, current.Meta.Revision, current.Meta.ContentHash, current.Meta.UpdatedAt.UnixNano(), nullableUint32(current.CPUThreads), nullableBool(current.HugePages), nullableBool(current.MSR), hostID.String(), profileID.String()); err != nil {
			return err
		}
		if err := service.propagateHostProfileWorkloads(ctx, tx, hostID, profileID); err != nil {
			return err
		}
		result = current
		return nil
	})
	if err != nil {
		return farmmodel.HostProfileSettings{}, wrapDB("update HostProfileSettings", err)
	}
	return result, nil
}

func (service *Service) validateSettingsForProfile(ctx context.Context, profile farmmodel.MiningProfile, content farmmodel.HostProfileSettingsContent) error {
	release, err := service.catalog.Lookup(ctx, profile.Package)
	if err != nil {
		return typed(farmerr.INVALID_REFERENCE, "HostProfileSettings package release is unavailable", err)
	}
	if !slices.Contains(release.AdapterIDs, profile.AdapterID) {
		return typed(farmerr.INVALID_REFERENCE, "HostProfileSettings adapter is not supported by the package release", nil)
	}
	return farmmodel.ValidateTuningCapabilities(content.CPUThreads, content.HugePages, content.MSR, release.Tuning)
}

// DeleteHostProfileSettings removes all explicit overrides for a Host+Profile
// pair, restoring inheritance from MiningProfile defaults.
func (service *Service) DeleteHostProfileSettings(ctx context.Context, hostID identity.HostID, profileID identity.ProfileID, expectedRevision uint64) error {
	if err := hostID.Validate(); err != nil {
		return typed(farmerr.CONFIG_CONFLICT, "invalid HostID", err)
	}
	if err := profileID.Validate(); err != nil {
		return typed(farmerr.CONFIG_CONFLICT, "invalid ProfileID", err)
	}
	err := service.writeTx(ctx, func(tx *sql.Tx) error {
		current, err := scanHostProfileSettings(tx.QueryRowContext(ctx, hostProfileSettingsSelect+" WHERE host_id=? AND profile_id=?", hostID.String(), profileID.String()))
		if err != nil {
			return err
		}
		if current.Meta.Revision != expectedRevision {
			return revisionConflict("HostProfileSettings", expectedRevision, current.Meta.Revision)
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM host_profile_settings WHERE host_id=? AND profile_id=?", hostID.String(), profileID.String()); err != nil {
			return err
		}
		return service.propagateHostProfileWorkloads(ctx, tx, hostID, profileID)
	})
	return wrapDB("delete HostProfileSettings", err)
}

func scanHostProfileSettings(row scanner) (farmmodel.HostProfileSettings, error) {
	var object farmmodel.HostProfileSettings
	var hostID, profileID string
	var created, updated int64
	var cpuThreads, hugePages, msr sql.NullInt64
	if err := row.Scan(&hostID, &profileID, &object.Meta.SchemaVersion, &object.Meta.Revision, &object.Meta.ContentHash, &object.Meta.Origin, &created, &updated, &cpuThreads, &hugePages, &msr); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return farmmodel.HostProfileSettings{}, typed(farmerr.NOT_FOUND, "HostProfileSettings was not found", nil)
		}
		return farmmodel.HostProfileSettings{}, typed(farmerr.INTERNAL_ERROR, "cannot read HostProfileSettings", err)
	}
	var err error
	if object.HostID, err = identity.ParseHostID(hostID); err != nil {
		return farmmodel.HostProfileSettings{}, corrupt("stored HostProfileSettings HostID is invalid", err)
	}
	if object.ProfileID, err = identity.ParseProfileID(profileID); err != nil {
		return farmmodel.HostProfileSettings{}, corrupt("stored HostProfileSettings ProfileID is invalid", err)
	}
	object.Meta.CreatedAt = time.Unix(0, created).UTC()
	object.Meta.UpdatedAt = time.Unix(0, updated).UTC()
	if cpuThreads.Valid {
		if cpuThreads.Int64 < 1 || cpuThreads.Int64 > math.MaxUint32 {
			return farmmodel.HostProfileSettings{}, corrupt("stored HostProfileSettings CPU thread setting is invalid", nil)
		}
		value := uint32(cpuThreads.Int64)
		object.CPUThreads = &value
	}
	if hugePages.Valid {
		if hugePages.Int64 != 0 && hugePages.Int64 != 1 {
			return farmmodel.HostProfileSettings{}, corrupt("stored HostProfileSettings huge pages setting is invalid", nil)
		}
		value := hugePages.Int64 != 0
		object.HugePages = &value
	}
	if msr.Valid {
		if msr.Int64 != 0 && msr.Int64 != 1 {
			return farmmodel.HostProfileSettings{}, corrupt("stored HostProfileSettings MSR setting is invalid", nil)
		}
		value := msr.Int64 != 0
		object.MSR = &value
	}
	if err := farmmodel.ValidateHostProfileSettings(object.HostID, object.ProfileID, object.HostProfileSettingsContent); err != nil {
		return farmmodel.HostProfileSettings{}, corrupt("stored HostProfileSettings content is invalid", err)
	}
	hash, err := farmmodel.HostProfileSettingsHash(object.HostProfileSettingsContent)
	if err != nil || hash != object.Meta.ContentHash {
		return farmmodel.HostProfileSettings{}, corrupt("stored HostProfileSettings content hash does not match", err)
	}
	if err := validateStoredMeta(object.Meta); err != nil {
		return farmmodel.HostProfileSettings{}, err
	}
	return object, nil
}

func hostProfileSettingsArgs(object farmmodel.HostProfileSettings) []any {
	return []any{object.HostID.String(), object.ProfileID.String(), object.Meta.SchemaVersion, object.Meta.Revision, object.Meta.ContentHash, object.Meta.Origin, object.Meta.CreatedAt.UnixNano(), object.Meta.UpdatedAt.UnixNano(), nullableUint32(object.CPUThreads), nullableBool(object.HugePages), nullableBool(object.MSR)}
}
