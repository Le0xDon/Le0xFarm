package controllerdb

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/le0xdon/le0xfarm/internal/farmerr"
	_ "modernc.org/sqlite"
)

func TestOpenMigratesSecuresAndReopens(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "controller")
	db, err := Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	var foreignKeys int
	if err := db.SQL().QueryRow("PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
		t.Fatal(err)
	}
	if foreignKeys != 1 {
		t.Fatal("foreign keys disabled")
	}
	var mode string
	if err := db.SQL().QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != "wal" {
		t.Fatalf("journal mode %q", mode)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	assertPerm(t, dir, 0700)
	assertPerm(t, filepath.Join(dir, FileName), 0600)
	db, err = Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err := db.SQL().QueryRow("SELECT count(*) FROM schema_migrations").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 5 {
		t.Fatalf("migration count %d", count)
	}
}

func TestOpenRejectsMigrationChecksumMismatch(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	path := db.Path()
	db.Close()
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = raw.Exec("UPDATE schema_migrations SET checksum='tampered' WHERE version=1")
	raw.Close()
	if err != nil {
		t.Fatal(err)
	}
	_, err = Open(context.Background(), dir)
	if code, _ := farmerr.CodeOf(err); code != farmerr.CONFIG_CONFLICT {
		t.Fatalf("got %v", err)
	}
}

func TestOpenRejectsNewerMigration(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	path := db.Path()
	db.Close()
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = raw.Exec("INSERT INTO schema_migrations VALUES(6,'future','hash',0)")
	raw.Close()
	if err != nil {
		t.Fatal(err)
	}
	_, err = Open(context.Background(), dir)
	if code, _ := farmerr.CodeOf(err); code != farmerr.CONFIG_CONFLICT {
		t.Fatalf("got %v", err)
	}
}

func TestOpenRejectsMigrationLedgerGap(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	path := db.Path()
	db.Close()
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = raw.Exec("DELETE FROM schema_migrations WHERE version=2")
	raw.Close()
	if err != nil {
		t.Fatal(err)
	}
	_, err = Open(context.Background(), dir)
	if code, _ := farmerr.CodeOf(err); code != farmerr.CONFIG_CONFLICT {
		t.Fatalf("got %v", err)
	}
}

func TestExistingM4DatabaseMigratesForward(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, name TEXT NOT NULL, checksum TEXT NOT NULL, applied_at_ns INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	migrations, err := loadMigrations()
	if err != nil || len(migrations) != 5 {
		t.Fatalf("migrations=%d err=%v", len(migrations), err)
	}
	for _, migration := range migrations[:3] {
		if _, err := raw.Exec(migration.sql); err != nil {
			t.Fatalf("apply M4 migration %d: %v", migration.version, err)
		}
		if _, err := raw.Exec("INSERT INTO schema_migrations VALUES(?,?,?,?)", migration.version, migration.name, migration.hash, time.Now().UnixNano()); err != nil {
			t.Fatal(err)
		}
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err := db.SQL().QueryRow("SELECT count(*) FROM schema_migrations").Scan(&count); err != nil || count != 5 {
		t.Fatalf("migration count=%d err=%v", count, err)
	}
	if _, err := db.SQL().Exec("SELECT host_profile_settings_revision FROM resolved_execution_snapshots LIMIT 0"); err != nil {
		t.Fatalf("M5 snapshot provenance column unavailable: %v", err)
	}
}

func TestForeignKeysAreEnforced(t *testing.T) {
	db, err := Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = db.SQL().Exec(`INSERT INTO mining_profiles(
		profile_id,object_schema_version,revision,content_hash,origin,created_at_ns,updated_at_ns,
		name,adapter_id,package_id,package_version,mode,coin,algorithm,pool_id,wallet_id,
		user_template,worker_placement) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		"profile_11111111111111111111111111111111", 1, 1, "sha256:x", "CONTROLLER", 0, 0,
		"profile", "adapter", "package_11111111111111111111111111111111", "1", "MINING", "COIN", "",
		"pool_11111111111111111111111111111111", "wallet_11111111111111111111111111111111", "${wallet}", "NONE")
	if err == nil {
		t.Fatal("profile with missing foreign references was accepted")
	}
}

func TestWALSidecarsHaveNoGroupOrWorldPermissions(t *testing.T) {
	db, err := Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.SQL().Exec(`INSERT INTO pools VALUES(
		'pool_11111111111111111111111111111111',1,1,'sha256:test','CONTROLLER',1,1,
		'pool','pool.example:1',0,'NONE','')`); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		path := db.Path() + suffix
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("expected active WAL sidecar %s: %v", path, err)
		}
		if got := info.Mode().Perm(); got&0077 != 0 {
			t.Fatalf("%s permissions %o expose group/other access", path, got)
		} else {
			t.Logf("%s permissions %04o", suffix, got)
		}
	}
}

func assertPerm(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s permissions %o want %o", path, got, want)
	}
}
