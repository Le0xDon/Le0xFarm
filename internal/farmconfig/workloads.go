package farmconfig

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/le0xdon/le0xfarm/internal/farmerr"
	"github.com/le0xdon/le0xfarm/internal/farmmodel"
	"github.com/le0xdon/le0xfarm/internal/farmresolve"
	"github.com/le0xdon/le0xfarm/internal/identity"
	"github.com/le0xdon/le0xfarm/internal/model"
	"github.com/le0xdon/le0xfarm/internal/protocol"
)

const workloadSelect = `SELECT workload_id,object_schema_version,revision,content_hash,origin,created_at_ns,updated_at_ns,name,host_id,profile_id,desired_run_state,worker,claim_cpu,desired_generation,effective_hash FROM desired_workloads`
const snapshotSelect = `SELECT workload_id,desired_generation,execution_id,host_id,profile_id,profile_revision,pool_id,pool_revision,wallet_id,wallet_revision,package_id,package_version,claim_cpu,plan_json,resolved_hash,created_at_ns FROM resolved_execution_snapshots`

func (service *Service) CreateDesiredWorkload(ctx context.Context, content farmmodel.DesiredWorkloadContent) (farmmodel.DesiredWorkload, error) {
	content.Resources = farmmodel.NormalizeResourceClaim(content.Resources)
	if err := farmmodel.ValidateDesiredWorkload(content); err != nil {
		return farmmodel.DesiredWorkload{}, err
	}
	id, err := service.newWorkloadID()
	if err != nil {
		return farmmodel.DesiredWorkload{}, typed(farmerr.INTERNAL_ERROR, "cannot generate WorkloadID", err)
	}
	hash, err := farmmodel.DesiredWorkloadHash(content)
	if err != nil {
		return farmmodel.DesiredWorkload{}, typed(farmerr.INTERNAL_ERROR, "cannot hash DesiredWorkload", err)
	}
	now := service.clock().UTC()
	object := farmmodel.DesiredWorkload{WorkloadID: id, Meta: newMeta(farmmodel.OriginController, hash, now), DesiredGeneration: 1, DesiredWorkloadContent: content}
	err = service.writeTx(ctx, func(tx *sql.Tx) error {
		var exists int
		if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM desired_workloads WHERE workload_id=?)", id.String()).Scan(&exists); err != nil {
			return err
		}
		if exists != 0 {
			return typed(farmerr.ALREADY_EXISTS, "WorkloadID already exists", nil)
		}
		if err := service.checkResourceConflict(ctx, tx, object, identity.WorkloadID{}); err != nil {
			return err
		}
		inputs, result, err := service.resolveWorkload(ctx, tx, object)
		if err != nil {
			return err
		}
		object.EffectiveHash = result.Hash
		if _, err = tx.ExecContext(ctx, `INSERT INTO desired_workloads(workload_id,object_schema_version,revision,content_hash,origin,created_at_ns,updated_at_ns,name,host_id,profile_id,desired_run_state,worker,claim_cpu,desired_generation,effective_hash) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, workloadArgs(object)...); err != nil {
			return err
		}
		if err = insertWorkloadDevices(ctx, tx, object.WorkloadID, object.Resources.DeviceIDs); err != nil {
			return err
		}
		if object.RunState == farmmodel.DesiredRunning {
			return service.createSnapshot(ctx, tx, inputs, result, object.DesiredGeneration, now)
		}
		return nil
	})
	if err != nil {
		return farmmodel.DesiredWorkload{}, wrapDB("create DesiredWorkload", err)
	}
	return object, nil
}

func (service *Service) GetDesiredWorkload(ctx context.Context, id identity.WorkloadID) (farmmodel.DesiredWorkload, error) {
	if err := id.Validate(); err != nil {
		return farmmodel.DesiredWorkload{}, typed(farmerr.CONFIG_CONFLICT, "invalid WorkloadID", err)
	}
	object, err := scanWorkload(service.db.QueryRowContext(ctx, workloadSelect+" WHERE workload_id=?", id.String()))
	if err != nil {
		return farmmodel.DesiredWorkload{}, err
	}
	object.Resources.DeviceIDs, err = loadWorkloadDevices(ctx, service.db, id)
	if err != nil {
		return farmmodel.DesiredWorkload{}, wrapDB("load DesiredWorkload resources", err)
	}
	return validateLoadedWorkload(object)
}

func (service *Service) ListDesiredWorkloads(ctx context.Context) ([]farmmodel.DesiredWorkload, error) {
	rows, err := service.db.QueryContext(ctx, workloadSelect+" ORDER BY workload_id")
	if err != nil {
		return nil, wrapDB("list DesiredWorkloads", err)
	}
	var result []farmmodel.DesiredWorkload
	for rows.Next() {
		object, err := scanWorkload(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		result = append(result, object)
	}
	if err := rows.Close(); err != nil {
		return nil, wrapDB("list DesiredWorkloads", err)
	}
	for i := range result {
		result[i].Resources.DeviceIDs, err = loadWorkloadDevices(ctx, service.db, result[i].WorkloadID)
		if err != nil {
			return nil, wrapDB("load DesiredWorkload resources", err)
		}
		result[i], err = validateLoadedWorkload(result[i])
		if err != nil {
			return nil, err
		}
	}
	return result, nil
}

func (service *Service) UpdateDesiredWorkload(ctx context.Context, id identity.WorkloadID, expectedRevision uint64, content farmmodel.DesiredWorkloadContent) (farmmodel.DesiredWorkload, error) {
	if err := id.Validate(); err != nil {
		return farmmodel.DesiredWorkload{}, typed(farmerr.CONFIG_CONFLICT, "invalid WorkloadID", err)
	}
	content.Resources = farmmodel.NormalizeResourceClaim(content.Resources)
	if err := farmmodel.ValidateDesiredWorkload(content); err != nil {
		return farmmodel.DesiredWorkload{}, err
	}
	hash, err := farmmodel.DesiredWorkloadHash(content)
	if err != nil {
		return farmmodel.DesiredWorkload{}, typed(farmerr.INTERNAL_ERROR, "cannot hash DesiredWorkload", err)
	}
	var resultObject farmmodel.DesiredWorkload
	err = service.writeTx(ctx, func(tx *sql.Tx) error {
		current, err := service.loadWorkloadTx(ctx, tx, id)
		if err != nil {
			return err
		}
		if current.Meta.Revision != expectedRevision {
			return revisionConflict("DesiredWorkload", expectedRevision, current.Meta.Revision)
		}
		// M4 has no cross-Host workload migration protocol. Changing the target
		// in place could abandon an execution owned by the immutable snapshots on
		// the old Host while starting a replacement on the new Host. Require an
		// explicit stopped/retired workload lifecycle instead.
		if content.HostID != current.HostID {
			return typed(farmerr.CONFIG_CONFLICT, "DesiredWorkload HostID is immutable; retire it before targeting another Host", nil)
		}
		if current.Meta.ContentHash == hash {
			resultObject = current
			return nil
		}
		updated := current
		updated.DesiredWorkloadContent = content
		updated.Meta.Revision++
		updated.Meta.ContentHash = hash
		updated.Meta.UpdatedAt = service.clock().UTC()
		if err := service.checkResourceConflict(ctx, tx, updated, id); err != nil {
			return err
		}
		inputs, resolved, err := service.resolveWorkload(ctx, tx, updated)
		if err != nil {
			return err
		}
		if resolved.Hash != current.EffectiveHash {
			updated.DesiredGeneration++
			updated.EffectiveHash = resolved.Hash
		}
		if _, err = tx.ExecContext(ctx, `UPDATE desired_workloads SET revision=?,content_hash=?,updated_at_ns=?,name=?,host_id=?,profile_id=?,desired_run_state=?,worker=?,claim_cpu=?,desired_generation=?,effective_hash=? WHERE workload_id=?`, updated.Meta.Revision, updated.Meta.ContentHash, updated.Meta.UpdatedAt.UnixNano(), updated.Name, updated.HostID.String(), updated.ProfileID.String(), updated.RunState, updated.Worker, updated.Resources.CPU, updated.DesiredGeneration, updated.EffectiveHash, id.String()); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "DELETE FROM desired_workload_devices WHERE workload_id=?", id.String()); err != nil {
			return err
		}
		if err = insertWorkloadDevices(ctx, tx, id, updated.Resources.DeviceIDs); err != nil {
			return err
		}
		if resolved.Hash != current.EffectiveHash && updated.RunState == farmmodel.DesiredRunning {
			if err = service.createSnapshot(ctx, tx, inputs, resolved, updated.DesiredGeneration, updated.Meta.UpdatedAt); err != nil {
				return err
			}
		}
		resultObject = updated
		return nil
	})
	if err != nil {
		return farmmodel.DesiredWorkload{}, wrapDB("update DesiredWorkload", err)
	}
	return resultObject, nil
}

// DeleteDesiredWorkload is intentionally fail-closed in M4.2. Any historical
// snapshot means Controller may still own an execution; M4.3 will add observed
// retirement proof. Only a never-running STOPPED workload can be deleted now.
func (service *Service) DeleteDesiredWorkload(ctx context.Context, id identity.WorkloadID, expectedRevision uint64) error {
	if err := id.Validate(); err != nil {
		return typed(farmerr.CONFIG_CONFLICT, "invalid WorkloadID", err)
	}
	err := service.writeTx(ctx, func(tx *sql.Tx) error {
		current, err := service.loadWorkloadTx(ctx, tx, id)
		if err != nil {
			return err
		}
		if current.Meta.Revision != expectedRevision {
			return revisionConflict("DesiredWorkload", expectedRevision, current.Meta.Revision)
		}
		var snapshots int
		if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM resolved_execution_snapshots WHERE workload_id=?)", id.String()).Scan(&snapshots); err != nil {
			return err
		}
		if current.RunState != farmmodel.DesiredStopped || snapshots != 0 {
			return typed(farmerr.REFERENCE_IN_USE, "DesiredWorkload may still own a runtime execution; fresh retirement proof is required", nil)
		}
		_, err = tx.ExecContext(ctx, "DELETE FROM desired_workloads WHERE workload_id=?", id.String())
		return err
	})
	return wrapDB("delete DesiredWorkload", err)
}

// FinalizeRetiredDesiredWorkloadDeletion is a narrow coordinator storage
// boundary, deliberately excluded from the ordinary DesiredWorkloads CRUD
// interface. The caller must serialize it with runtime dispatch/result handling
// and establish fresh, causally sufficient retirement proof. Historical
// resolved snapshots remain append-only and queryable.
func (service *Service) FinalizeRetiredDesiredWorkloadDeletion(ctx context.Context, id identity.WorkloadID, expectedRevision uint64) error {
	if err := id.Validate(); err != nil {
		return typed(farmerr.CONFIG_CONFLICT, "invalid WorkloadID", err)
	}
	err := service.writeTx(ctx, func(tx *sql.Tx) error {
		current, err := service.loadWorkloadTx(ctx, tx, id)
		if err != nil {
			return err
		}
		if current.Meta.Revision != expectedRevision {
			return revisionConflict("DesiredWorkload", expectedRevision, current.Meta.Revision)
		}
		if current.RunState != farmmodel.DesiredStopped {
			return typed(farmerr.REFERENCE_IN_USE, "DesiredWorkload must be STOPPED before deletion", nil)
		}
		_, err = tx.ExecContext(ctx, "DELETE FROM desired_workloads WHERE workload_id=?", id.String())
		return err
	})
	return wrapDB("finalize retired DesiredWorkload deletion", err)
}

func (service *Service) BlockDesiredGeneration(ctx context.Context, id identity.WorkloadID, generation uint64, code farmerr.Code, message string) error {
	if err := id.Validate(); err != nil || generation < 1 || code == "" {
		return typed(farmerr.CONFIG_CONFLICT, "invalid blocked workload generation", err)
	}
	err := service.writeTx(ctx, func(tx *sql.Tx) error {
		workload, err := service.loadWorkloadTx(ctx, tx, id)
		if err != nil {
			return err
		}
		// Stale results are facts about an old snapshot, never state transitions
		// for the currently desired generation.
		if workload.RunState != farmmodel.DesiredRunning || workload.DesiredGeneration != generation {
			return nil
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO workload_runtime_bindings(workload_id,blocked_generation,error_code,human_message,blocked_at_ns)
			VALUES(?,?,?,?,?) ON CONFLICT(workload_id) DO UPDATE SET blocked_generation=excluded.blocked_generation,error_code=excluded.error_code,human_message=excluded.human_message,blocked_at_ns=excluded.blocked_at_ns`, id.String(), generation, code, message, service.clock().UTC().UnixNano())
		return err
	})
	return wrapDB("block DesiredWorkload generation", err)
}

