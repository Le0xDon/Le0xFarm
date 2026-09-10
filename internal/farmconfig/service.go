// Package farmconfig implements persistent Controller farm-object CRUD.
package farmconfig

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/le0xdon/le0xfarm/internal/controllerdb"
	"github.com/le0xdon/le0xfarm/internal/farmerr"
	"github.com/le0xdon/le0xfarm/internal/farmmodel"
	"github.com/le0xdon/le0xfarm/internal/farmresolve"
	"github.com/le0xdon/le0xfarm/internal/identity"
)

type Options struct {
	Clock          func() time.Time
	NewPoolID      func() (identity.PoolID, error)
	NewWalletID    func() (identity.WalletID, error)
	NewProfileID   func() (identity.ProfileID, error)
	NewWorkloadID  func() (identity.WorkloadID, error)
	NewExecutionID func() (identity.ExecutionID, error)
}

type Service struct {
	db             *sql.DB
	catalog        farmmodel.PackageCatalog
	clock          func() time.Time
	newPoolID      func() (identity.PoolID, error)
	newWalletID    func() (identity.WalletID, error)
	newProfileID   func() (identity.ProfileID, error)
	newWorkloadID  func() (identity.WorkloadID, error)
	newExecutionID func() (identity.ExecutionID, error)
	resolver       farmresolve.Resolver
}

func New(db *controllerdb.DB, catalog farmmodel.PackageCatalog, options Options) (*Service, error) {
	if db == nil || catalog == nil {
		return nil, typed(farmerr.CONFIG_CONFLICT, "farm database and package catalog are required", nil)
	}
	if options.Clock == nil {
		options.Clock = time.Now
	}
	if options.NewPoolID == nil {
		options.NewPoolID = identity.NewPoolID
	}
	if options.NewWalletID == nil {
		options.NewWalletID = identity.NewWalletID
	}
	if options.NewProfileID == nil {
		options.NewProfileID = identity.NewProfileID
	}
	if options.NewWorkloadID == nil {
		options.NewWorkloadID = identity.NewWorkloadID
	}
	if options.NewExecutionID == nil {
		options.NewExecutionID = identity.NewExecutionID
	}
	return &Service{db: db.SQL(), catalog: catalog, clock: options.Clock, newPoolID: options.NewPoolID, newWalletID: options.NewWalletID, newProfileID: options.NewProfileID, newWorkloadID: options.NewWorkloadID, newExecutionID: options.NewExecutionID, resolver: farmresolve.Resolver{Catalog: catalog}}, nil
}

func (service *Service) CreatePool(ctx context.Context, content farmmodel.PoolContent) (farmmodel.Pool, error) {
	if err := farmmodel.ValidatePool(content); err != nil {
		return farmmodel.Pool{}, err
	}
	id, err := service.newPoolID()
	if err != nil {
		return farmmodel.Pool{}, typed(farmerr.INTERNAL_ERROR, "cannot generate PoolID", err)
	}
	hash, err := farmmodel.PoolHash(content)
	if err != nil {
		return farmmodel.Pool{}, typed(farmerr.INTERNAL_ERROR, "cannot hash Pool", err)
	}
	now := service.clock().UTC()
	object := farmmodel.Pool{PoolID: id, Meta: newMeta(farmmodel.OriginController, hash, now), PoolContent: content}
	err = service.writeTx(ctx, func(tx *sql.Tx) error {
		var exists int
		if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM pools WHERE pool_id=?)", id.String()).Scan(&exists); err != nil {
			return err
		}
		if exists != 0 {
			return typed(farmerr.ALREADY_EXISTS, "PoolID already exists", nil)
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO pools(pool_id,object_schema_version,revision,content_hash,origin,created_at_ns,updated_at_ns,name,address,tls,auth_kind,public_literal) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, poolArgs(object)...)
		return err
	})
	if err != nil {
		return farmmodel.Pool{}, wrapDB("create Pool", err)
	}
	return object, nil
}

