package farmconfig

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/le0xdon/le0xfarm/internal/farmerr"
	"github.com/le0xdon/le0xfarm/internal/farmmodel"
	"github.com/le0xdon/le0xfarm/internal/identity"
	incidentengine "github.com/le0xdon/le0xfarm/internal/incidents"
)

const incidentSelect = `SELECT incident_id,incident_key,incident_type,severity,lifecycle_state,host_id,workload_id,execution_id,device_id,first_observed_at_ns,last_observed_at_ns,resolved_at_ns,occurrence_count,reason_code,source FROM incidents`

// IncidentAuthority returns an opaque fingerprint of every persistent fact
// that can affect incident evaluation for one Host. ReconcileIncidents checks
// the same fingerprint inside its write transaction, closing the evaluation-
// to-publication window without exposing SQLite details to the Coordinator.
func (service *Service) IncidentAuthority(ctx context.Context, hostID identity.HostID) (string, error) {
	if err := hostID.Validate(); err != nil {
		return "", typed(farmerr.CONFIG_CONFLICT, "invalid incident HostID", err)
	}
	tx, err := service.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return "", wrapDB("begin incident authority read", err)
	}
	authority, err := incidentAuthorityTx(ctx, tx, hostID)
	if err != nil {
		tx.Rollback()
		return "", wrapDB("read incident authority", err)
	}
	if err := tx.Commit(); err != nil {
		return "", wrapDB("finish incident authority read", err)
	}
	return authority, nil
}