func (service *Service) GetWorkloadRuntimeBinding(ctx context.Context, id identity.WorkloadID) (farmmodel.WorkloadRuntimeBinding, bool, error) {
	if err := id.Validate(); err != nil {
		return farmmodel.WorkloadRuntimeBinding{}, false, typed(farmerr.CONFIG_CONFLICT, "invalid WorkloadID", err)
	}
	var generation uint64
	var code, message string
	var blockedAt int64
	err := service.db.QueryRowContext(ctx, "SELECT blocked_generation,error_code,human_message,blocked_at_ns FROM workload_runtime_bindings WHERE workload_id=?", id.String()).Scan(&generation, &code, &message, &blockedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return farmmodel.WorkloadRuntimeBinding{}, false, nil
	}
	if err != nil {
		return farmmodel.WorkloadRuntimeBinding{}, false, wrapDB("read workload runtime binding", err)
	}
	if generation < 1 || code == "" || blockedAt <= 0 {
		return farmmodel.WorkloadRuntimeBinding{}, false, corrupt("stored workload runtime binding is invalid", nil)
	}
	return farmmodel.WorkloadRuntimeBinding{WorkloadID: id, BlockedGeneration: generation, ErrorCode: farmerr.Code(code), HumanMessage: message, BlockedAt: time.Unix(0, blockedAt).UTC()}, true, nil
}

