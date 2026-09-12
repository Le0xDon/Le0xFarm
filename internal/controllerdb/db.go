// Package controllerdb owns the canonical SQLite database and its migrations.
package controllerdb

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/le0xdon/le0xfarm/internal/farmerr"
	_ "modernc.org/sqlite"
)

const FileName = "farm.db"

const SchemaVersion = 7

//go:embed migrations/*.sql
var migrationFiles embed.FS

type DB struct {
	sql  *sql.DB
	path string
}

type migration struct {
	version int
	name    string
	sql     string
	hash    string
}

func Open(ctx context.Context, controllerDataDir string) (*DB, error) {
	if controllerDataDir == "" {
		return nil, typed(farmerr.CONFIG_CONFLICT, "Controller data directory is required", nil)
	}
	if err := os.MkdirAll(controllerDataDir, 0700); err != nil {
		return nil, typed(farmerr.PERMISSION_DENIED, "cannot create Controller data directory", err)
	}
	if err := os.Chmod(controllerDataDir, 0700); err != nil {
		return nil, typed(farmerr.PERMISSION_DENIED, "cannot secure Controller data directory", err)
	}
	path := filepath.Join(controllerDataDir, FileName)
	dsn := (&url.URL{Scheme: "file", Path: path, RawQuery: "_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)&_pragma=busy_timeout(5000)"}).String()
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, typed(farmerr.INTERNAL_ERROR, "cannot open farm database", err)
	}
	// A single connection gives deterministic write serialization for M4.1 CRUD.
	sqlDB.SetMaxOpenConns(1)
	db := &DB{sql: sqlDB, path: path}
	if err := sqlDB.PingContext(ctx); err != nil {
		sqlDB.Close()
		return nil, typed(farmerr.INTERNAL_ERROR, "cannot access farm database", err)
	}
	if err := os.Chmod(path, 0600); err != nil {
		sqlDB.Close()
		return nil, typed(farmerr.PERMISSION_DENIED, "cannot secure farm database", err)
	}
	if err := db.migrate(ctx); err != nil {
		sqlDB.Close()
		return nil, err
	}
	return db, nil
}

func (db *DB) SQL() *sql.DB { return db.sql }
func (db *DB) Path() string { return db.path }
func (db *DB) Close() error { return db.sql.Close() }

// ValidateSchema verifies the complete immutable migration ledger without
// applying migrations. Backup verification uses it on a read-only database.
func ValidateSchema(ctx context.Context, sqlDB *sql.DB) error {
	migrations, err := loadMigrations()
	if err != nil {
		return typed(farmerr.INTERNAL_ERROR, "cannot load database migrations", err)
	}
	rows, err := sqlDB.QueryContext(ctx, "SELECT version,name,checksum FROM schema_migrations ORDER BY version")
	if err != nil {
		return typed(farmerr.CONFIG_CONFLICT, "backup database has no valid migration ledger", err)
	}
	defer rows.Close()
	index := 0
	for rows.Next() {
		if index >= len(migrations) {
			return typed(farmerr.CONFIG_CONFLICT, "backup database schema is newer than this Controller", nil)
		}
		var version int
		var name, checksum string
		if err := rows.Scan(&version, &name, &checksum); err != nil {
			return typed(farmerr.CONFIG_CONFLICT, "backup migration ledger is malformed", err)
		}
		expected := migrations[index]
		if version != expected.version || name != expected.name || checksum != expected.hash {
			return typed(farmerr.CONFIG_CONFLICT, "backup migration ledger is incompatible", nil)
		}
		index++
	}
	if err := rows.Err(); err != nil {
		return typed(farmerr.CONFIG_CONFLICT, "backup migration ledger cannot be read", err)
	}
	if index != len(migrations) {
		return typed(farmerr.CONFIG_CONFLICT, "backup database schema is older than this Controller", nil)
	}
	return nil
}

