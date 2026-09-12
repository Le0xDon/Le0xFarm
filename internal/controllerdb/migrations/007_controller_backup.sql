CREATE TABLE controller_backup_state (
    singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
    persistent_revision INTEGER NOT NULL CHECK (persistent_revision > 0),
    last_automatic_revision INTEGER NOT NULL CHECK (last_automatic_revision >= 0),
    restore_required INTEGER NOT NULL CHECK (restore_required IN (0, 1)),
    restore_backup_id TEXT NOT NULL,
    source_controller_id TEXT NOT NULL,
    source_farm_id TEXT NOT NULL,
    source_trust_fingerprint TEXT NOT NULL,
    restored_at_ns INTEGER NOT NULL,
    retention_pending INTEGER NOT NULL CHECK (retention_pending IN (0, 1)),
    artifact_backup_id TEXT NOT NULL,
    artifact_class TEXT NOT NULL,
    artifact_controller_id TEXT NOT NULL,
    artifact_farm_id TEXT NOT NULL,
    artifact_trust_fingerprint TEXT NOT NULL,
    artifact_created_at_ns INTEGER NOT NULL
);

INSERT INTO controller_backup_state(singleton,persistent_revision,last_automatic_revision,restore_required,restore_backup_id,source_controller_id,source_farm_id,source_trust_fingerprint,restored_at_ns,retention_pending,artifact_backup_id,artifact_class,artifact_controller_id,artifact_farm_id,artifact_trust_fingerprint,artifact_created_at_ns)
VALUES(1,1,0,0,'','','','',0,0,'','','','','',0);

CREATE TABLE restore_host_barriers (
    host_id TEXT PRIMARY KEY,
    backup_id TEXT NOT NULL,
    revision INTEGER NOT NULL CHECK (revision > 0),
    status TEXT NOT NULL CHECK (status IN ('PENDING', 'CONFLICT')),
    reason_code TEXT NOT NULL,
    updated_at_ns INTEGER NOT NULL
);

CREATE TRIGGER backup_revision_pools_insert AFTER INSERT ON pools BEGIN UPDATE controller_backup_state SET persistent_revision=persistent_revision+1 WHERE singleton=1; END;
CREATE TRIGGER backup_revision_pools_update AFTER UPDATE ON pools BEGIN UPDATE controller_backup_state SET persistent_revision=persistent_revision+1 WHERE singleton=1; END;
CREATE TRIGGER backup_revision_pools_delete AFTER DELETE ON pools BEGIN UPDATE controller_backup_state SET persistent_revision=persistent_revision+1 WHERE singleton=1; END;

CREATE TRIGGER backup_revision_wallets_insert AFTER INSERT ON wallet_refs BEGIN UPDATE controller_backup_state SET persistent_revision=persistent_revision+1 WHERE singleton=1; END;
CREATE TRIGGER backup_revision_wallets_update AFTER UPDATE ON wallet_refs BEGIN UPDATE controller_backup_state SET persistent_revision=persistent_revision+1 WHERE singleton=1; END;
CREATE TRIGGER backup_revision_wallets_delete AFTER DELETE ON wallet_refs BEGIN UPDATE controller_backup_state SET persistent_revision=persistent_revision+1 WHERE singleton=1; END;

CREATE TRIGGER backup_revision_profiles_insert AFTER INSERT ON mining_profiles BEGIN UPDATE controller_backup_state SET persistent_revision=persistent_revision+1 WHERE singleton=1; END;
CREATE TRIGGER backup_revision_profiles_update AFTER UPDATE ON mining_profiles BEGIN UPDATE controller_backup_state SET persistent_revision=persistent_revision+1 WHERE singleton=1; END;
CREATE TRIGGER backup_revision_profiles_delete AFTER DELETE ON mining_profiles BEGIN UPDATE controller_backup_state SET persistent_revision=persistent_revision+1 WHERE singleton=1; END;

CREATE TRIGGER backup_revision_settings_insert AFTER INSERT ON host_profile_settings BEGIN UPDATE controller_backup_state SET persistent_revision=persistent_revision+1 WHERE singleton=1; END;
CREATE TRIGGER backup_revision_settings_update AFTER UPDATE ON host_profile_settings BEGIN UPDATE controller_backup_state SET persistent_revision=persistent_revision+1 WHERE singleton=1; END;
CREATE TRIGGER backup_revision_settings_delete AFTER DELETE ON host_profile_settings BEGIN UPDATE controller_backup_state SET persistent_revision=persistent_revision+1 WHERE singleton=1; END;

