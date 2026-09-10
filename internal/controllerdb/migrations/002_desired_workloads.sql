CREATE TABLE desired_workloads (
    workload_id TEXT PRIMARY KEY,
    object_schema_version INTEGER NOT NULL,
    revision INTEGER NOT NULL CHECK (revision > 0),
    content_hash TEXT NOT NULL,
    origin TEXT NOT NULL,
    created_at_ns INTEGER NOT NULL,
    updated_at_ns INTEGER NOT NULL,
    name TEXT NOT NULL,
    host_id TEXT NOT NULL,
    profile_id TEXT NOT NULL REFERENCES mining_profiles(profile_id) ON DELETE RESTRICT,
    desired_run_state TEXT NOT NULL CHECK (desired_run_state IN ('RUNNING', 'STOPPED')),
    worker TEXT NOT NULL,
    claim_cpu INTEGER NOT NULL CHECK (claim_cpu IN (0, 1)),
    desired_generation INTEGER NOT NULL CHECK (desired_generation > 0),
    effective_hash TEXT NOT NULL
);

CREATE INDEX desired_workloads_profile_id ON desired_workloads(profile_id);
CREATE INDEX desired_workloads_host_state ON desired_workloads(host_id, desired_run_state);

CREATE TABLE desired_workload_devices (
    workload_id TEXT NOT NULL REFERENCES desired_workloads(workload_id) ON DELETE CASCADE,
    device_id TEXT NOT NULL,
    PRIMARY KEY (workload_id, device_id)
);

CREATE INDEX desired_workload_devices_device_id ON desired_workload_devices(device_id);

CREATE TABLE resolved_execution_snapshots (
    workload_id TEXT NOT NULL,
    desired_generation INTEGER NOT NULL,
    execution_id TEXT NOT NULL UNIQUE,
    host_id TEXT NOT NULL,
    profile_id TEXT NOT NULL,
    profile_revision INTEGER NOT NULL,
    pool_id TEXT NOT NULL,
    pool_revision INTEGER NOT NULL,
    wallet_id TEXT NOT NULL,
    wallet_revision INTEGER NOT NULL,
    package_id TEXT NOT NULL,
    package_version TEXT NOT NULL,
    claim_cpu INTEGER NOT NULL CHECK (claim_cpu IN (0, 1)),
    plan_json BLOB NOT NULL,
    resolved_hash TEXT NOT NULL,
    created_at_ns INTEGER NOT NULL,
    PRIMARY KEY (workload_id, desired_generation)
);

CREATE TABLE resolved_snapshot_devices (
    workload_id TEXT NOT NULL,
    desired_generation INTEGER NOT NULL,
    device_id TEXT NOT NULL,
    PRIMARY KEY (workload_id, desired_generation, device_id),
    FOREIGN KEY (workload_id, desired_generation)
        REFERENCES resolved_execution_snapshots(workload_id, desired_generation)
        ON DELETE RESTRICT
);

CREATE INDEX resolved_snapshots_execution_id ON resolved_execution_snapshots(execution_id);

CREATE TRIGGER resolved_snapshots_no_update
BEFORE UPDATE ON resolved_execution_snapshots
BEGIN
    SELECT RAISE(ABORT, 'resolved execution snapshots are immutable');
END;

CREATE TRIGGER resolved_snapshots_no_delete
BEFORE DELETE ON resolved_execution_snapshots
BEGIN
    SELECT RAISE(ABORT, 'resolved execution snapshots are append-only');
END;

CREATE TRIGGER resolved_snapshot_devices_no_update
BEFORE UPDATE ON resolved_snapshot_devices
BEGIN
    SELECT RAISE(ABORT, 'resolved snapshot resources are immutable');
END;

CREATE TRIGGER resolved_snapshot_devices_no_delete
BEFORE DELETE ON resolved_snapshot_devices
BEGIN
    SELECT RAISE(ABORT, 'resolved snapshot resources are append-only');
END;