func (db *DB) migrate(ctx context.Context) error {
	migrations, err := loadMigrations()
	if err != nil {
		return typed(farmerr.INTERNAL_ERROR, "cannot load database migrations", err)
	}
	if _, err := db.sql.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
version INTEGER PRIMARY KEY, name TEXT NOT NULL, checksum TEXT NOT NULL, applied_at_ns INTEGER NOT NULL)`); err != nil {
		return typed(farmerr.INTERNAL_ERROR, "cannot initialize migration ledger", err)
	}
	rows, err := db.sql.QueryContext(ctx, "SELECT version, name, checksum FROM schema_migrations ORDER BY version")
	if err != nil {
		return typed(farmerr.INTERNAL_ERROR, "cannot read migration ledger", err)
	}
	applied := make(map[int][2]string)
	for rows.Next() {
		var version int
		var name, checksum string
		if err := rows.Scan(&version, &name, &checksum); err != nil {
			rows.Close()
			return typed(farmerr.INTERNAL_ERROR, "invalid migration ledger", err)
		}
		applied[version] = [2]string{name, checksum}
	}
	if err := rows.Close(); err != nil {
		return typed(farmerr.INTERNAL_ERROR, "cannot close migration ledger", err)
	}
	for version := range applied {
		if version < 1 || version > len(migrations) {
			return typed(farmerr.CONFIG_CONFLICT, "farm database was created by a newer or incompatible Controller", nil)
		}
	}
	// The ledger must describe a contiguous prefix. Applying a missing older
	// migration after a recorded newer one would hide corruption and violate the
	// ordered, immutable migration contract.
	for version := 1; version <= len(applied); version++ {
		if _, ok := applied[version]; !ok {
			return typed(farmerr.CONFIG_CONFLICT, "farm database migration ledger is not contiguous", nil)
		}
	}
	for _, item := range migrations {
		if existing, ok := applied[item.version]; ok {
			if existing[0] != item.name || existing[1] != item.hash {
				return typed(farmerr.CONFIG_CONFLICT, "applied database migration checksum does not match this Controller", nil)
			}
			continue
		}
		tx, err := db.sql.BeginTx(ctx, nil)
		if err != nil {
			return typed(farmerr.INTERNAL_ERROR, "cannot begin database migration", err)
		}
		if _, err := tx.ExecContext(ctx, item.sql); err != nil {
			tx.Rollback()
			return typed(farmerr.INTERNAL_ERROR, "cannot apply database migration", err)
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO schema_migrations(version,name,checksum,applied_at_ns) VALUES(?,?,?,?)", item.version, item.name, item.hash, time.Now().UTC().UnixNano()); err != nil {
			tx.Rollback()
			return typed(farmerr.INTERNAL_ERROR, "cannot record database migration", err)
		}
		if err := tx.Commit(); err != nil {
			return typed(farmerr.INTERNAL_ERROR, "cannot commit database migration", err)
		}
	}
	return nil
}

func loadMigrations() ([]migration, error) {
	entries, err := migrationFiles.ReadDir("migrations")
	if err != nil {
		return nil, err
	}
	items := make([]migration, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		prefix, _, ok := strings.Cut(entry.Name(), "_")
		if !ok {
			return nil, fmt.Errorf("migration lacks numeric prefix: %s", entry.Name())
		}
		version, err := strconv.Atoi(prefix)
		if err != nil || version < 1 {
			return nil, fmt.Errorf("invalid migration version: %s", entry.Name())
		}
		data, err := migrationFiles.ReadFile("migrations/" + entry.Name())
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(data)
		items = append(items, migration{version: version, name: entry.Name(), sql: string(data), hash: hex.EncodeToString(sum[:])})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].version < items[j].version })
	for i, item := range items {
		if item.version != i+1 {
			return nil, errors.New("database migrations must be contiguous from version 1")
		}
	}
	return items, nil
}

func typed(code farmerr.Code, message string, cause error) error {
	details := map[string]string{}
	if cause != nil {
		details["cause"] = cause.Error()
	}
	return farmerr.Error{Code: code, HumanMessage: message, Details: details}
}
