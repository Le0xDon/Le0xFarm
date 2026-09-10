CREATE TABLE workload_runtime_bindings (
    workload_id TEXT PRIMARY KEY REFERENCES desired_workloads(workload_id) ON DELETE CASCADE,
    blocked_generation INTEGER NOT NULL CHECK (blocked_generation > 0),
    error_code TEXT NOT NULL,
    human_message TEXT NOT NULL,
    blocked_at_ns INTEGER NOT NULL
);