// RetryWorkload is the explicit application operation that creates one new
// generation from the immutable effective runtime snapshot of a blocked one.
func (service *Service) RetryWorkload(ctx context.Context, id identity.WorkloadID, expectedRevision uint64) (farmmodel.DesiredWorkload, error) {
	if err := id.Validate(); err != nil {
		return farmmodel.DesiredWorkload{}, typed(farmerr.CONFIG_CONFLICT, "invalid WorkloadID", err)
	}
	var result farmmodel.DesiredWorkload
	err := service.writeTx(ctx, func(tx *sql.Tx) error {
		workload, err := service.loadWorkloadTx(ctx, tx, id)
		if err != nil {
			return err
		}
		if workload.Meta.Revision != expectedRevision {
			return revisionConflict("DesiredWorkload", expectedRevision, workload.Meta.Revision)
		}
		if workload.RunState != farmmodel.DesiredRunning {
			return typed(farmerr.CONFIG_CONFLICT, "only a RUNNING workload can be retried", nil)
		}
		var blocked uint64
		if err := tx.QueryRowContext(ctx, "SELECT blocked_generation FROM workload_runtime_bindings WHERE workload_id=?", id.String()).Scan(&blocked); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return typed(farmerr.CONFIG_CONFLICT, "DesiredWorkload generation is not blocked", nil)
			}
			return err
		}
		if blocked != workload.DesiredGeneration {
			return typed(farmerr.CONFIG_CONFLICT, "current DesiredWorkload generation is not blocked", nil)
		}
		previous, err := service.getSnapshot(ctx, tx, id, workload.DesiredGeneration)
		if err != nil {
			return err
		}
		executionID, err := service.newExecutionID()
		if err != nil {
			return typed(farmerr.INTERNAL_ERROR, "cannot generate ExecutionID", err)
		}
		var exists int
		if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM resolved_execution_snapshots WHERE execution_id=?)", executionID.String()).Scan(&exists); err != nil {
			return err
		}
		if exists != 0 {
			return typed(farmerr.ALREADY_EXISTS, "ExecutionID already exists", nil)
		}
		now := service.clock().UTC()
		workload.Meta.Revision++
		workload.Meta.UpdatedAt = now
		workload.DesiredGeneration++
		next := previous
		next.DesiredGeneration = workload.DesiredGeneration
		next.ExecutionID = executionID
		next.CreatedAt = now
		next.Plan.ExecutionID = executionID
		next.Plan.Ownership.DesiredGeneration = workload.DesiredGeneration
		next.Plan.Ownership.DeviceIDs = append([]identity.DeviceID(nil), previous.Plan.Ownership.DeviceIDs...)
		next.Plan.DeviceIDs = append([]identity.DeviceID(nil), previous.Plan.DeviceIDs...)
		if err := insertResolvedSnapshot(ctx, tx, next); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE desired_workloads SET revision=?,updated_at_ns=?,desired_generation=? WHERE workload_id=?", workload.Meta.Revision, now.UnixNano(), workload.DesiredGeneration, id.String()); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM workload_runtime_bindings WHERE workload_id=?", id.String()); err != nil {
			return err
		}
		result = workload
		return nil
	})
	if err != nil {
		return farmmodel.DesiredWorkload{}, wrapDB("retry DesiredWorkload", err)
	}
	return result, nil
}