func (service *Service) GetPool(ctx context.Context, id identity.PoolID) (farmmodel.Pool, error) {
	if err := id.Validate(); err != nil {
		return farmmodel.Pool{}, typed(farmerr.CONFIG_CONFLICT, "invalid PoolID", err)
	}
	return scanPool(service.db.QueryRowContext(ctx, poolSelect+" WHERE pool_id=?", id.String()))
}

func (service *Service) ListPools(ctx context.Context) ([]farmmodel.Pool, error) {
	rows, err := service.db.QueryContext(ctx, poolSelect+" ORDER BY pool_id")
	if err != nil {
		return nil, wrapDB("list Pools", err)
	}
	defer rows.Close()
	var result []farmmodel.Pool
	for rows.Next() {
		object, err := scanPool(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, object)
	}
	return result, wrapDB("list Pools", rows.Err())
}

func (service *Service) UpdatePool(ctx context.Context, id identity.PoolID, expectedRevision uint64, content farmmodel.PoolContent) (farmmodel.Pool, error) {
	if err := id.Validate(); err != nil {
		return farmmodel.Pool{}, typed(farmerr.CONFIG_CONFLICT, "invalid PoolID", err)
	}
	if err := farmmodel.ValidatePool(content); err != nil {
		return farmmodel.Pool{}, err
	}
	hash, err := farmmodel.PoolHash(content)
	if err != nil {
		return farmmodel.Pool{}, typed(farmerr.INTERNAL_ERROR, "cannot hash Pool", err)
	}
	var result farmmodel.Pool
	err = service.writeTx(ctx, func(tx *sql.Tx) error {
		current, err := scanPool(tx.QueryRowContext(ctx, poolSelect+" WHERE pool_id=?", id.String()))
		if err != nil {
			return err
		}
		if current.Meta.Revision != expectedRevision {
			return revisionConflict("Pool", expectedRevision, current.Meta.Revision)
		}
		if current.Meta.ContentHash == hash {
			result = current
			return nil
		}
		current.Meta.Revision++
		current.Meta.ContentHash = hash
		current.Meta.UpdatedAt = service.clock().UTC()
		current.PoolContent = content
		_, err = tx.ExecContext(ctx, `UPDATE pools SET revision=?,content_hash=?,updated_at_ns=?,name=?,address=?,tls=?,auth_kind=?,public_literal=? WHERE pool_id=?`, current.Meta.Revision, current.Meta.ContentHash, current.Meta.UpdatedAt.UnixNano(), current.Name, current.Address, current.TLS, current.Auth.Kind, current.Auth.PublicLiteral, id.String())
		if err == nil {
			err = service.propagateProfileWorkloads(ctx, tx, "SELECT profile_id FROM mining_profiles WHERE pool_id=?", id.String())
		}
		result = current
		return err
	})
	if err != nil {
		return farmmodel.Pool{}, wrapDB("update Pool", err)
	}
	return result, nil
}

func (service *Service) DeletePool(ctx context.Context, id identity.PoolID, expectedRevision uint64) error {
	if err := id.Validate(); err != nil {
		return typed(farmerr.CONFIG_CONFLICT, "invalid PoolID", err)
	}
	err := service.writeTx(ctx, func(tx *sql.Tx) error {
		current, err := scanPool(tx.QueryRowContext(ctx, poolSelect+" WHERE pool_id=?", id.String()))
		if err != nil {
			return err
		}
		if current.Meta.Revision != expectedRevision {
			return revisionConflict("Pool", expectedRevision, current.Meta.Revision)
		}
		var used int
		if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM mining_profiles WHERE pool_id=?)", id.String()).Scan(&used); err != nil {
			return err
		}
		if used != 0 {
			return typed(farmerr.REFERENCE_IN_USE, "Pool is referenced by a MiningProfile", nil)
		}
		_, err = tx.ExecContext(ctx, "DELETE FROM pools WHERE pool_id=?", id.String())
		return err
	})
	return wrapDB("delete Pool", err)
}

