CREATE TABLE pools (
    pool_id TEXT PRIMARY KEY,
    object_schema_version INTEGER NOT NULL,
    revision INTEGER NOT NULL CHECK (revision > 0),
    content_hash TEXT NOT NULL,
    origin TEXT NOT NULL,
    created_at_ns INTEGER NOT NULL,
    updated_at_ns INTEGER NOT NULL,
    name TEXT NOT NULL,
    address TEXT NOT NULL,
    tls INTEGER NOT NULL CHECK (tls IN (0, 1)),
    auth_kind TEXT NOT NULL,
    public_literal TEXT NOT NULL
);

CREATE TABLE wallet_refs (
    wallet_id TEXT PRIMARY KEY,
    object_schema_version INTEGER NOT NULL,
    revision INTEGER NOT NULL CHECK (revision > 0),
    content_hash TEXT NOT NULL,
    origin TEXT NOT NULL,
    created_at_ns INTEGER NOT NULL,
    updated_at_ns INTEGER NOT NULL,
    name TEXT NOT NULL,
    coin TEXT NOT NULL,
    address TEXT NOT NULL
);

CREATE TABLE mining_profiles (
    profile_id TEXT PRIMARY KEY,
    object_schema_version INTEGER NOT NULL,
    revision INTEGER NOT NULL CHECK (revision > 0),
    content_hash TEXT NOT NULL,
    origin TEXT NOT NULL,
    created_at_ns INTEGER NOT NULL,
    updated_at_ns INTEGER NOT NULL,
    name TEXT NOT NULL,
    adapter_id TEXT NOT NULL,
    package_id TEXT NOT NULL,
    package_version TEXT NOT NULL,
    mode TEXT NOT NULL,
    coin TEXT NOT NULL,
    algorithm TEXT NOT NULL,
    pool_id TEXT NOT NULL REFERENCES pools(pool_id) ON DELETE RESTRICT,
    wallet_id TEXT NOT NULL REFERENCES wallet_refs(wallet_id) ON DELETE RESTRICT,
    user_template TEXT NOT NULL,
    worker_placement TEXT NOT NULL,
    cpu_threads INTEGER,
    huge_pages INTEGER CHECK (huge_pages IS NULL OR huge_pages IN (0, 1)),
    msr INTEGER CHECK (msr IS NULL OR msr IN (0, 1))
);

CREATE INDEX mining_profiles_pool_id ON mining_profiles(pool_id);
CREATE INDEX mining_profiles_wallet_id ON mining_profiles(wallet_id);
