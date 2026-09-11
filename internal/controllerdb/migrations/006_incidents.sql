CREATE TABLE incidents (
    incident_id TEXT PRIMARY KEY,
    incident_key TEXT NOT NULL UNIQUE,
    incident_type TEXT NOT NULL,
    severity TEXT NOT NULL,
    lifecycle_state TEXT NOT NULL CHECK (lifecycle_state IN ('ACTIVE', 'RESOLVED')),
    host_id TEXT NOT NULL,
    workload_id TEXT,
    execution_id TEXT,
    device_id TEXT,
    first_observed_at_ns INTEGER NOT NULL,
    last_observed_at_ns INTEGER NOT NULL,
    resolved_at_ns INTEGER,
    occurrence_count INTEGER NOT NULL CHECK (occurrence_count > 0),
    reason_code TEXT NOT NULL,
    source TEXT NOT NULL
);

CREATE INDEX incidents_state_last_idx ON incidents(lifecycle_state, last_observed_at_ns DESC);
CREATE INDEX incidents_host_state_idx ON incidents(host_id, lifecycle_state);