func (service *Service) GetCurrentResolvedSnapshot(ctx context.Context, id identity.WorkloadID) (farmmodel.ResolvedExecutionSnapshot, error) {
	workload, err := service.GetDesiredWorkload(ctx, id)
	if err != nil {
		return farmmodel.ResolvedExecutionSnapshot{}, err
	}
	if workload.RunState != farmmodel.DesiredRunning {
		return farmmodel.ResolvedExecutionSnapshot{}, typed(farmerr.NOT_FOUND, "STOPPED DesiredWorkload has no active execution snapshot", nil)
	}
	return service.getSnapshot(ctx, service.db, id, workload.DesiredGeneration)
}

func (service *Service) ListResolvedSnapshots(ctx context.Context, id identity.WorkloadID) ([]farmmodel.ResolvedExecutionSnapshot, error) {
	if err := id.Validate(); err != nil {
		return nil, typed(farmerr.CONFIG_CONFLICT, "invalid WorkloadID", err)
	}
	rows, err := service.db.QueryContext(ctx, snapshotSelect+" WHERE workload_id=? ORDER BY desired_generation", id.String())
	if err != nil {
		return nil, wrapDB("list resolved snapshots", err)
	}
	var result []farmmodel.ResolvedExecutionSnapshot
	for rows.Next() {
		snapshot, err := scanSnapshot(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		result = append(result, snapshot)
	}
	if err := rows.Close(); err != nil {
		return nil, wrapDB("list resolved snapshots", err)
	}
	for i := range result {
		devices, err := loadSnapshotDevices(ctx, service.db, id, result[i].DesiredGeneration)
		if err != nil {
			return nil, err
		}
		result[i].Resources.DeviceIDs = devices
		if err := validateSnapshot(result[i]); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func (service *Service) ListAllResolvedSnapshots(ctx context.Context) ([]farmmodel.ResolvedExecutionSnapshot, error) {
	rows, err := service.db.QueryContext(ctx, snapshotSelect+" ORDER BY workload_id,desired_generation")
	if err != nil {
		return nil, wrapDB("list all resolved snapshots", err)
	}
	var result []farmmodel.ResolvedExecutionSnapshot
	for rows.Next() {
		snapshot, err := scanSnapshot(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		result = append(result, snapshot)
	}
	if err := rows.Close(); err != nil {
		return nil, wrapDB("list all resolved snapshots", err)
	}
	for i := range result {
		result[i].Resources.DeviceIDs, err = loadSnapshotDevices(ctx, service.db, result[i].WorkloadID, result[i].DesiredGeneration)
		if err != nil {
			return nil, err
		}
		if err := validateSnapshot(result[i]); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func (service *Service) resolveWorkload(ctx context.Context, tx *sql.Tx, workload farmmodel.DesiredWorkload) (farmresolve.Inputs, farmresolve.Result, error) {
	profile, err := scanProfile(tx.QueryRowContext(ctx, profileSelect+" WHERE profile_id=?", workload.ProfileID.String()))
	if err != nil {
		if code, _ := farmerr.CodeOf(err); code == farmerr.NOT_FOUND {
			return farmresolve.Inputs{}, farmresolve.Result{}, typed(farmerr.INVALID_REFERENCE, "DesiredWorkload references a missing MiningProfile", nil)
		}
		return farmresolve.Inputs{}, farmresolve.Result{}, err
	}
	pool, err := scanPool(tx.QueryRowContext(ctx, poolSelect+" WHERE pool_id=?", profile.PoolID.String()))
	if err != nil {
		return farmresolve.Inputs{}, farmresolve.Result{}, typed(farmerr.INVALID_REFERENCE, "MiningProfile references an unavailable Pool", err)
	}
	wallet, err := scanWallet(tx.QueryRowContext(ctx, walletSelect+" WHERE wallet_id=?", profile.WalletID.String()))
	if err != nil {
		return farmresolve.Inputs{}, farmresolve.Result{}, typed(farmerr.INVALID_REFERENCE, "MiningProfile references an unavailable WalletRef", err)
	}
	inputs := farmresolve.Inputs{Workload: workload, Profile: profile, Pool: pool, Wallet: wallet}
	resolved, err := service.resolver.Resolve(ctx, inputs)
	return inputs, resolved, err
}

func (service *Service) createSnapshot(ctx context.Context, tx *sql.Tx, inputs farmresolve.Inputs, result farmresolve.Result, generation uint64, created time.Time) error {
	executionID, err := service.newExecutionID()
	if err != nil {
		return typed(farmerr.INTERNAL_ERROR, "cannot generate ExecutionID", err)
	}
	var exists int
	if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM resolved_execution_snapshots WHERE execution_id=?)", executionID.String()).Scan(&exists); err != nil {
		return err
	}
	if exists != 0 {
		return typed(farmerr.ALREADY_EXISTS, "ExecutionID already exists", nil)
	}
	snapshot, err := service.resolver.Snapshot(result, inputs, executionID, generation, created)
	if err != nil {
		return err
	}
	return insertResolvedSnapshot(ctx, tx, snapshot)
}

func insertResolvedSnapshot(ctx context.Context, tx *sql.Tx, snapshot farmmodel.ResolvedExecutionSnapshot) error {
	if err := validateSnapshot(snapshot); err != nil {
		return err
	}
	planJSON, err := json.Marshal(snapshot.Plan)
	if err != nil {
		return typed(farmerr.INTERNAL_ERROR, "cannot encode immutable ExecutionPlan", err)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO resolved_execution_snapshots(workload_id,desired_generation,execution_id,host_id,profile_id,profile_revision,pool_id,pool_revision,wallet_id,wallet_revision,package_id,package_version,claim_cpu,plan_json,resolved_hash,created_at_ns) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, snapshot.WorkloadID.String(), snapshot.DesiredGeneration, snapshot.ExecutionID.String(), snapshot.HostID.String(), snapshot.ProfileID.String(), snapshot.ProfileRevision, snapshot.PoolID.String(), snapshot.PoolRevision, snapshot.WalletID.String(), snapshot.WalletRevision, snapshot.Package.PackageID.String(), snapshot.Package.Version, snapshot.Resources.CPU, planJSON, snapshot.ResolvedHash, snapshot.CreatedAt.UnixNano())
	if err != nil {
		return err
	}
	for _, device := range snapshot.Resources.DeviceIDs {
		if _, err = tx.ExecContext(ctx, "INSERT INTO resolved_snapshot_devices(workload_id,desired_generation,device_id) VALUES(?,?,?)", snapshot.WorkloadID.String(), snapshot.DesiredGeneration, device.String()); err != nil {
			return err
		}
	}
	return nil
}

func (service *Service) propagateProfileWorkloads(ctx context.Context, tx *sql.Tx, profileQuery string, arg any) error {
	rows, err := tx.QueryContext(ctx, profileQuery, arg)
	if err != nil {
		return err
	}
	var profiles []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		profiles = append(profiles, id)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	seen := map[string]struct{}{}
	var workloads []identity.WorkloadID
	for _, profileID := range profiles {
		rows, err := tx.QueryContext(ctx, "SELECT workload_id FROM desired_workloads WHERE profile_id=? ORDER BY workload_id", profileID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var raw string
			if err := rows.Scan(&raw); err != nil {
				rows.Close()
				return err
			}
			if _, ok := seen[raw]; ok {
				continue
			}
			id, err := identity.ParseWorkloadID(raw)
			if err != nil {
				rows.Close()
				return corrupt("stored WorkloadID is invalid", err)
			}
			seen[raw] = struct{}{}
			workloads = append(workloads, id)
		}
		if err := rows.Close(); err != nil {
			return err
		}
	}
	for _, id := range workloads {
		workload, err := service.loadWorkloadTx(ctx, tx, id)
		if err != nil {
			return err
		}
		inputs, resolved, err := service.resolveWorkload(ctx, tx, workload)
		if err != nil {
			return err
		}
		if resolved.Hash == workload.EffectiveHash {
			continue
		}
		workload.DesiredGeneration++
		workload.EffectiveHash = resolved.Hash
		if _, err = tx.ExecContext(ctx, "UPDATE desired_workloads SET desired_generation=?,effective_hash=? WHERE workload_id=?", workload.DesiredGeneration, workload.EffectiveHash, id.String()); err != nil {
			return err
		}
		if workload.RunState == farmmodel.DesiredRunning {
			if err = service.createSnapshot(ctx, tx, inputs, resolved, workload.DesiredGeneration, service.clock().UTC()); err != nil {
				return err
			}
		}
	}
	return nil
}

func (service *Service) checkResourceConflict(ctx context.Context, tx *sql.Tx, candidate farmmodel.DesiredWorkload, exclude identity.WorkloadID) error {
	if candidate.RunState != farmmodel.DesiredRunning {
		return nil
	}
	excludeID := exclude.String()
	if candidate.Resources.CPU {
		var conflict int
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM desired_workloads WHERE host_id=? AND desired_run_state='RUNNING' AND claim_cpu=1 AND workload_id<>?)`, candidate.HostID.String(), excludeID).Scan(&conflict); err != nil {
			return err
		}
		if conflict != 0 {
			return typed(farmerr.CONFIG_CONFLICT, "another RUNNING workload claims this Host CPU", nil)
		}
	}
	for _, device := range candidate.Resources.DeviceIDs {
		var conflict int
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM desired_workloads w JOIN desired_workload_devices d ON d.workload_id=w.workload_id WHERE w.host_id=? AND w.desired_run_state='RUNNING' AND d.device_id=? AND w.workload_id<>?)`, candidate.HostID.String(), device.String(), excludeID).Scan(&conflict); err != nil {
			return err
		}
		if conflict != 0 {
			return typed(farmerr.CONFIG_CONFLICT, "another RUNNING workload claims this DeviceID", nil)
		}
	}
	return nil
}

func (service *Service) loadWorkloadTx(ctx context.Context, tx *sql.Tx, id identity.WorkloadID) (farmmodel.DesiredWorkload, error) {
	object, err := scanWorkload(tx.QueryRowContext(ctx, workloadSelect+" WHERE workload_id=?", id.String()))
	if err != nil {
		return farmmodel.DesiredWorkload{}, err
	}
	object.Resources.DeviceIDs, err = loadWorkloadDevices(ctx, tx, id)
	if err != nil {
		return farmmodel.DesiredWorkload{}, err
	}
	return validateLoadedWorkload(object)
}

func scanWorkload(row scanner) (farmmodel.DesiredWorkload, error) {
	var object farmmodel.DesiredWorkload
	var workloadID, hostID, profileID string
	var created, updated int64
	var cpu int
	err := row.Scan(&workloadID, &object.Meta.SchemaVersion, &object.Meta.Revision, &object.Meta.ContentHash, &object.Meta.Origin, &created, &updated, &object.Name, &hostID, &profileID, &object.RunState, &object.Worker, &cpu, &object.DesiredGeneration, &object.EffectiveHash)
	if err != nil {
		return farmmodel.DesiredWorkload{}, scanError("DesiredWorkload", err)
	}
	var parseErr error
	if object.WorkloadID, parseErr = identity.ParseWorkloadID(workloadID); parseErr != nil {
		return farmmodel.DesiredWorkload{}, corrupt("stored WorkloadID is invalid", parseErr)
	}
	if object.HostID, parseErr = identity.ParseHostID(hostID); parseErr != nil {
		return farmmodel.DesiredWorkload{}, corrupt("stored workload HostID is invalid", parseErr)
	}
	if object.ProfileID, parseErr = identity.ParseProfileID(profileID); parseErr != nil {
		return farmmodel.DesiredWorkload{}, corrupt("stored workload ProfileID is invalid", parseErr)
	}
	if cpu != 0 && cpu != 1 {
		return farmmodel.DesiredWorkload{}, corrupt("stored CPU resource claim is invalid", nil)
	}
	object.Resources.CPU = cpu != 0
	object.Meta.CreatedAt = time.Unix(0, created).UTC()
	object.Meta.UpdatedAt = time.Unix(0, updated).UTC()
	return object, nil
}

func validateLoadedWorkload(object farmmodel.DesiredWorkload) (farmmodel.DesiredWorkload, error) {
	object.Resources = farmmodel.NormalizeResourceClaim(object.Resources)
	if err := farmmodel.ValidateDesiredWorkload(object.DesiredWorkloadContent); err != nil {
		return farmmodel.DesiredWorkload{}, corrupt("stored DesiredWorkload content is invalid", err)
	}
	hash, err := farmmodel.DesiredWorkloadHash(object.DesiredWorkloadContent)
	if err != nil || hash != object.Meta.ContentHash {
		return farmmodel.DesiredWorkload{}, corrupt("stored DesiredWorkload content hash does not match", err)
	}
	if err := validateStoredMeta(object.Meta); err != nil {
		return farmmodel.DesiredWorkload{}, err
	}
	if object.DesiredGeneration < 1 || !validResolvedHash(object.EffectiveHash) {
		return farmmodel.DesiredWorkload{}, corrupt("stored DesiredWorkload generation association is invalid", nil)
	}
	return object, nil
}

func loadWorkloadDevices(ctx context.Context, queryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, id identity.WorkloadID) ([]identity.DeviceID, error) {
	rows, err := queryer.QueryContext(ctx, "SELECT device_id FROM desired_workload_devices WHERE workload_id=? ORDER BY device_id", id.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []identity.DeviceID
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		device, err := identity.ParseDeviceID(raw)
		if err != nil {
			return nil, corrupt("stored workload DeviceID is invalid", err)
		}
		result = append(result, device)
	}
	return result, rows.Err()
}
func insertWorkloadDevices(ctx context.Context, tx *sql.Tx, id identity.WorkloadID, devices []identity.DeviceID) error {
	for _, device := range devices {
		if _, err := tx.ExecContext(ctx, "INSERT INTO desired_workload_devices(workload_id,device_id) VALUES(?,?)", id.String(), device.String()); err != nil {
			return err
		}
	}
	return nil
}
func workloadArgs(object farmmodel.DesiredWorkload) []any {
	return []any{object.WorkloadID.String(), object.Meta.SchemaVersion, object.Meta.Revision, object.Meta.ContentHash, object.Meta.Origin, object.Meta.CreatedAt.UnixNano(), object.Meta.UpdatedAt.UnixNano(), object.Name, object.HostID.String(), object.ProfileID.String(), object.RunState, object.Worker, object.Resources.CPU, object.DesiredGeneration, object.EffectiveHash}
}

func (service *Service) getSnapshot(ctx context.Context, queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, id identity.WorkloadID, generation uint64) (farmmodel.ResolvedExecutionSnapshot, error) {
	snapshot, err := scanSnapshot(queryer.QueryRowContext(ctx, snapshotSelect+" WHERE workload_id=? AND desired_generation=?", id.String(), generation))
	if err != nil {
		return farmmodel.ResolvedExecutionSnapshot{}, err
	}
	snapshot.Resources.DeviceIDs, err = loadSnapshotDevices(ctx, queryer, id, generation)
	if err != nil {
		return farmmodel.ResolvedExecutionSnapshot{}, err
	}
	if err := validateSnapshot(snapshot); err != nil {
		return farmmodel.ResolvedExecutionSnapshot{}, err
	}
	return snapshot, nil
}
func loadSnapshotDevices(ctx context.Context, queryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, id identity.WorkloadID, generation uint64) ([]identity.DeviceID, error) {
	rows, err := queryer.QueryContext(ctx, "SELECT device_id FROM resolved_snapshot_devices WHERE workload_id=? AND desired_generation=? ORDER BY device_id", id.String(), generation)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []identity.DeviceID
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		device, err := identity.ParseDeviceID(raw)
		if err != nil {
			return nil, corrupt("stored snapshot DeviceID is invalid", err)
		}
		result = append(result, device)
	}
	return result, rows.Err()
}

func scanSnapshot(row scanner) (farmmodel.ResolvedExecutionSnapshot, error) {
	var s farmmodel.ResolvedExecutionSnapshot
	var workload, execution, host, profile, pool, wallet, packageID string
	var cpu int
	var planJSON []byte
	var created int64
	err := row.Scan(&workload, &s.DesiredGeneration, &execution, &host, &profile, &s.ProfileRevision, &pool, &s.PoolRevision, &wallet, &s.WalletRevision, &packageID, &s.Package.Version, &cpu, &planJSON, &s.ResolvedHash, &created)
	if err != nil {
		return s, scanError("ResolvedExecutionSnapshot", err)
	}
	var e error
	if s.WorkloadID, e = identity.ParseWorkloadID(workload); e != nil {
		return s, corrupt("snapshot WorkloadID is invalid", e)
	}
	if s.ExecutionID, e = identity.ParseExecutionID(execution); e != nil {
		return s, corrupt("snapshot ExecutionID is invalid", e)
	}
	if s.HostID, e = identity.ParseHostID(host); e != nil {
		return s, corrupt("snapshot HostID is invalid", e)
	}
	if s.ProfileID, e = identity.ParseProfileID(profile); e != nil {
		return s, corrupt("snapshot ProfileID is invalid", e)
	}
	if s.PoolID, e = identity.ParsePoolID(pool); e != nil {
		return s, corrupt("snapshot PoolID is invalid", e)
	}
	if s.WalletID, e = identity.ParseWalletID(wallet); e != nil {
		return s, corrupt("snapshot WalletID is invalid", e)
	}
	if s.Package.PackageID, e = identity.ParsePackageID(packageID); e != nil {
		return s, corrupt("snapshot PackageID is invalid", e)
	}
	if cpu != 0 && cpu != 1 {
		return s, corrupt("snapshot CPU claim is invalid", nil)
	}
	s.Resources.CPU = cpu != 0
	s.CreatedAt = time.Unix(0, created).UTC()
	if err := json.Unmarshal(planJSON, &s.Plan); err != nil {
		return s, corrupt("snapshot ExecutionPlan is invalid", err)
	}
	return s, nil
}

func validateSnapshot(s farmmodel.ResolvedExecutionSnapshot) error {
	if s.DesiredGeneration < 1 || s.ProfileRevision < 1 || s.PoolRevision < 1 || s.WalletRevision < 1 || s.CreatedAt.IsZero() || s.ResolvedHash == "" {
		return corrupt("snapshot provenance is invalid", nil)
	}
	if err := farmmodel.ValidateResourceClaim(s.Resources); err != nil {
		return corrupt("snapshot resource claim is invalid", err)
	}
	ownership := s.Plan.Ownership
	if ownership.Validate() != nil || ownership.WorkloadID != s.WorkloadID || ownership.DesiredGeneration != s.DesiredGeneration || ownership.ResolvedHash != s.ResolvedHash || ownership.HostID != s.HostID || ownership.CPU != s.Resources.CPU || !slices.Equal(ownership.DeviceIDs, s.Resources.DeviceIDs) ||
		s.Plan.SchemaVersion != protocol.CurrentSchemaVersion || s.Plan.ExecutionID != s.ExecutionID || s.Plan.HostID != s.HostID || s.Plan.ProfileID != s.ProfileID ||
		s.Plan.Executable != "" || len(s.Plan.Args) != 0 || len(s.Plan.Environment) != 0 || s.Plan.WorkingDirectory != "" || s.Plan.RestartPolicy != model.RestartOnFailure ||
		s.Plan.Miner == nil || s.Plan.Miner.SpecVersion != 1 || s.Plan.Miner.Mode != model.MinerModeMining || s.Plan.Miner.Endpoint == nil || len(s.Plan.Miner.Options) != 0 ||
		s.Plan.Miner.PackageID != s.Package.PackageID || s.Plan.Miner.PackageVersion != s.Package.Version || s.Plan.Miner.PoolID == nil || *s.Plan.Miner.PoolID != s.PoolID || s.Plan.Miner.WalletID == nil || *s.Plan.Miner.WalletID != s.WalletID ||
		!equalUint32(s.Plan.CPUThreads, s.Plan.Miner.CPUThreads) || !slices.Equal(s.Plan.DeviceIDs, s.Resources.DeviceIDs) {
		return corrupt("snapshot ExecutionPlan identity does not match provenance", nil)
	}
	content := farmmodel.ResolvedRuntimeContent{RunState: farmmodel.DesiredRunning, HostID: s.HostID, ProfileID: s.ProfileID, Resources: s.Resources, AdapterID: s.Plan.Miner.AdapterID,
		Package: s.Package, Mode: farmmodel.ProfileMode(s.Plan.Miner.Mode), Coin: s.Plan.Miner.Coin, Algorithm: s.Plan.Miner.Algorithm,
		Endpoint: *s.Plan.Miner.Endpoint, CPUThreads: s.Plan.Miner.CPUThreads, HugePages: s.Plan.Miner.HugePages, MSR: s.Plan.Miner.MSR}
	hash, err := farmmodel.ResolvedRuntimeHash(content)
	if err != nil || hash != s.ResolvedHash {
		return corrupt("snapshot resolved hash does not match immutable plan", err)
	}
	return nil
}

func validResolvedHash(value string) bool {
	const prefix = "sha256:"
	if !strings.HasPrefix(value, prefix) || len(value) != len(prefix)+64 {
		return false
	}
	raw := strings.TrimPrefix(value, prefix)
	if raw != strings.ToLower(raw) {
		return false
	}
	_, err := hex.DecodeString(raw)
	return err == nil
}

func equalUint32(left, right *uint32) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}