CREATE TRIGGER backup_revision_workloads_insert AFTER INSERT ON desired_workloads BEGIN UPDATE controller_backup_state SET persistent_revision=persistent_revision+1 WHERE singleton=1; END;
CREATE TRIGGER backup_revision_workloads_update AFTER UPDATE ON desired_workloads BEGIN UPDATE controller_backup_state SET persistent_revision=persistent_revision+1 WHERE singleton=1; END;
CREATE TRIGGER backup_revision_workloads_delete AFTER DELETE ON desired_workloads BEGIN UPDATE controller_backup_state SET persistent_revision=persistent_revision+1 WHERE singleton=1; END;
CREATE TRIGGER backup_revision_workload_devices_insert AFTER INSERT ON desired_workload_devices BEGIN UPDATE controller_backup_state SET persistent_revision=persistent_revision+1 WHERE singleton=1; END;
CREATE TRIGGER backup_revision_workload_devices_delete AFTER DELETE ON desired_workload_devices BEGIN UPDATE controller_backup_state SET persistent_revision=persistent_revision+1 WHERE singleton=1; END;

CREATE TRIGGER backup_revision_snapshots_insert AFTER INSERT ON resolved_execution_snapshots BEGIN UPDATE controller_backup_state SET persistent_revision=persistent_revision+1 WHERE singleton=1; END;
CREATE TRIGGER backup_revision_snapshot_devices_insert AFTER INSERT ON resolved_snapshot_devices BEGIN UPDATE controller_backup_state SET persistent_revision=persistent_revision+1 WHERE singleton=1; END;

CREATE TRIGGER backup_revision_bindings_insert AFTER INSERT ON workload_runtime_bindings BEGIN UPDATE controller_backup_state SET persistent_revision=persistent_revision+1 WHERE singleton=1; END;
CREATE TRIGGER backup_revision_bindings_update AFTER UPDATE ON workload_runtime_bindings BEGIN UPDATE controller_backup_state SET persistent_revision=persistent_revision+1 WHERE singleton=1; END;
CREATE TRIGGER backup_revision_bindings_delete AFTER DELETE ON workload_runtime_bindings BEGIN UPDATE controller_backup_state SET persistent_revision=persistent_revision+1 WHERE singleton=1; END;

CREATE TRIGGER backup_revision_holds_insert AFTER INSERT ON maintenance_holds BEGIN UPDATE controller_backup_state SET persistent_revision=persistent_revision+1 WHERE singleton=1; END;
CREATE TRIGGER backup_revision_holds_update AFTER UPDATE ON maintenance_holds BEGIN UPDATE controller_backup_state SET persistent_revision=persistent_revision+1 WHERE singleton=1; END;
CREATE TRIGGER backup_revision_holds_delete AFTER DELETE ON maintenance_holds BEGIN UPDATE controller_backup_state SET persistent_revision=persistent_revision+1 WHERE singleton=1; END;

CREATE TRIGGER backup_revision_incidents_insert AFTER INSERT ON incidents BEGIN UPDATE controller_backup_state SET persistent_revision=persistent_revision+1 WHERE singleton=1; END;
CREATE TRIGGER backup_revision_incidents_update AFTER UPDATE ON incidents
WHEN OLD.incident_key IS NOT NEW.incident_key
  OR OLD.incident_type IS NOT NEW.incident_type
  OR OLD.severity IS NOT NEW.severity
  OR OLD.lifecycle_state IS NOT NEW.lifecycle_state
  OR OLD.host_id IS NOT NEW.host_id
  OR OLD.workload_id IS NOT NEW.workload_id
  OR OLD.execution_id IS NOT NEW.execution_id
  OR OLD.device_id IS NOT NEW.device_id
  OR OLD.resolved_at_ns IS NOT NEW.resolved_at_ns
  OR OLD.occurrence_count IS NOT NEW.occurrence_count
  OR OLD.reason_code IS NOT NEW.reason_code
  OR OLD.source IS NOT NEW.source
BEGIN UPDATE controller_backup_state SET persistent_revision=persistent_revision+1 WHERE singleton=1; END;
CREATE TRIGGER backup_revision_incidents_delete AFTER DELETE ON incidents BEGIN UPDATE controller_backup_state SET persistent_revision=persistent_revision+1 WHERE singleton=1; END;