// ReconcileIncidents atomically activates current conditions and resolves only
// absent condition families for which the caller has fresh authority.
func (service *Service) ReconcileIncidents(ctx context.Context, hostID identity.HostID, expectedAuthority string, conditions []farmmodel.IncidentCondition, resolvable map[farmmodel.IncidentType]bool) error {
	if err := hostID.Validate(); err != nil {
		return typed(farmerr.CONFIG_CONFLICT, "invalid incident HostID", err)
	}
	if decoded, err := hex.DecodeString(expectedAuthority); err != nil || len(decoded) != sha256.Size {
		return typed(farmerr.CONFIG_CONFLICT, "invalid incident authority", err)
	}
	current := make(map[identity.IncidentID]farmmodel.IncidentCondition, len(conditions))
	for _, condition := range conditions {
		if err := farmmodel.ValidateIncidentCondition(condition); err != nil || condition.HostID != hostID {
			return typed(farmerr.CONFIG_CONFLICT, "invalid incident condition", err)
		}
		expected := incidentengine.NewCondition(condition.Type, condition.Severity, condition.HostID, condition.WorkloadID, condition.ExecutionID, condition.DeviceID, condition.ReasonCode)
		if expected.IncidentID != condition.IncidentID || expected.Key != condition.Key {
			return typed(farmerr.CONFIG_CONFLICT, "incident identity does not match typed scope", nil)
		}
		if _, exists := current[condition.IncidentID]; exists {
			return typed(farmerr.CONFIG_CONFLICT, "duplicate incident condition", nil)
		}
		current[condition.IncidentID] = condition
	}
	now := service.clock().UTC()
	err := service.writeTx(ctx, func(tx *sql.Tx) error {
		currentAuthority, err := incidentAuthorityTx(ctx, tx, hostID)
		if err != nil {
			return err
		}
		if currentAuthority != expectedAuthority {
			return typed(farmerr.REVISION_CONFLICT, "incident evaluation authority changed", nil)
		}
		rows, err := tx.QueryContext(ctx, incidentSelect+" WHERE host_id=? AND lifecycle_state=?", hostID.String(), farmmodel.IncidentActive)
		if err != nil {
			return err
		}
		var active []farmmodel.Incident
		for rows.Next() {
			item, err := scanIncident(rows)
			if err != nil {
				rows.Close()
				return err
			}
			active = append(active, item)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, incident := range active {
			if _, present := current[incident.IncidentID]; present || !resolvable[incident.Type] {
				continue
			}
			if _, err := tx.ExecContext(ctx, `UPDATE incidents SET lifecycle_state=?,last_observed_at_ns=?,resolved_at_ns=? WHERE incident_id=? AND lifecycle_state=?`, farmmodel.IncidentResolved, now.UnixNano(), now.UnixNano(), incident.IncidentID.String(), farmmodel.IncidentActive); err != nil {
				return err
			}
		}
		for _, condition := range conditions {
			var state string
			var storedKey string
			var count uint64
			err := tx.QueryRowContext(ctx, "SELECT lifecycle_state,occurrence_count,incident_key FROM incidents WHERE incident_id=?", condition.IncidentID.String()).Scan(&state, &count, &storedKey)
			switch {
			case err == sql.ErrNoRows:
				_, err = tx.ExecContext(ctx, `INSERT INTO incidents(incident_id,incident_key,incident_type,severity,lifecycle_state,host_id,workload_id,execution_id,device_id,first_observed_at_ns,last_observed_at_ns,resolved_at_ns,occurrence_count,reason_code,source) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, incidentArgs(condition, farmmodel.IncidentActive, now, now, nil, 1)...)
			case err != nil:
				return err
			case storedKey != condition.Key:
				return typed(farmerr.CONFIG_CONFLICT, "IncidentID collision with different typed scope", nil)
			case state == string(farmmodel.IncidentResolved):
				_, err = tx.ExecContext(ctx, `UPDATE incidents SET severity=?,lifecycle_state=?,last_observed_at_ns=?,resolved_at_ns=NULL,occurrence_count=?,reason_code=?,source=? WHERE incident_id=?`, condition.Severity, farmmodel.IncidentActive, now.UnixNano(), count+1, condition.ReasonCode, condition.Source, condition.IncidentID.String())
			default:
				_, err = tx.ExecContext(ctx, `UPDATE incidents SET severity=?,last_observed_at_ns=?,reason_code=?,source=? WHERE incident_id=?`, condition.Severity, now.UnixNano(), condition.ReasonCode, condition.Source, condition.IncidentID.String())
			}
			if err != nil {
				return err
			}
		}
		return nil
	})
	return wrapDB("reconcile incidents", err)
}

func incidentAuthorityTx(ctx context.Context, tx *sql.Tx, hostID identity.HostID) (string, error) {
	digest := sha256.New()
	writeAuthorityFields(digest, "HOST", hostID.String())
	var holdActive bool
	var holdRevision int64
	err := tx.QueryRowContext(ctx, `SELECT active,revision FROM maintenance_holds WHERE host_id=?`, hostID.String()).Scan(&holdActive, &holdRevision)
	if err == nil {
		writeAuthorityFields(digest, "HOLD", strconv.FormatBool(holdActive), strconv.FormatInt(holdRevision, 10))
	} else if err != sql.ErrNoRows {
		return "", err
	}

	rows, err := tx.QueryContext(ctx, `SELECT workload_id,revision,content_hash,desired_run_state,claim_cpu,desired_generation,effective_hash FROM desired_workloads WHERE host_id=? ORDER BY workload_id`, hostID.String())
	if err != nil {
		return "", err
	}
	for rows.Next() {
		var workloadID, contentHash, runState, effectiveHash string
		var revision, generation int64
		var cpu bool
		if err := rows.Scan(&workloadID, &revision, &contentHash, &runState, &cpu, &generation, &effectiveHash); err != nil {
			rows.Close()
			return "", err
		}
		writeAuthorityFields(digest, "WORKLOAD", workloadID, strconv.FormatInt(revision, 10), contentHash, runState, strconv.FormatBool(cpu), strconv.FormatInt(generation, 10), effectiveHash)
	}
	if err := closeAuthorityRows(rows); err != nil {
		return "", err
	}

	rows, err = tx.QueryContext(ctx, `SELECT devices.workload_id,devices.device_id FROM desired_workload_devices AS devices JOIN desired_workloads AS workloads ON workloads.workload_id=devices.workload_id WHERE workloads.host_id=? ORDER BY devices.workload_id,devices.device_id`, hostID.String())
	if err != nil {
		return "", err
	}
	for rows.Next() {
		var workloadID, deviceID string
		if err := rows.Scan(&workloadID, &deviceID); err != nil {
			rows.Close()
			return "", err
		}
		writeAuthorityFields(digest, "WORKLOAD_DEVICE", workloadID, deviceID)
	}
	if err := closeAuthorityRows(rows); err != nil {
		return "", err
	}

	rows, err = tx.QueryContext(ctx, `SELECT workload_id,desired_generation,execution_id,resolved_hash,claim_cpu FROM resolved_execution_snapshots WHERE host_id=? ORDER BY workload_id,desired_generation`, hostID.String())
	if err != nil {
		return "", err
	}
	for rows.Next() {
		var workloadID, executionID, resolvedHash string
		var generation int64
		var cpu bool
		if err := rows.Scan(&workloadID, &generation, &executionID, &resolvedHash, &cpu); err != nil {
			rows.Close()
			return "", err
		}
		writeAuthorityFields(digest, "SNAPSHOT", workloadID, strconv.FormatInt(generation, 10), executionID, resolvedHash, strconv.FormatBool(cpu))
	}
	if err := closeAuthorityRows(rows); err != nil {
		return "", err
	}

	rows, err = tx.QueryContext(ctx, `SELECT devices.workload_id,devices.desired_generation,devices.device_id FROM resolved_snapshot_devices AS devices JOIN resolved_execution_snapshots AS snapshots ON snapshots.workload_id=devices.workload_id AND snapshots.desired_generation=devices.desired_generation WHERE snapshots.host_id=? ORDER BY devices.workload_id,devices.desired_generation,devices.device_id`, hostID.String())
	if err != nil {
		return "", err
	}
	for rows.Next() {
		var workloadID, deviceID string
		var generation int64
		if err := rows.Scan(&workloadID, &generation, &deviceID); err != nil {
			rows.Close()
			return "", err
		}
		writeAuthorityFields(digest, "SNAPSHOT_DEVICE", workloadID, strconv.FormatInt(generation, 10), deviceID)
	}
	if err := closeAuthorityRows(rows); err != nil {
		return "", err
	}

	rows, err = tx.QueryContext(ctx, `SELECT bindings.workload_id,bindings.blocked_generation,bindings.error_code,bindings.blocked_at_ns FROM workload_runtime_bindings AS bindings JOIN desired_workloads AS workloads ON workloads.workload_id=bindings.workload_id WHERE workloads.host_id=? ORDER BY bindings.workload_id`, hostID.String())
	if err != nil {
		return "", err
	}
	for rows.Next() {
		var workloadID, errorCode string
		var generation, blockedAt int64
		if err := rows.Scan(&workloadID, &generation, &errorCode, &blockedAt); err != nil {
			rows.Close()
			return "", err
		}
		writeAuthorityFields(digest, "BINDING", workloadID, strconv.FormatInt(generation, 10), errorCode, strconv.FormatInt(blockedAt, 10))
	}
	if err := closeAuthorityRows(rows); err != nil {
		return "", err
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func writeAuthorityFields(digest hash.Hash, fields ...string) {
	var size [8]byte
	for _, field := range fields {
		binary.BigEndian.PutUint64(size[:], uint64(len(field)))
		_, _ = digest.Write(size[:])
		_, _ = io.WriteString(digest, field)
	}
}

func closeAuthorityRows(rows *sql.Rows) error {
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	return rows.Close()
}

// resolveDeletedWorkloadIncidentsTx closes only incidents scoped to a
// deliberately deleted workload. Host-scoped monitoring/safety incidents are
// intentionally untouched. Running this in the deletion transaction makes the
// lifecycle crash-safe and prevents orphan ACTIVE workload incidents.
func (service *Service) resolveDeletedWorkloadIncidentsTx(ctx context.Context, tx *sql.Tx, workloadID identity.WorkloadID) error {
	now := service.clock().UTC().UnixNano()
	_, err := tx.ExecContext(ctx, `UPDATE incidents SET lifecycle_state=?,last_observed_at_ns=?,resolved_at_ns=? WHERE workload_id=? AND lifecycle_state=?`, farmmodel.IncidentResolved, now, now, workloadID.String(), farmmodel.IncidentActive)
	return err
}

func (service *Service) ListActiveIncidents(ctx context.Context) ([]farmmodel.Incident, error) {
	return service.ListIncidents(ctx, farmmodel.IncidentQuery{State: farmmodel.IncidentActive})
}

// ListIncidents is the local Core diagnostic access boundary. It deliberately
// returns normalized incident records, not raw logs or telemetry samples.
func (service *Service) ListIncidents(ctx context.Context, query farmmodel.IncidentQuery) ([]farmmodel.Incident, error) {
	limit := query.Limit
	if limit == 0 {
		limit = 200
	}
	if limit < 1 || limit > 1000 {
		return nil, typed(farmerr.CONFIG_CONFLICT, "incident query limit must be between 1 and 1000", nil)
	}
	clauses := []string{"1=1"}
	var args []any
	if query.HostID != nil {
		if err := query.HostID.Validate(); err != nil {
			return nil, typed(farmerr.CONFIG_CONFLICT, "invalid incident query HostID", err)
		}
		clauses = append(clauses, "host_id=?")
		args = append(args, query.HostID.String())
	}
	if query.State != "" {
		if query.State != farmmodel.IncidentActive && query.State != farmmodel.IncidentResolved {
			return nil, typed(farmerr.CONFIG_CONFLICT, "invalid incident query state", nil)
		}
		clauses = append(clauses, "lifecycle_state=?")
		args = append(args, query.State)
	}
	args = append(args, limit)
	rows, err := service.db.QueryContext(ctx, incidentSelect+" WHERE "+strings.Join(clauses, " AND ")+" ORDER BY last_observed_at_ns DESC,incident_id LIMIT ?", args...)
	if err != nil {
		return nil, wrapDB("list incidents", err)
	}
	defer rows.Close()
	var result []farmmodel.Incident
	for rows.Next() {
		item, err := scanIncident(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, wrapDB("list incidents", rows.Err())
}

func incidentArgs(condition farmmodel.IncidentCondition, state farmmodel.IncidentState, first, last time.Time, resolved *time.Time, count uint64) []any {
	var resolvedNS any
	if resolved != nil {
		resolvedNS = resolved.UnixNano()
	}
	return []any{condition.IncidentID.String(), condition.Key, condition.Type, condition.Severity, state, condition.HostID.String(), optionalID(condition.WorkloadID), optionalID(condition.ExecutionID), optionalID(condition.DeviceID), first.UnixNano(), last.UnixNano(), resolvedNS, count, condition.ReasonCode, condition.Source}
}

func optionalID[T interface{ String() string }](value *T) any {
	if value == nil {
		return nil
	}
	return (*value).String()
}

func scanIncident(row scanner) (farmmodel.Incident, error) {
	var item farmmodel.Incident
	var incidentID, hostID string
	var workloadID, executionID, deviceID sql.NullString
	var first, last int64
	var resolved sql.NullInt64
	if err := row.Scan(&incidentID, &item.Key, &item.Type, &item.Severity, &item.State, &hostID, &workloadID, &executionID, &deviceID, &first, &last, &resolved, &item.OccurrenceCount, &item.ReasonCode, &item.Source); err != nil {
		return farmmodel.Incident{}, scanError("Incident", err)
	}
	var err error
	if item.IncidentID, err = identity.ParseIncidentID(incidentID); err != nil {
		return farmmodel.Incident{}, corrupt("stored IncidentID is invalid", err)
	}
	if item.HostID, err = identity.ParseHostID(hostID); err != nil {
		return farmmodel.Incident{}, corrupt("stored incident HostID is invalid", err)
	}
	if workloadID.Valid {
		value, parseErr := identity.ParseWorkloadID(workloadID.String)
		if parseErr != nil {
			return farmmodel.Incident{}, corrupt("stored incident WorkloadID is invalid", parseErr)
		}
		item.WorkloadID = &value
	}
	if executionID.Valid {
		value, parseErr := identity.ParseExecutionID(executionID.String)
		if parseErr != nil {
			return farmmodel.Incident{}, corrupt("stored incident ExecutionID is invalid", parseErr)
		}
		item.ExecutionID = &value
	}
	if deviceID.Valid {
		value, parseErr := identity.ParseDeviceID(deviceID.String)
		if parseErr != nil {
			return farmmodel.Incident{}, corrupt("stored incident DeviceID is invalid", parseErr)
		}
		item.DeviceID = &value
	}
	item.FirstObservedAt = time.Unix(0, first).UTC()
	item.LastObservedAt = time.Unix(0, last).UTC()
	if resolved.Valid {
		value := time.Unix(0, resolved.Int64).UTC()
		item.ResolvedAt = &value
	}
	if item.State != farmmodel.IncidentActive && item.State != farmmodel.IncidentResolved || item.OccurrenceCount == 0 || !farmmodel.ValidIncidentType(item.Type) || !farmmodel.ValidIncidentSeverity(item.Severity) || item.Source != farmmodel.IncidentSourceController {
		return farmmodel.Incident{}, corrupt("stored incident classification is invalid", fmt.Errorf("invalid incident fields"))
	}
	if item.FirstObservedAt.IsZero() || item.LastObservedAt.Before(item.FirstObservedAt) || item.State == farmmodel.IncidentActive && item.ResolvedAt != nil || item.State == farmmodel.IncidentResolved && (item.ResolvedAt == nil || item.ResolvedAt.Before(item.FirstObservedAt)) {
		return farmmodel.Incident{}, corrupt("stored incident lifecycle is invalid", nil)
	}
	if err := farmmodel.ValidateIncidentCondition(item.IncidentCondition); err != nil {
		return farmmodel.Incident{}, corrupt("stored incident condition is invalid", err)
	}
	expected := incidentengine.NewCondition(item.Type, item.Severity, item.HostID, item.WorkloadID, item.ExecutionID, item.DeviceID, item.ReasonCode)
	if expected.IncidentID != item.IncidentID || expected.Key != item.Key {
		return farmmodel.Incident{}, corrupt("stored incident identity does not match typed scope", nil)
	}
	return item, nil
}