func (service *Service) CreateWalletRef(ctx context.Context, content farmmodel.WalletRefContent) (farmmodel.WalletRef, error) {
	if err := farmmodel.ValidateWalletRef(content); err != nil {
		return farmmodel.WalletRef{}, err
	}
	id, err := service.newWalletID()
	if err != nil {
		return farmmodel.WalletRef{}, typed(farmerr.INTERNAL_ERROR, "cannot generate WalletID", err)
	}
	hash, err := farmmodel.WalletRefHash(content)
	if err != nil {
		return farmmodel.WalletRef{}, typed(farmerr.INTERNAL_ERROR, "cannot hash WalletRef", err)
	}
	now := service.clock().UTC()
	object := farmmodel.WalletRef{WalletID: id, Meta: newMeta(farmmodel.OriginController, hash, now), WalletRefContent: content}
	err = service.writeTx(ctx, func(tx *sql.Tx) error {
		var exists int
		if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM wallet_refs WHERE wallet_id=?)", id.String()).Scan(&exists); err != nil {
			return err
		}
		if exists != 0 {
			return typed(farmerr.ALREADY_EXISTS, "WalletID already exists", nil)
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO wallet_refs(wallet_id,object_schema_version,revision,content_hash,origin,created_at_ns,updated_at_ns,name,coin,address) VALUES(?,?,?,?,?,?,?,?,?,?)`, walletArgs(object)...)
		return err
	})
	if err != nil {
		return farmmodel.WalletRef{}, wrapDB("create WalletRef", err)
	}
	return object, nil
}

func (service *Service) GetWalletRef(ctx context.Context, id identity.WalletID) (farmmodel.WalletRef, error) {
	if err := id.Validate(); err != nil {
		return farmmodel.WalletRef{}, typed(farmerr.CONFIG_CONFLICT, "invalid WalletID", err)
	}
	return scanWallet(service.db.QueryRowContext(ctx, walletSelect+" WHERE wallet_id=?", id.String()))
}

func (service *Service) ListWalletRefs(ctx context.Context) ([]farmmodel.WalletRef, error) {
	rows, err := service.db.QueryContext(ctx, walletSelect+" ORDER BY wallet_id")
	if err != nil {
		return nil, wrapDB("list WalletRefs", err)
	}
	defer rows.Close()
	var result []farmmodel.WalletRef
	for rows.Next() {
		object, err := scanWallet(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, object)
	}
	return result, wrapDB("list WalletRefs", rows.Err())
}

func (service *Service) UpdateWalletRef(ctx context.Context, id identity.WalletID, expectedRevision uint64, content farmmodel.WalletRefContent) (farmmodel.WalletRef, error) {
	if err := id.Validate(); err != nil {
		return farmmodel.WalletRef{}, typed(farmerr.CONFIG_CONFLICT, "invalid WalletID", err)
	}
	if err := farmmodel.ValidateWalletRef(content); err != nil {
		return farmmodel.WalletRef{}, err
	}
	hash, err := farmmodel.WalletRefHash(content)
	if err != nil {
		return farmmodel.WalletRef{}, typed(farmerr.INTERNAL_ERROR, "cannot hash WalletRef", err)
	}
	var result farmmodel.WalletRef
	err = service.writeTx(ctx, func(tx *sql.Tx) error {
		current, err := scanWallet(tx.QueryRowContext(ctx, walletSelect+" WHERE wallet_id=?", id.String()))
		if err != nil {
			return err
		}
		if current.Meta.Revision != expectedRevision {
			return revisionConflict("WalletRef", expectedRevision, current.Meta.Revision)
		}
		if current.Meta.ContentHash == hash {
			result = current
			return nil
		}
		current.Meta.Revision++
		current.Meta.ContentHash = hash
		current.Meta.UpdatedAt = service.clock().UTC()
		current.WalletRefContent = content
		_, err = tx.ExecContext(ctx, `UPDATE wallet_refs SET revision=?,content_hash=?,updated_at_ns=?,name=?,coin=?,address=? WHERE wallet_id=?`, current.Meta.Revision, current.Meta.ContentHash, current.Meta.UpdatedAt.UnixNano(), current.Name, current.Coin, current.Address, id.String())
		if err == nil {
			err = service.propagateProfileWorkloads(ctx, tx, "SELECT profile_id FROM mining_profiles WHERE wallet_id=?", id.String())
		}
		result = current
		return err
	})
	if err != nil {
		return farmmodel.WalletRef{}, wrapDB("update WalletRef", err)
	}
	return result, nil
}

func (service *Service) DeleteWalletRef(ctx context.Context, id identity.WalletID, expectedRevision uint64) error {
	if err := id.Validate(); err != nil {
		return typed(farmerr.CONFIG_CONFLICT, "invalid WalletID", err)
	}
	err := service.writeTx(ctx, func(tx *sql.Tx) error {
		current, err := scanWallet(tx.QueryRowContext(ctx, walletSelect+" WHERE wallet_id=?", id.String()))
		if err != nil {
			return err
		}
		if current.Meta.Revision != expectedRevision {
			return revisionConflict("WalletRef", expectedRevision, current.Meta.Revision)
		}
		var used int
		if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM mining_profiles WHERE wallet_id=?)", id.String()).Scan(&used); err != nil {
			return err
		}
		if used != 0 {
			return typed(farmerr.REFERENCE_IN_USE, "WalletRef is referenced by a MiningProfile", nil)
		}
		_, err = tx.ExecContext(ctx, "DELETE FROM wallet_refs WHERE wallet_id=?", id.String())
		return err
	})
	return wrapDB("delete WalletRef", err)
}

func (service *Service) CreateMiningProfile(ctx context.Context, content farmmodel.MiningProfileContent) (farmmodel.MiningProfile, error) {
	if err := service.validateProfile(ctx, content); err != nil {
		return farmmodel.MiningProfile{}, err
	}
	id, err := service.newProfileID()
	if err != nil {
		return farmmodel.MiningProfile{}, typed(farmerr.INTERNAL_ERROR, "cannot generate ProfileID", err)
	}
	hash, err := farmmodel.MiningProfileHash(content)
	if err != nil {
		return farmmodel.MiningProfile{}, typed(farmerr.INTERNAL_ERROR, "cannot hash MiningProfile", err)
	}
	now := service.clock().UTC()
	object := farmmodel.MiningProfile{ProfileID: id, Meta: newMeta(farmmodel.OriginController, hash, now), MiningProfileContent: content}
	err = service.writeTx(ctx, func(tx *sql.Tx) error {
		if err := requireReferences(ctx, tx, content.PoolID, content.WalletID); err != nil {
			return err
		}
		var exists int
		if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM mining_profiles WHERE profile_id=?)", id.String()).Scan(&exists); err != nil {
			return err
		}
		if exists != 0 {
			return typed(farmerr.ALREADY_EXISTS, "ProfileID already exists", nil)
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO mining_profiles(profile_id,object_schema_version,revision,content_hash,origin,created_at_ns,updated_at_ns,name,adapter_id,package_id,package_version,mode,coin,algorithm,pool_id,wallet_id,user_template,worker_placement,cpu_threads,huge_pages,msr) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, profileArgs(object)...)
		return err
	})
	if err != nil {
		return farmmodel.MiningProfile{}, wrapDB("create MiningProfile", err)
	}
	return object, nil
}

func (service *Service) GetMiningProfile(ctx context.Context, id identity.ProfileID) (farmmodel.MiningProfile, error) {
	if err := id.Validate(); err != nil {
		return farmmodel.MiningProfile{}, typed(farmerr.CONFIG_CONFLICT, "invalid ProfileID", err)
	}
	return scanProfile(service.db.QueryRowContext(ctx, profileSelect+" WHERE profile_id=?", id.String()))
}

func (service *Service) ListMiningProfiles(ctx context.Context) ([]farmmodel.MiningProfile, error) {
	rows, err := service.db.QueryContext(ctx, profileSelect+" ORDER BY profile_id")
	if err != nil {
		return nil, wrapDB("list MiningProfiles", err)
	}
	defer rows.Close()
	var result []farmmodel.MiningProfile
	for rows.Next() {
		object, err := scanProfile(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, object)
	}
	return result, wrapDB("list MiningProfiles", rows.Err())
}

func (service *Service) UpdateMiningProfile(ctx context.Context, id identity.ProfileID, expectedRevision uint64, content farmmodel.MiningProfileContent) (farmmodel.MiningProfile, error) {
	if err := id.Validate(); err != nil {
		return farmmodel.MiningProfile{}, typed(farmerr.CONFIG_CONFLICT, "invalid ProfileID", err)
	}
	if err := service.validateProfile(ctx, content); err != nil {
		return farmmodel.MiningProfile{}, err
	}
	hash, err := farmmodel.MiningProfileHash(content)
	if err != nil {
		return farmmodel.MiningProfile{}, typed(farmerr.INTERNAL_ERROR, "cannot hash MiningProfile", err)
	}
	var result farmmodel.MiningProfile
	err = service.writeTx(ctx, func(tx *sql.Tx) error {
		current, err := scanProfile(tx.QueryRowContext(ctx, profileSelect+" WHERE profile_id=?", id.String()))
		if err != nil {
			return err
		}
		if current.Meta.Revision != expectedRevision {
			return revisionConflict("MiningProfile", expectedRevision, current.Meta.Revision)
		}
		if current.Meta.ContentHash == hash {
			result = current
			return nil
		}
		if err := requireReferences(ctx, tx, content.PoolID, content.WalletID); err != nil {
			return err
		}
		current.Meta.Revision++
		current.Meta.ContentHash = hash
		current.Meta.UpdatedAt = service.clock().UTC()
		current.MiningProfileContent = content
		_, err = tx.ExecContext(ctx, `UPDATE mining_profiles SET revision=?,content_hash=?,updated_at_ns=?,name=?,adapter_id=?,package_id=?,package_version=?,mode=?,coin=?,algorithm=?,pool_id=?,wallet_id=?,user_template=?,worker_placement=?,cpu_threads=?,huge_pages=?,msr=? WHERE profile_id=?`, current.Meta.Revision, current.Meta.ContentHash, current.Meta.UpdatedAt.UnixNano(), current.Name, current.AdapterID, current.Package.PackageID.String(), current.Package.Version, current.Mode, current.Coin, current.Algorithm, current.PoolID.String(), current.WalletID.String(), current.LoginPolicy.UserTemplate, current.LoginPolicy.WorkerPlacement, nullableUint32(current.CPUThreads), nullableBool(current.HugePages), nullableBool(current.MSR), id.String())
		if err == nil {
			err = service.propagateProfileWorkloads(ctx, tx, "SELECT profile_id FROM mining_profiles WHERE profile_id=?", id.String())
		}
		result = current
		return err
	})
	if err != nil {
		return farmmodel.MiningProfile{}, wrapDB("update MiningProfile", err)
	}
	return result, nil
}

func (service *Service) DeleteMiningProfile(ctx context.Context, id identity.ProfileID, expectedRevision uint64) error {
	if err := id.Validate(); err != nil {
		return typed(farmerr.CONFIG_CONFLICT, "invalid ProfileID", err)
	}
	err := service.writeTx(ctx, func(tx *sql.Tx) error {
		current, err := scanProfile(tx.QueryRowContext(ctx, profileSelect+" WHERE profile_id=?", id.String()))
		if err != nil {
			return err
		}
		if current.Meta.Revision != expectedRevision {
			return revisionConflict("MiningProfile", expectedRevision, current.Meta.Revision)
		}
		var used int
		if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM desired_workloads WHERE profile_id=?)", id.String()).Scan(&used); err != nil {
			return err
		}
		if used != 0 {
			return typed(farmerr.REFERENCE_IN_USE, "MiningProfile is referenced by a DesiredWorkload", nil)
		}
		_, err = tx.ExecContext(ctx, "DELETE FROM mining_profiles WHERE profile_id=?", id.String())
		return err
	})
	return wrapDB("delete MiningProfile", err)
}

func (service *Service) validateProfile(ctx context.Context, content farmmodel.MiningProfileContent) error {
	if err := farmmodel.ValidateMiningProfile(content); err != nil {
		return err
	}
	release, err := service.catalog.Lookup(ctx, content.Package)
	if err != nil {
		return typed(farmerr.INVALID_REFERENCE, "MiningProfile package release is unavailable", err)
	}
	if !slices.Contains(release.AdapterIDs, content.AdapterID) {
		return typed(farmerr.INVALID_REFERENCE, "MiningProfile adapter is not supported by the package release", nil)
	}
	return nil
}

func requireReferences(ctx context.Context, tx *sql.Tx, poolID identity.PoolID, walletID identity.WalletID) error {
	var pool, wallet int
	if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM pools WHERE pool_id=?)", poolID.String()).Scan(&pool); err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM wallet_refs WHERE wallet_id=?)", walletID.String()).Scan(&wallet); err != nil {
		return err
	}
	if pool == 0 {
		return typed(farmerr.INVALID_REFERENCE, "MiningProfile references a missing Pool", nil)
	}
	if wallet == 0 {
		return typed(farmerr.INVALID_REFERENCE, "MiningProfile references a missing WalletRef", nil)
	}
	return nil
}

func (service *Service) writeTx(ctx context.Context, operation func(*sql.Tx) error) error {
	tx, err := service.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := operation(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

type scanner interface{ Scan(...any) error }

const poolSelect = `SELECT pool_id,object_schema_version,revision,content_hash,origin,created_at_ns,updated_at_ns,name,address,tls,auth_kind,public_literal FROM pools`
const walletSelect = `SELECT wallet_id,object_schema_version,revision,content_hash,origin,created_at_ns,updated_at_ns,name,coin,address FROM wallet_refs`
const profileSelect = `SELECT profile_id,object_schema_version,revision,content_hash,origin,created_at_ns,updated_at_ns,name,adapter_id,package_id,package_version,mode,coin,algorithm,pool_id,wallet_id,user_template,worker_placement,cpu_threads,huge_pages,msr FROM mining_profiles`

func scanPool(row scanner) (farmmodel.Pool, error) {
	var object farmmodel.Pool
	var id string
	var created, updated int64
	var tls int
	err := row.Scan(&id, &object.Meta.SchemaVersion, &object.Meta.Revision, &object.Meta.ContentHash, &object.Meta.Origin, &created, &updated, &object.Name, &object.Address, &tls, &object.Auth.Kind, &object.Auth.PublicLiteral)
	if err != nil {
		return farmmodel.Pool{}, scanError("Pool", err)
	}
	parsed, err := identity.ParsePoolID(id)
	if err != nil {
		return farmmodel.Pool{}, typed(farmerr.INTERNAL_ERROR, "stored PoolID is invalid", err)
	}
	object.PoolID = parsed
	object.Meta.CreatedAt = time.Unix(0, created).UTC()
	object.Meta.UpdatedAt = time.Unix(0, updated).UTC()
	if tls != 0 && tls != 1 {
		return farmmodel.Pool{}, corrupt("stored Pool TLS value is invalid", nil)
	}
	object.TLS = tls != 0
	if err := farmmodel.ValidatePool(object.PoolContent); err != nil {
		return farmmodel.Pool{}, corrupt("stored Pool content is invalid", err)
	}
	hash, err := farmmodel.PoolHash(object.PoolContent)
	if err != nil || hash != object.Meta.ContentHash {
		return farmmodel.Pool{}, corrupt("stored Pool content hash does not match", err)
	}
	if err := validateStoredMeta(object.Meta); err != nil {
		return farmmodel.Pool{}, err
	}
	return object, nil
}

func scanWallet(row scanner) (farmmodel.WalletRef, error) {
	var object farmmodel.WalletRef
	var id string
	var created, updated int64
	err := row.Scan(&id, &object.Meta.SchemaVersion, &object.Meta.Revision, &object.Meta.ContentHash, &object.Meta.Origin, &created, &updated, &object.Name, &object.Coin, &object.Address)
	if err != nil {
		return farmmodel.WalletRef{}, scanError("WalletRef", err)
	}
	parsed, err := identity.ParseWalletID(id)
	if err != nil {
		return farmmodel.WalletRef{}, typed(farmerr.INTERNAL_ERROR, "stored WalletID is invalid", err)
	}
	object.WalletID = parsed
	object.Meta.CreatedAt = time.Unix(0, created).UTC()
	object.Meta.UpdatedAt = time.Unix(0, updated).UTC()
	if err := farmmodel.ValidateWalletRef(object.WalletRefContent); err != nil {
		return farmmodel.WalletRef{}, corrupt("stored WalletRef content is invalid", err)
	}
	hash, err := farmmodel.WalletRefHash(object.WalletRefContent)
	if err != nil || hash != object.Meta.ContentHash {
		return farmmodel.WalletRef{}, corrupt("stored WalletRef content hash does not match", err)
	}
	if err := validateStoredMeta(object.Meta); err != nil {
		return farmmodel.WalletRef{}, err
	}
	return object, nil
}

func scanProfile(row scanner) (farmmodel.MiningProfile, error) {
	var object farmmodel.MiningProfile
	var id, packageID, poolID, walletID string
	var created, updated int64
	var cpuThreads, hugePages, msr sql.NullInt64
	err := row.Scan(&id, &object.Meta.SchemaVersion, &object.Meta.Revision, &object.Meta.ContentHash, &object.Meta.Origin, &created, &updated, &object.Name, &object.AdapterID, &packageID, &object.Package.Version, &object.Mode, &object.Coin, &object.Algorithm, &poolID, &walletID, &object.LoginPolicy.UserTemplate, &object.LoginPolicy.WorkerPlacement, &cpuThreads, &hugePages, &msr)
	if err != nil {
		return farmmodel.MiningProfile{}, scanError("MiningProfile", err)
	}
	profile, err := identity.ParseProfileID(id)
	if err != nil {
		return farmmodel.MiningProfile{}, typed(farmerr.INTERNAL_ERROR, "stored ProfileID is invalid", err)
	}
	packageParsed, err := identity.ParsePackageID(packageID)
	if err != nil {
		return farmmodel.MiningProfile{}, typed(farmerr.INTERNAL_ERROR, "stored PackageID is invalid", err)
	}
	pool, err := identity.ParsePoolID(poolID)
	if err != nil {
		return farmmodel.MiningProfile{}, typed(farmerr.INTERNAL_ERROR, "stored PoolID is invalid", err)
	}
	wallet, err := identity.ParseWalletID(walletID)
	if err != nil {
		return farmmodel.MiningProfile{}, typed(farmerr.INTERNAL_ERROR, "stored WalletID is invalid", err)
	}
	object.ProfileID = profile
	object.Package.PackageID = packageParsed
	object.PoolID = pool
	object.WalletID = wallet
	object.Meta.CreatedAt = time.Unix(0, created).UTC()
	object.Meta.UpdatedAt = time.Unix(0, updated).UTC()
	if cpuThreads.Valid {
		if cpuThreads.Int64 < 1 || cpuThreads.Int64 > math.MaxUint32 {
			return farmmodel.MiningProfile{}, corrupt("stored CPU thread setting is invalid", nil)
		}
		value := uint32(cpuThreads.Int64)
		object.CPUThreads = &value
	}
	if hugePages.Valid {
		if hugePages.Int64 != 0 && hugePages.Int64 != 1 {
			return farmmodel.MiningProfile{}, corrupt("stored huge pages setting is invalid", nil)
		}
		value := hugePages.Int64 != 0
		object.HugePages = &value
	}
	if msr.Valid {
		if msr.Int64 != 0 && msr.Int64 != 1 {
			return farmmodel.MiningProfile{}, corrupt("stored MSR setting is invalid", nil)
		}
		value := msr.Int64 != 0
		object.MSR = &value
	}
	if err := farmmodel.ValidateMiningProfile(object.MiningProfileContent); err != nil {
		return farmmodel.MiningProfile{}, corrupt("stored MiningProfile content is invalid", err)
	}
	hash, err := farmmodel.MiningProfileHash(object.MiningProfileContent)
	if err != nil || hash != object.Meta.ContentHash {
		return farmmodel.MiningProfile{}, corrupt("stored MiningProfile content hash does not match", err)
	}
	if err := validateStoredMeta(object.Meta); err != nil {
		return farmmodel.MiningProfile{}, err
	}
	return object, nil
}

func poolArgs(object farmmodel.Pool) []any {
	return []any{object.PoolID.String(), object.Meta.SchemaVersion, object.Meta.Revision, object.Meta.ContentHash, object.Meta.Origin, object.Meta.CreatedAt.UnixNano(), object.Meta.UpdatedAt.UnixNano(), object.Name, object.Address, object.TLS, object.Auth.Kind, object.Auth.PublicLiteral}
}
func walletArgs(object farmmodel.WalletRef) []any {
	return []any{object.WalletID.String(), object.Meta.SchemaVersion, object.Meta.Revision, object.Meta.ContentHash, object.Meta.Origin, object.Meta.CreatedAt.UnixNano(), object.Meta.UpdatedAt.UnixNano(), object.Name, object.Coin, object.Address}
}
func profileArgs(object farmmodel.MiningProfile) []any {
	return []any{object.ProfileID.String(), object.Meta.SchemaVersion, object.Meta.Revision, object.Meta.ContentHash, object.Meta.Origin, object.Meta.CreatedAt.UnixNano(), object.Meta.UpdatedAt.UnixNano(), object.Name, object.AdapterID, object.Package.PackageID.String(), object.Package.Version, object.Mode, object.Coin, object.Algorithm, object.PoolID.String(), object.WalletID.String(), object.LoginPolicy.UserTemplate, object.LoginPolicy.WorkerPlacement, nullableUint32(object.CPUThreads), nullableBool(object.HugePages), nullableBool(object.MSR)}
}
func nullableUint32(value *uint32) any {
	if value == nil {
		return nil
	}
	return int64(*value)
}
func nullableBool(value *bool) any {
	if value == nil {
		return nil
	}
	if *value {
		return 1
	}
	return 0
}
func newMeta(origin farmmodel.Origin, hash string, now time.Time) farmmodel.ObjectMeta {
	return farmmodel.ObjectMeta{SchemaVersion: farmmodel.ObjectSchemaVersion, Revision: 1, ContentHash: hash, Origin: origin, CreatedAt: now, UpdatedAt: now}
}
func validateOrigin(origin farmmodel.Origin) error {
	if origin != farmmodel.OriginController && origin != farmmodel.OriginBuiltin {
		return typed(farmerr.CONFIG_CONFLICT, "invalid object origin", nil)
	}
	return nil
}
func validateStoredMeta(meta farmmodel.ObjectMeta) error {
	if meta.SchemaVersion != farmmodel.ObjectSchemaVersion {
		return corrupt("stored object schema version is unsupported", nil)
	}
	if meta.Revision < 1 {
		return corrupt("stored object revision is invalid", nil)
	}
	if err := validateOrigin(meta.Origin); err != nil {
		return corrupt("stored object origin is invalid", err)
	}
	if meta.CreatedAt.IsZero() || meta.UpdatedAt.IsZero() || meta.UpdatedAt.Before(meta.CreatedAt) {
		return corrupt("stored object timestamps are invalid", nil)
	}
	return nil
}
func corrupt(message string, cause error) error {
	return typed(farmerr.CONFIG_CONFLICT, message, cause)
}
func revisionConflict(kind string, expected, current uint64) error {
	return farmerr.Error{Code: farmerr.REVISION_CONFLICT, HumanMessage: kind + " revision changed", Details: map[string]string{"expected": fmt.Sprint(expected), "current": fmt.Sprint(current)}}
}
func scanError(kind string, err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return typed(farmerr.NOT_FOUND, kind+" was not found", nil)
	}
	return typed(farmerr.INTERNAL_ERROR, "cannot read "+kind, err)
}
func wrapDB(operation string, err error) error {
	if err == nil {
		return nil
	}
	if _, ok := farmerr.CodeOf(err); ok {
		return err
	}
	return typed(farmerr.INTERNAL_ERROR, "farm database operation failed: "+operation, err)
}
func typed(code farmerr.Code, message string, cause error) error {
	details := map[string]string{}
	if cause != nil {
		if typedCode, ok := farmerr.CodeOf(cause); ok {
			details["cause_code"] = string(typedCode)
		} else {
			details["cause"] = "underlying operation failed"
		}
	}
	return farmerr.Error{Code: code, HumanMessage: message, Details: details}
}
