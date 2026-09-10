CREATE TABLE host_profile_settings (
    host_id TEXT NOT NULL,
    profile_id TEXT NOT NULL REFERENCES mining_profiles(profile_id) ON DELETE RESTRICT,
    object_schema_version INTEGER NOT NULL,
    revision INTEGER NOT NULL CHECK (revision > 0),
    content_hash TEXT NOT NULL,
    origin TEXT NOT NULL,
    created_at_ns INTEGER NOT NULL,
    updated_at_ns INTEGER NOT NULL,
    cpu_threads INTEGER CHECK (cpu_threads IS NULL OR cpu_threads > 0),
    huge_pages INTEGER CHECK (huge_pages IS NULL OR huge_pages IN (0, 1)),
    msr INTEGER CHECK (msr IS NULL OR msr IN (0, 1)),
    CHECK (cpu_threads IS NOT NULL OR huge_pages IS NOT NULL OR msr IS NOT NULL),
    PRIMARY KEY (host_id, profile_id)
);

CREATE INDEX host_profile_settings_profile_id ON host_profile_settings(profile_id);

ALTER TABLE resolved_execution_snapshots
    ADD COLUMN host_profile_settings_revision INTEGER NOT NULL DEFAULT 0;
