//go:build linux

package controllerbackup

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/le0xdon/le0xfarm/internal/controllerdb"
	"github.com/le0xdon/le0xfarm/internal/controllerlock"
	"github.com/le0xdon/le0xfarm/internal/farmconfig"
	"github.com/le0xdon/le0xfarm/internal/farmerr"
	"github.com/le0xdon/le0xfarm/internal/farmmodel"
	"github.com/le0xdon/le0xfarm/internal/identity"
	"github.com/le0xdon/le0xfarm/internal/miners/xmrig"
	"github.com/le0xdon/le0xfarm/internal/packagecatalog"
)

var (
	testControllerID, _  = identity.ParseControllerID("controller_0123456789abcdef0123456789abcdef")
	testFarmID, _        = identity.ParseFarmID("farm_0123456789abcdef0123456789abcdef")
	testHostID, _        = identity.ParseHostID("host_0123456789abcdef0123456789abcdef")
	testTrustFingerprint = "SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
)

func TestConsistentBackupScopeVerificationAndChangeDetection(t *testing.T) {
	ctx := context.Background()
	manager, db, dataDir := newTestManager(t)
	insertHold(t, db, "old")
	if err := os.WriteFile(filepath.Join(dataDir, "controller-private-key.pem"), []byte("synthetic-secret"), 0600); err != nil {
		t.Fatal(err)
	}
	first, err := manager.CreateScheduled(ctx, db)
	if err != nil || !first.Created || first.Code != farmerr.BACKUP_CREATED {
		t.Fatalf("first scheduled backup=%+v err=%v", first, err)
	}
	manifest, err := manager.Verify(ctx, first.Manifest.BackupID)
	if err != nil || manifest.StateRevision == 0 {
		t.Fatalf("verify=%+v err=%v", manifest, err)
	}
	entries, err := os.ReadDir(filepath.Join(manager.backupDir, manifest.BackupID.String()))
	if err != nil || len(entries) != 2 {
		t.Fatalf("backup scope entries=%v err=%v", entries, err)
	}
	backupDB, err := openSQLite(filepath.Join(manager.backupDir, manifest.BackupID.String(), DatabaseFile), true)
	if err != nil {
		t.Fatal(err)
	}
	defer backupDB.Close()
	var reason string
	if err := backupDB.QueryRow("SELECT reason FROM maintenance_holds WHERE host_id=?", testHostID.String()).Scan(&reason); err != nil || reason != "old" {
		t.Fatalf("persistent Hold missing from backup reason=%q err=%v", reason, err)
	}
	second, err := manager.CreateScheduled(ctx, db)
	if err != nil || second.Code != farmerr.BACKUP_SKIPPED_UNCHANGED || second.Created {
		t.Fatalf("unchanged scheduled backup=%+v err=%v", second, err)
	}
	if _, err := db.SQL().ExecContext(ctx, "UPDATE maintenance_holds SET reason='changed',revision=revision+1,updated_at_ns=updated_at_ns+1 WHERE host_id=?", testHostID.String()); err != nil {
		t.Fatal(err)
	}
	third, err := manager.CreateScheduled(ctx, db)
	if err != nil || !third.Created || third.Manifest.StateRevision <= first.Manifest.StateRevision {
		t.Fatalf("changed scheduled backup=%+v err=%v", third, err)
	}
}

func TestIncompleteAndCorruptBackupsFailClosed(t *testing.T) {
	ctx := context.Background()
	manager, db, _ := newTestManager(t)
	manager.beforePublish = func() error { return errors.New("injected publication failure") }
	if _, err := manager.CreateManual(ctx, db); code(err) != farmerr.BACKUP_FAILED {
		t.Fatalf("incomplete create error=%v", err)
	}
	values, err := manager.List(ctx)
	if err != nil || len(values) != 0 {
		t.Fatalf("incomplete artifact was published: %+v err=%v", values, err)
	}
	manager.beforePublish = nil
	result, err := manager.CreateManual(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	databasePath := filepath.Join(manager.backupDir, result.Manifest.BackupID.String(), DatabaseFile)
	file, err := os.OpenFile(databasePath, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = file.WriteString("corrupt")
	_ = file.Close()
	if _, err := manager.Verify(ctx, result.Manifest.BackupID); code(err) != farmerr.BACKUP_CORRUPT {
		t.Fatalf("corrupt verification error=%v", err)
	}
}

func TestUnsupportedManifestSchemaFailsClosed(t *testing.T) {
	ctx := context.Background()
	manager, db, _ := newTestManager(t)
	result, err := manager.CreateManual(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	manifest := result.Manifest
	manifest.DatabaseSchema++
	path := filepath.Join(manager.backupDir, manifest.BackupID.String(), ManifestFile)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := writeManifest(path, manifest); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Verify(ctx, manifest.BackupID); code(err) != farmerr.BACKUP_CORRUPT {
		t.Fatalf("unsupported schema verification error=%v", err)
	}
}

func TestManifestProvenanceCannotBeSubstituted(t *testing.T) {
	ctx := context.Background()
	manager, db, _ := newTestManager(t)
	result, err := manager.CreateManual(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	manifest := result.Manifest
	manifest.CreatedAt = manifest.CreatedAt.Add(24 * time.Hour)
	path := filepath.Join(manager.backupDir, manifest.BackupID.String(), ManifestFile)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := writeManifest(path, manifest); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Verify(ctx, manifest.BackupID); code(err) != farmerr.BACKUP_CORRUPT {
		t.Fatalf("substituted manifest verification error=%v", err)
	}
}

func TestTrustProvenanceCannotBeSubstitutedOrReusedWithDifferentCA(t *testing.T) {
	ctx := context.Background()
	manager, db, dataDir := newTestManager(t)
	result, err := manager.CreateManual(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	manifest := result.Manifest
	manifest.TrustFingerprint = "SHA256:BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"
	path := filepath.Join(manager.backupDir, manifest.BackupID.String(), ManifestFile)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := writeManifest(path, manifest); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Verify(ctx, manifest.BackupID); code(err) != farmerr.BACKUP_CORRUPT {
		t.Fatalf("substituted trust provenance verification error=%v", err)
	}

	// Recreate a valid artifact, then prove public IDs alone cannot establish
	// restore continuity under a replaced CA.
	result, err = manager.CreateManual(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	lease, err := controllerlock.Acquire(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	wrongTrust, err := New(Config{DataDir: dataDir, ControllerID: testControllerID, FarmID: testFarmID, TrustFingerprint: "SHA256:BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wrongTrust.Restore(ctx, result.Manifest.BackupID, lease); code(err) != farmerr.RESTORE_IDENTITY_MISMATCH {
		t.Fatalf("same public IDs with different CA restored: %v", err)
	}
	if _, err := New(Config{DataDir: dataDir, ControllerID: testControllerID, FarmID: testFarmID}); code(err) != farmerr.CONFIG_CONFLICT {
		t.Fatalf("missing trust provenance accepted: %v", err)
	}
}

func TestManifestDuplicateSecurityFieldsAreRejected(t *testing.T) {
	ctx := context.Background()
	for _, field := range []struct {
		name  string
		value string
	}{
		{name: "backup_id", value: `"backup_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"`},
		{name: "controller_id", value: `"controller_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"`},
		{name: "farm_id", value: `"farm_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"`},
		{name: "trust_fingerprint", value: `"SHA256:BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"`},
		{name: "created_at", value: `"2026-09-10T12:00:00Z"`},
		{name: "class", value: `"MANUAL"`},
		{name: "database_sha256", value: `"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"`},
	} {
		t.Run(field.name, func(t *testing.T) {
			manager, db, _ := newTestManager(t)
			result, err := manager.CreateManual(ctx, db)
			if err != nil {
				t.Fatal(err)
			}
			data, err := json.Marshal(result.Manifest)
			if err != nil {
				t.Fatal(err)
			}
			data = append([]byte(`{"`+field.name+`":`+field.value+`,`), data[1:]...)
			path := filepath.Join(manager.backupDir, result.Manifest.BackupID.String(), ManifestFile)
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := manager.Verify(ctx, result.Manifest.BackupID); code(err) != farmerr.BACKUP_CORRUPT {
				t.Fatalf("duplicate %s accepted: %v", field.name, err)
			}
		})
	}
}

func TestVerificationRejectsSymlinkArtifact(t *testing.T) {
	ctx := context.Background()
	manager, db, dataDir := newTestManager(t)
	result, err := manager.CreateManual(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(manager.backupDir, result.Manifest.BackupID.String(), DatabaseFile)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dataDir, controllerdb.FileName), path); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Verify(ctx, result.Manifest.BackupID); code(err) != farmerr.BACKUP_CORRUPT {
		t.Fatalf("symlink artifact verification error=%v", err)
	}
}

func TestVerificationRejectsHardLinkedArtifact(t *testing.T) {
	ctx := context.Background()
	manager, db, dataDir := newTestManager(t)
	result, err := manager.CreateManual(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(manager.backupDir, result.Manifest.BackupID.String(), DatabaseFile)
	if err := os.Link(path, filepath.Join(dataDir, "extra-database-link")); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Verify(ctx, result.Manifest.BackupID); code(err) != farmerr.BACKUP_CORRUPT {
		t.Fatalf("hard-linked artifact verification error=%v", err)
	}
}

func TestScheduledBackupDoesNotOverlap(t *testing.T) {
	ctx := context.Background()
	manager, db, _ := newTestManager(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	manager.beforeSnapshot = func() { close(entered); <-release }
	done := make(chan error, 1)
	go func() { _, err := manager.CreateScheduled(ctx, db); done <- err }()
	<-entered
	if _, err := manager.CreateScheduled(ctx, db); code(err) != farmerr.BACKUP_BUSY {
		t.Fatalf("overlapping backup error=%v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestBackupSerializesWithConcurrentWriteAndCapturesCoherentCommit(t *testing.T) {
	ctx := context.Background()
	manager, db, _ := newTestManager(t)
	tx, err := db.SQL().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().UnixNano()
	if _, err := tx.ExecContext(ctx, "INSERT INTO maintenance_holds(host_id,active,revision,reason,created_at_ns,updated_at_ns) VALUES(?,?,?,?,?,?)", testHostID.String(), true, 1, "transactional", now, now); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	manager.beforeSnapshot = func() { close(entered); <-release }
	type outcome struct {
		result CreateResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := manager.CreateManual(ctx, db)
		done <- outcome{result: result, err: err}
	}()
	<-entered
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	close(release)
	created := <-done
	if created.err != nil {
		t.Fatal(created.err)
	}
	copyDB, err := openSQLite(filepath.Join(manager.backupDir, created.result.Manifest.BackupID.String(), DatabaseFile), true)
	if err != nil {
		t.Fatal(err)
	}
	defer copyDB.Close()
	var reason string
	if err := copyDB.QueryRowContext(ctx, "SELECT reason FROM maintenance_holds WHERE host_id=?", testHostID.String()).Scan(&reason); err != nil || reason != "transactional" {
		t.Fatalf("coherent transaction missing reason=%q err=%v", reason, err)
	}
}

func TestRestoreIsAtomicIdentityBoundAndInstallsDurableBarriers(t *testing.T) {
	ctx := context.Background()
	manager, db, dataDir := newTestManager(t)
	workloadID, executionID := insertDesiredRuntime(t, db)
	insertHold(t, db, "restored")
	insertIncident(t, db, "ACTIVE")
	backup, err := manager.CreateManual(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().ExecContext(ctx, "UPDATE maintenance_holds SET reason='newer',revision=revision+1,updated_at_ns=updated_at_ns+1 WHERE host_id=?", testHostID.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().ExecContext(ctx, "UPDATE incidents SET lifecycle_state='RESOLVED',resolved_at_ns=last_observed_at_ns+1 WHERE incident_id='incident_0123456789abcdef0123456789abcdef'"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	lease, err := controllerlock.Acquire(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	wrongID, _ := identity.ParseControllerID("controller_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	wrong, _ := New(Config{DataDir: dataDir, BackupDir: manager.backupDir, ControllerID: wrongID, FarmID: testFarmID, TrustFingerprint: testTrustFingerprint})
	if _, err := wrong.Restore(ctx, backup.Manifest.BackupID, lease); code(err) != farmerr.RESTORE_IDENTITY_MISMATCH {
		t.Fatalf("cross-identity restore error=%v", err)
	}
	result, err := manager.Restore(ctx, backup.Manifest.BackupID, lease)
	if err != nil || result.Code != farmerr.RESTORE_RECONCILIATION_REQUIRED || result.HostBarriers != 1 || result.SafetyBackupID == "" {
		t.Fatalf("restore=%+v err=%v", result, err)
	}
	reopened, err := controllerdb.Open(ctx, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	var reason string
	if err := reopened.SQL().QueryRowContext(ctx, "SELECT reason FROM maintenance_holds WHERE host_id=?", testHostID.String()).Scan(&reason); err != nil || reason != "restored" {
		t.Fatalf("restored Hold reason=%q err=%v", reason, err)
	}
	var required bool
	var status string
	if err := reopened.SQL().QueryRowContext(ctx, "SELECT restore_required FROM controller_backup_state WHERE singleton=1").Scan(&required); err != nil || !required {
		t.Fatalf("restore marker required=%t err=%v", required, err)
	}
	if err := reopened.SQL().QueryRowContext(ctx, "SELECT status FROM restore_host_barriers WHERE host_id=?", testHostID.String()).Scan(&status); err != nil || status != "PENDING" {
		t.Fatalf("restore barrier status=%q err=%v", status, err)
	}
	var desiredState, restoredExecution, incidentState string
	if err := reopened.SQL().QueryRowContext(ctx, "SELECT desired_run_state FROM desired_workloads WHERE workload_id=?", workloadID.String()).Scan(&desiredState); err != nil || desiredState != "RUNNING" {
		t.Fatalf("restored Desired state=%q err=%v", desiredState, err)
	}
	if err := reopened.SQL().QueryRowContext(ctx, "SELECT execution_id FROM resolved_execution_snapshots WHERE workload_id=? AND desired_generation=1", workloadID.String()).Scan(&restoredExecution); err != nil || restoredExecution != executionID.String() {
		t.Fatalf("restored snapshot execution=%q err=%v", restoredExecution, err)
	}
	if err := reopened.SQL().QueryRowContext(ctx, "SELECT lifecycle_state FROM incidents WHERE incident_id='incident_0123456789abcdef0123456789abcdef'").Scan(&incidentState); err != nil || incidentState != "ACTIVE" {
		t.Fatalf("restored incident state=%q err=%v", incidentState, err)
	}
}

func TestFailedRestoreLeavesLiveStateIntact(t *testing.T) {
	ctx := context.Background()
	manager, db, dataDir := newTestManager(t)
	insertHold(t, db, "old")
	backup, err := manager.CreateManual(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().ExecContext(ctx, "UPDATE maintenance_holds SET reason='current',revision=revision+1,updated_at_ns=updated_at_ns+1 WHERE host_id=?", testHostID.String()); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	lease, err := controllerlock.Acquire(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	manager.beforeInstall = func() error { return errors.New("injected pre-install failure") }
	if _, err := manager.Restore(ctx, backup.Manifest.BackupID, lease); code(err) != farmerr.RESTORE_VALIDATION_FAILED {
		t.Fatalf("failed restore error=%v", err)
	}
	reopened, err := controllerdb.Open(ctx, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	var reason string
	if err := reopened.SQL().QueryRowContext(ctx, "SELECT reason FROM maintenance_holds WHERE host_id=?", testHostID.String()).Scan(&reason); err != nil || reason != "current" {
		t.Fatalf("failed restore changed live state reason=%q err=%v", reason, err)
	}
}

func TestRestoreRejectsCandidatePathReplacement(t *testing.T) {
	for _, test := range []struct {
		name    string
		replace func(string, string) error
	}{
		{name: "symlink", replace: func(candidate, dataDir string) error {
			if err := os.Remove(candidate); err != nil {
				return err
			}
			return os.Symlink(filepath.Join(dataDir, controllerdb.FileName), candidate)
		}},
		{name: "regular file", replace: func(candidate, _ string) error {
			if err := os.Remove(candidate); err != nil {
				return err
			}
			return os.WriteFile(candidate, []byte("substituted"), 0600)
		}},
		{name: "hard link", replace: func(candidate, dataDir string) error {
			target := filepath.Join(dataDir, "candidate-hardlink-target")
			if err := os.WriteFile(target, []byte("substituted"), 0600); err != nil {
				return err
			}
			if err := os.Remove(candidate); err != nil {
				return err
			}
			return os.Link(target, candidate)
		}},
		{name: "delete and recreate", replace: func(candidate, _ string) error {
			if err := os.Remove(candidate); err != nil {
				return err
			}
			file, err := os.OpenFile(candidate, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
			if err != nil {
				return err
			}
			_, writeErr := file.WriteString("recreated")
			return errors.Join(writeErr, file.Close())
		}},
		{name: "barrier removal in place", replace: func(candidate, _ string) error {
			db, err := openSQLite(candidate, false)
			if err != nil {
				return err
			}
			_, deleteErr := db.Exec(`DELETE FROM restore_host_barriers`)
			_, markerErr := db.Exec(`UPDATE controller_backup_state SET restore_required=0`)
			return errors.Join(deleteErr, markerErr, db.Close())
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			manager, db, dataDir := newTestManager(t)
			insertHold(t, db, "backup")
			backup, err := manager.CreateManual(ctx, db)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.SQL().ExecContext(ctx, "UPDATE maintenance_holds SET reason='live',revision=revision+1,updated_at_ns=updated_at_ns+1 WHERE host_id=?", testHostID.String()); err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			lease, err := controllerlock.Acquire(dataDir)
			if err != nil {
				t.Fatal(err)
			}
			defer lease.Close()
			manager.beforeInstall = func() error {
				matches, err := filepath.Glob(filepath.Join(dataDir, ".restore-candidate-*.db"))
				if err != nil || len(matches) != 1 {
					return errors.New("restore candidate not found")
				}
				return test.replace(matches[0], dataDir)
			}
			if _, err := manager.Restore(ctx, backup.Manifest.BackupID, lease); code(err) != farmerr.RESTORE_VALIDATION_FAILED {
				t.Fatalf("candidate %s replacement restore error=%v", test.name, err)
			}
			reopened, err := controllerdb.Open(ctx, dataDir)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			var reason string
			if err := reopened.SQL().QueryRowContext(ctx, "SELECT reason FROM maintenance_holds WHERE host_id=?", testHostID.String()).Scan(&reason); err != nil || reason != "live" {
				t.Fatalf("candidate substitution changed live state reason=%q err=%v", reason, err)
			}
		})
	}
}

func TestRestoreExactInstallBoundaryRejectsRacedDestinationAndRecoversOriginal(t *testing.T) {
	for _, test := range []struct {
		name   string
		attack func(string) error
	}{
		{name: "symlink", attack: func(path string) error { return os.Symlink(filepath.Join(filepath.Dir(path), "outside-target"), path) }},
		{name: "different regular file", attack: func(path string) error { return os.WriteFile(path, []byte("unbarriered"), 0600) }},
		{name: "hard link", attack: func(path string) error {
			target := filepath.Join(filepath.Dir(path), "outside-hardlink")
			if err := os.WriteFile(target, []byte("unbarriered"), 0600); err != nil {
				return err
			}
			return os.Link(target, path)
		}},
		{name: "delete recreate", attack: func(path string) error {
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			return os.WriteFile(path, []byte("recreated"), 0600)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			manager, db, dataDir := newTestManager(t)
			insertHold(t, db, "backup")
			backup, err := manager.CreateManual(ctx, db)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.SQL().ExecContext(ctx, "UPDATE maintenance_holds SET reason='live',revision=revision+1,updated_at_ns=updated_at_ns+1 WHERE host_id=?", testHostID.String()); err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			lease, err := controllerlock.Acquire(dataDir)
			if err != nil {
				t.Fatal(err)
			}
			defer lease.Close()
			live := filepath.Join(dataDir, controllerdb.FileName)
			manager.beforeInstallLink = func() error {
				if _, err := os.Lstat(filepath.Join(dataDir, restoreOldFile)); err != nil {
					return errors.New("hook did not reach the final linkat interval")
				}
				return test.attack(live)
			}
			if _, err := manager.Restore(ctx, backup.Manifest.BackupID, lease); code(err) != farmerr.RESTORE_VALIDATION_FAILED {
				t.Fatalf("raced destination restore error=%v", err)
			}
			manager.beforeInstallLink = nil
			if err := os.Remove(live); err != nil {
				t.Fatal(err)
			}
			if err := RecoverInterruptedRestore(ctx, dataDir, lease, testControllerID, testFarmID, testTrustFingerprint); err != nil {
				t.Fatalf("original database was not deterministically recoverable: %v", err)
			}
			assertHoldReason(t, dataDir, "live")
		})
	}
}

func TestRestoreIntoEmptyDataDirKeepsFailClosedMarkerOnRacedDestination(t *testing.T) {
	ctx := context.Background()
	manager, db, dataDir := newTestManager(t)
	backup, err := manager.CreateManual(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dataDir, controllerdb.FileName)); err != nil {
		t.Fatal(err)
	}
	lease, err := controllerlock.Acquire(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	manager.beforeInstallLink = func() error {
		return os.WriteFile(filepath.Join(dataDir, controllerdb.FileName), []byte("unbarriered"), 0600)
	}
	if _, err := manager.Restore(ctx, backup.Manifest.BackupID, lease); code(err) != farmerr.RESTORE_VALIDATION_FAILED {
		t.Fatalf("raced empty restore error=%v", err)
	}
	manager.beforeInstallLink = nil
	if err := RecoverInterruptedRestore(ctx, dataDir, lease, testControllerID, testFarmID, testTrustFingerprint); code(err) != farmerr.RESTORE_VALIDATION_FAILED {
		t.Fatalf("unexpected empty restore destination was not fail-closed: %v", err)
	}
	if err := os.Remove(filepath.Join(dataDir, controllerdb.FileName)); err != nil {
		t.Fatal(err)
	}
	if err := RecoverInterruptedRestore(ctx, dataDir, lease, testControllerID, testFarmID, testTrustFingerprint); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(dataDir, restoreMarkerFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("empty aborted restore marker still exists: %v", err)
	}
}

func TestRestoreValidationNameSubstitutionFailsClosed(t *testing.T) {
	ctx := context.Background()
	manager, db, dataDir := newTestManager(t)
	insertHold(t, db, "backup")
	backup, err := manager.CreateManual(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().ExecContext(ctx, "UPDATE maintenance_holds SET reason='live',revision=revision+1,updated_at_ns=updated_at_ns+1 WHERE host_id=?", testHostID.String()); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	lease, err := controllerlock.Acquire(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	manager.beforeInstallLink = func() error {
		matches, err := filepath.Glob(filepath.Join(dataDir, ".restore-exact-*"))
		if err != nil || len(matches) != 1 {
			return errors.New("descriptor-bound validation name not found")
		}
		if err := os.Remove(matches[0]); err != nil {
			return err
		}
		return os.Symlink(filepath.Join(dataDir, "attacker-database"), matches[0])
	}
	_, restoreErr := manager.Restore(ctx, backup.Manifest.BackupID, lease)
	if restoreErr != nil && code(restoreErr) != farmerr.RESTORE_VALIDATION_FAILED {
		t.Fatalf("unexpected restore error=%v", restoreErr)
	}
	manager.beforeInstallLink = nil
	if restoreErr != nil {
		if err := RecoverInterruptedRestore(ctx, dataDir, lease, testControllerID, testFarmID, testTrustFingerprint); err != nil {
			t.Fatal(err)
		}
	}
	assertHoldReason(t, dataDir, "live")
}

func TestInterruptedExactInstallRecoversOnlyBarrieredDatabase(t *testing.T) {
	ctx := context.Background()
	manager, db, dataDir := newTestManager(t)
	insertHold(t, db, "backup")
	backup, err := manager.CreateManual(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().ExecContext(ctx, "UPDATE maintenance_holds SET reason='live',revision=revision+1,updated_at_ns=updated_at_ns+1 WHERE host_id=?", testHostID.String()); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	lease, err := controllerlock.Acquire(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	manager.afterInstallLink = func() error { return errors.New("simulated crash after exact linkat") }
	if _, err := manager.Restore(ctx, backup.Manifest.BackupID, lease); code(err) != farmerr.RESTORE_VALIDATION_FAILED {
		t.Fatalf("interrupted restore error=%v", err)
	}
	manager.afterInstallLink = nil
	if err := RecoverInterruptedRestore(ctx, dataDir, lease, testControllerID, testFarmID, testTrustFingerprint); err != nil {
		t.Fatal(err)
	}
	assertHoldReason(t, dataDir, "backup")
}

func TestRestoredRecoveryMakesAuthoritativeNameDurableBeforeCleanup(t *testing.T) {
	t.Run("failure before authoritative directory sync preserves all evidence", func(t *testing.T) {
		manager, dataDir, lease := interruptedRestoreWithInstalledRestored(t)
		directory, err := lease.OpenDirectory(dataDir)
		if err != nil {
			t.Fatal(err)
		}
		transaction, err := readRestoreTransactionMarker(directory)
		if err != nil {
			directory.Close()
			t.Fatal(err)
		}
		hookReached := false
		err = recoverRestoreTransaction(context.Background(), directory, transaction, manager.controllerID, manager.farmID, manager.trustFingerprint, restoreRecoveryHooks{
			beforeRestoredDirectorySync: func() error {
				hookReached = true
				return errors.New("simulated crash before authoritative directory sync")
			},
		})
		if closeErr := directory.Close(); err == nil || closeErr != nil || !hookReached {
			t.Fatalf("pre-sync interruption err=%v close=%v hook=%t", err, closeErr, hookReached)
		}
		assertRecoveryEntryExists(t, dataDir, restoreMarkerFile)
		assertRecoveryEntryExists(t, dataDir, restoreOldFile)
		if err := RecoverInterruptedRestore(context.Background(), dataDir, lease, manager.controllerID, manager.farmID, manager.trustFingerprint); err != nil {
			t.Fatal(err)
		}
		assertHoldReason(t, dataDir, "backup")
	})

	t.Run("failure after authoritative sync preserves recovery evidence", func(t *testing.T) {
		manager, dataDir, lease := interruptedRestoreWithInstalledRestored(t)
		directory, err := lease.OpenDirectory(dataDir)
		if err != nil {
			t.Fatal(err)
		}
		transaction, err := readRestoreTransactionMarker(directory)
		if err != nil {
			directory.Close()
			t.Fatal(err)
		}
		hookReached := false
		err = recoverRestoreTransaction(context.Background(), directory, transaction, manager.controllerID, manager.farmID, manager.trustFingerprint, restoreRecoveryHooks{
			afterRestoredDirectorySync: func() error {
				hookReached = true
				return errors.New("simulated crash after authoritative directory sync")
			},
		})
		if closeErr := directory.Close(); err == nil || closeErr != nil || !hookReached {
			t.Fatalf("post-sync interruption err=%v close=%v hook=%t", err, closeErr, hookReached)
		}
		assertRecoveryEntryExists(t, dataDir, restoreMarkerFile)
		assertRecoveryEntryExists(t, dataDir, restoreOldFile)
		if err := RecoverInterruptedRestore(context.Background(), dataDir, lease, manager.controllerID, manager.farmID, manager.trustFingerprint); err != nil {
			t.Fatal(err)
		}
		assertHoldReason(t, dataDir, "backup")
	})

	t.Run("cleanup interruption remains safe and idempotent", func(t *testing.T) {
		manager, dataDir, lease := interruptedRestoreWithInstalledRestored(t)
		directory, err := lease.OpenDirectory(dataDir)
		if err != nil {
			t.Fatal(err)
		}
		transaction, err := readRestoreTransactionMarker(directory)
		if err != nil {
			directory.Close()
			t.Fatal(err)
		}
		validationName := restoreValidationName(transaction)
		if err := os.Link(filepath.Join(dataDir, controllerdb.FileName), filepath.Join(dataDir, validationName)); err != nil {
			directory.Close()
			t.Fatal(err)
		}
		authoritativeSynced := false
		cleanupHookReached := false
		err = recoverRestoreTransaction(context.Background(), directory, transaction, manager.controllerID, manager.farmID, manager.trustFingerprint, restoreRecoveryHooks{
			afterRestoredDirectorySync: func() error {
				authoritativeSynced = true
				return nil
			},
			afterRestoredValidationCleanup: func() error {
				cleanupHookReached = true
				if !authoritativeSynced {
					return errors.New("cleanup began before authoritative directory sync")
				}
				return errors.New("simulated crash during recovery cleanup")
			},
		})
		if closeErr := directory.Close(); err == nil || closeErr != nil || !cleanupHookReached || !authoritativeSynced {
			t.Fatalf("cleanup interruption err=%v close=%v synced=%t cleanup=%t", err, closeErr, authoritativeSynced, cleanupHookReached)
		}
		assertRecoveryEntryExists(t, dataDir, restoreMarkerFile)
		assertRecoveryEntryExists(t, dataDir, restoreOldFile)
		if _, err := os.Lstat(filepath.Join(dataDir, validationName)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("validation entry was not the first cleanup step: %v", err)
		}
		for attempt := 0; attempt < 2; attempt++ {
			if err := RecoverInterruptedRestore(context.Background(), dataDir, lease, manager.controllerID, manager.farmID, manager.trustFingerprint); err != nil {
				t.Fatalf("recovery attempt %d: %v", attempt+1, err)
			}
		}
		assertHoldReason(t, dataDir, "backup")
		if _, err := os.Lstat(filepath.Join(dataDir, restoreMarkerFile)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("completed recovery marker remains: %v", err)
		}
	})
}

func TestRecoveryRejectsInstalledDatabaseWhoseBarriersChanged(t *testing.T) {
	ctx := context.Background()
	manager, db, dataDir := newTestManager(t)
	insertHold(t, db, "backup")
	backup, err := manager.CreateManual(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().ExecContext(ctx, "UPDATE maintenance_holds SET reason='live',revision=revision+1,updated_at_ns=updated_at_ns+1 WHERE host_id=?", testHostID.String()); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	lease, err := controllerlock.Acquire(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	manager.afterInstallLink = func() error {
		installed, err := openSQLite(filepath.Join(dataDir, controllerdb.FileName), false)
		if err != nil {
			return err
		}
		_, deleteErr := installed.Exec("DELETE FROM restore_host_barriers")
		_, markerErr := installed.Exec("UPDATE controller_backup_state SET restore_required=0")
		return errors.Join(deleteErr, markerErr, installed.Close())
	}
	if _, err := manager.Restore(ctx, backup.Manifest.BackupID, lease); code(err) != farmerr.RESTORE_VALIDATION_FAILED {
		t.Fatalf("tampered exact install error=%v", err)
	}
	manager.afterInstallLink = nil
	if err := RecoverInterruptedRestore(ctx, dataDir, lease, testControllerID, testFarmID, testTrustFingerprint); code(err) != farmerr.RESTORE_VALIDATION_FAILED {
		t.Fatalf("tampered installed database was accepted by recovery: %v", err)
	}
	if err := os.Remove(filepath.Join(dataDir, controllerdb.FileName)); err != nil {
		t.Fatal(err)
	}
	if err := RecoverInterruptedRestore(ctx, dataDir, lease, testControllerID, testFarmID, testTrustFingerprint); err != nil {
		t.Fatal(err)
	}
	assertHoldReason(t, dataDir, "live")
}

func TestInterruptedRestoreRecoveryRequiresCurrentTrustProvenance(t *testing.T) {
	ctx := context.Background()
	manager, db, dataDir := newTestManager(t)
	insertHold(t, db, "backup")
	backup, err := manager.CreateManual(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	lease, err := controllerlock.Acquire(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	manager.afterInstallLink = func() error { return errors.New("simulated crash") }
	if _, err := manager.Restore(ctx, backup.Manifest.BackupID, lease); code(err) != farmerr.RESTORE_VALIDATION_FAILED {
		t.Fatalf("interrupted restore error=%v", err)
	}
	wrongTrust := "SHA256:BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"
	if err := RecoverInterruptedRestore(ctx, dataDir, lease, testControllerID, testFarmID, wrongTrust); code(err) != farmerr.RESTORE_IDENTITY_MISMATCH {
		t.Fatalf("wrong-trust recovery accepted: %v", err)
	}
	if err := RecoverInterruptedRestore(ctx, dataDir, lease, testControllerID, testFarmID, testTrustFingerprint); err != nil {
		t.Fatal(err)
	}
	assertHoldReason(t, dataDir, "backup")
}

func TestRestoreOldSubstitutionFailsClosed(t *testing.T) {
	for _, test := range []struct {
		name   string
		attack func(*testing.T, string, []byte)
	}{
		{name: "symlink", attack: func(t *testing.T, path string, _ []byte) {
			target := filepath.Join(t.TempDir(), "unknown.db")
			if err := os.WriteFile(target, []byte("untouched"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, path); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "different regular database", attack: func(t *testing.T, path string, _ []byte) {
			otherDir := filepath.Join(t.TempDir(), "other")
			other, err := controllerdb.Open(context.Background(), otherDir)
			if err != nil {
				t.Fatal(err)
			}
			insertHold(t, other, "attacker")
			if err := other.Close(); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(filepath.Join(otherDir, controllerdb.FileName))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "hard link", attack: func(t *testing.T, path string, original []byte) {
			target := filepath.Join(filepath.Dir(path), "attacker-hardlink.db")
			if err := os.WriteFile(target, original, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Link(target, path); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "delete recreate", attack: func(t *testing.T, path string, original []byte) {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, original, 0600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "modified in place", attack: func(t *testing.T, path string, _ []byte) {
			file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
			if err != nil {
				t.Fatal(err)
			}
			_, writeErr := file.Write([]byte("changed"))
			if err := errors.Join(writeErr, file.Close()); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			manager, dataDir, lease, original := interruptedRestoreWithRetainedOld(t)
			oldPath := filepath.Join(dataDir, restoreOldFile)
			test.attack(t, oldPath, original)
			if err := RecoverInterruptedRestore(context.Background(), dataDir, lease, manager.controllerID, manager.farmID, manager.trustFingerprint); code(err) != farmerr.RESTORE_VALIDATION_FAILED {
				t.Fatalf("substituted restore-old was accepted: %v", err)
			}
			if _, err := os.Lstat(filepath.Join(dataDir, controllerdb.FileName)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("unknown restore-old was promoted: %v", err)
			}
			if _, err := os.Lstat(filepath.Join(dataDir, restoreMarkerFile)); err != nil {
				t.Fatalf("fail-closed recovery discarded transaction evidence: %v", err)
			}
		})
	}
}

func TestRestoreOldReplacementAtExactPromotionBoundaryCannotSubstituteSource(t *testing.T) {
	manager, dataDir, lease, _ := interruptedRestoreWithRetainedOld(t)
	directory, err := lease.OpenDirectory(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	transaction, err := readRestoreTransactionMarker(directory)
	if err != nil {
		t.Fatal(err)
	}
	hookReached := false
	err = recoverRestoreTransaction(context.Background(), directory, transaction, manager.controllerID, manager.farmID, manager.trustFingerprint, restoreRecoveryHooks{beforeOldPromotion: func() error {
		hookReached = true
		oldPath := filepath.Join(dataDir, restoreOldFile)
		preserved := filepath.Join(dataDir, "attacker-moved-old.db")
		if err := os.Rename(oldPath, preserved); err != nil {
			return err
		}
		return os.Symlink(filepath.Join(dataDir, "unknown-target.db"), oldPath)
	}})
	if err == nil || !hookReached {
		t.Fatalf("final old-object promotion race was not forced: %v", err)
	}
	// The exact descriptor-bound original may win the authoritative link, but
	// the substituted cleanup entry keeps the transaction fail-closed.
	assertHoldReason(t, dataDir, "live")
	if _, err := os.Lstat(filepath.Join(dataDir, restoreMarkerFile)); err != nil {
		t.Fatalf("ambiguous cleanup discarded transaction evidence: %v", err)
	}
}

func TestInProcessRollbackPromotesOnlyPreviouslyValidatedOldDescriptor(t *testing.T) {
	ctx := context.Background()
	manager, db, dataDir := newTestManager(t)
	insertHold(t, db, "backup")
	backup, err := manager.CreateManual(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().ExecContext(ctx, "UPDATE maintenance_holds SET reason='live',revision=revision+1,updated_at_ns=updated_at_ns+1 WHERE host_id=?", testHostID.String()); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	lease, err := controllerlock.Acquire(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	hookReached := false
	manager.beforeInstallLink = func() error { return errors.New("force in-process rollback") }
	manager.beforeOldPromotion = func() error {
		hookReached = true
		oldPath := filepath.Join(dataDir, restoreOldFile)
		preserved := filepath.Join(dataDir, "attacker-moved-old.db")
		if err := os.Rename(oldPath, preserved); err != nil {
			return err
		}
		return os.Symlink(filepath.Join(dataDir, "unknown-target.db"), oldPath)
	}
	if _, err := manager.Restore(ctx, backup.Manifest.BackupID, lease); code(err) != farmerr.RESTORE_VALIDATION_FAILED {
		t.Fatalf("in-process rollback race error=%v", err)
	}
	if !hookReached {
		t.Fatal("in-process rollback did not reach the exact old-object promotion boundary")
	}
	assertHoldReason(t, dataDir, "live")
	if _, err := os.Lstat(filepath.Join(dataDir, restoreMarkerFile)); err != nil {
		t.Fatalf("ambiguous rollback discarded transaction evidence: %v", err)
	}
}

func TestValidRestoreOldRecoveryIsIdempotent(t *testing.T) {
	manager, dataDir, lease, _ := interruptedRestoreWithRetainedOld(t)
	for attempt := 0; attempt < 2; attempt++ {
		if err := RecoverInterruptedRestore(context.Background(), dataDir, lease, manager.controllerID, manager.farmID, manager.trustFingerprint); err != nil {
			t.Fatalf("recovery attempt %d: %v", attempt+1, err)
		}
	}
	assertHoldReason(t, dataDir, "live")
	if _, err := os.Lstat(filepath.Join(dataDir, restoreMarkerFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("completed transaction marker remains: %v", err)
	}
}

func TestRestoreRecoveryRejectsWhenNeitherDatabaseMatches(t *testing.T) {
	manager, dataDir, lease, _ := interruptedRestoreWithRetainedOld(t)
	if err := os.Remove(filepath.Join(dataDir, restoreOldFile)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, controllerdb.FileName), []byte("unknown"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := RecoverInterruptedRestore(context.Background(), dataDir, lease, manager.controllerID, manager.farmID, manager.trustFingerprint); code(err) != farmerr.RESTORE_VALIDATION_FAILED {
		t.Fatalf("ambiguous transaction was accepted: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dataDir, restoreMarkerFile)); err != nil {
		t.Fatalf("ambiguous transaction evidence was removed: %v", err)
	}
}

func TestPublishedArtifactIsVerifiedBeforeAutomaticWatermark(t *testing.T) {
	ctx := context.Background()
	manager, db, _ := newTestManager(t)
	manager.afterPublish = func() error {
		entries, err := os.ReadDir(manager.backupDir)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if _, err := ParseBackupID(entry.Name()); err != nil {
				continue
			}
			published := filepath.Join(manager.backupDir, entry.Name())
			moved := filepath.Join(manager.backupDir, "attacker-moved-artifact")
			if err := os.Rename(published, moved); err != nil {
				return err
			}
			return os.Symlink(moved, published)
		}
		return errors.New("published interval was not reached")
	}
	if _, err := manager.CreateScheduled(ctx, db); code(err) != farmerr.BACKUP_FAILED {
		t.Fatalf("substituted publication error=%v", err)
	}
	var revision, watermark uint64
	if err := db.SQL().QueryRowContext(ctx, "SELECT persistent_revision,last_automatic_revision FROM controller_backup_state WHERE singleton=1").Scan(&revision, &watermark); err != nil {
		t.Fatal(err)
	}
	if revision == 0 || watermark != 0 {
		t.Fatalf("unproven publication advanced watermark revision=%d watermark=%d", revision, watermark)
	}
	manager.afterPublish = nil
	if result, err := manager.CreateScheduled(ctx, db); err != nil || !result.Created {
		t.Fatalf("failed publication was not retried result=%+v err=%v", result, err)
	}
}

func TestLeaseBoundBackupRejectsReplacedDataDirectory(t *testing.T) {
	ctx := context.Background()
	base, db, dataDir := newTestManager(t)
	lease, err := controllerlock.Acquire(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	manager, err := New(Config{DataDir: dataDir, ControllerID: testControllerID, FarmID: testFarmID, TrustFingerprint: testTrustFingerprint, Clock: base.clock, Lease: lease})
	if err != nil {
		t.Fatal(err)
	}
	renamed := dataDir + "-renamed"
	if err := os.Rename(dataDir, renamed); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dataDir, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.CreateManual(ctx, db); code(err) != farmerr.SERVICE_NOT_READY {
		t.Fatalf("lease-bound backup accepted replacement DataDir: %v", err)
	}
	if second, err := controllerlock.Acquire(dataDir); code(err) != farmerr.SERVICE_NOT_READY || second != nil {
		if second != nil {
			_ = second.Close()
		}
		t.Fatalf("replacement DataDir obtained second authority err=%v lease=%v", err, second)
	}
}

func TestPruneDirectoryReplacementCannotRedirectDeletion(t *testing.T) {
	ctx := context.Background()
	manager, db, _ := newTestManager(t)
	base := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	for index := 0; index < defaultHourlyRetention+1; index++ {
		when := base.Add(-time.Duration(index) * time.Minute)
		manager.clock = func() time.Time { return when }
		if _, err := manager.create(ctx, db, ClassHourly); err != nil {
			t.Fatal(err)
		}
	}
	values, err := manager.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	retained := Retained(values, defaultHourlyRetention, defaultDailyRetention)
	var victim BackupID
	for _, item := range values {
		if !retained[item.BackupID] {
			victim = item.BackupID
			break
		}
	}
	if victim == "" {
		t.Fatal("retention victim not found")
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.Mkdir(outside, 0700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(outside, "sentinel")
	if err := os.WriteFile(sentinel, []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(manager.backupDir, "moved-validated-artifact")
	manager.beforePruneDelete = func(id BackupID) error {
		if id != victim {
			return nil
		}
		path := filepath.Join(manager.backupDir, id.String())
		if err := os.Rename(path, moved); err != nil {
			return err
		}
		return os.Symlink(outside, path)
	}
	if err := manager.Prune(ctx); code(err) != farmerr.BACKUP_FAILED {
		t.Fatalf("replaced prune directory error=%v", err)
	}
	if data, err := os.ReadFile(sentinel); err != nil || string(data) != "preserve" {
		t.Fatalf("prune escaped exact artifact data=%q err=%v", data, err)
	}
	if _, err := os.Stat(filepath.Join(moved, DatabaseFile)); err != nil {
		t.Fatalf("opened validated artifact was destructively redirected: %v", err)
	}
}

func TestPruneDifferentDirectoryReplacementCannotDeleteValidatedObject(t *testing.T) {
	ctx := context.Background()
	manager, _, victim := newPruneVictim(t)
	moved := filepath.Join(manager.backupDir, "moved-validated-artifact")
	replacement := filepath.Join(manager.backupDir, victim.String())
	manager.beforePruneDelete = func(id BackupID) error {
		if id != victim {
			return nil
		}
		if err := os.Rename(replacement, moved); err != nil {
			return err
		}
		if err := os.Mkdir(replacement, 0700); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(replacement, "attacker-sentinel"), []byte("preserve"), 0600)
	}
	if err := manager.Prune(ctx); code(err) != farmerr.BACKUP_FAILED {
		t.Fatalf("different-directory prune error=%v", err)
	}
	if _, err := os.Stat(filepath.Join(moved, DatabaseFile)); err != nil {
		t.Fatalf("validated directory content was deleted: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(replacement, "attacker-sentinel")); err != nil || string(data) != "preserve" {
		t.Fatalf("replacement directory was deleted data=%q err=%v", data, err)
	}
}

func TestPruneChildReplacementNeverFollowsSymlink(t *testing.T) {
	ctx := context.Background()
	manager, _, victim := newPruneVictim(t)
	outside := filepath.Join(t.TempDir(), "outside-database")
	if err := os.WriteFile(outside, []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	manager.beforePruneDelete = func(id BackupID) error {
		if id != victim {
			return nil
		}
		child := filepath.Join(manager.backupDir, id.String(), DatabaseFile)
		if err := os.Remove(child); err != nil {
			return err
		}
		return os.Symlink(outside, child)
	}
	if err := manager.Prune(ctx); code(err) != farmerr.BACKUP_FAILED {
		t.Fatalf("child-symlink prune error=%v", err)
	}
	if data, err := os.ReadFile(outside); err != nil || string(data) != "preserve" {
		t.Fatalf("prune followed replaced child data=%q err=%v", data, err)
	}
}

func newPruneVictim(t *testing.T) (*Manager, *controllerdb.DB, BackupID) {
	t.Helper()
	ctx := context.Background()
	manager, db, _ := newTestManager(t)
	base := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	for index := 0; index < defaultHourlyRetention+1; index++ {
		when := base.Add(-time.Duration(index) * time.Minute)
		manager.clock = func() time.Time { return when }
		if _, err := manager.create(ctx, db, ClassHourly); err != nil {
			t.Fatal(err)
		}
	}
	values, err := manager.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	retained := Retained(values, defaultHourlyRetention, defaultDailyRetention)
	for _, item := range values {
		if !retained[item.BackupID] {
			return manager, db, item.BackupID
		}
	}
	t.Fatal("retention victim not found")
	return nil, nil, ""
}

func TestScheduledRetentionFailureIsDurablyRetriedWithoutDuplicateBackup(t *testing.T) {
	ctx := context.Background()
	manager, db, dataDir := newTestManager(t)
	manager.beforePrune = func() error { return errors.New("injected prune failure") }
	if _, err := manager.CreateScheduled(ctx, db); code(err) != farmerr.BACKUP_FAILED {
		t.Fatalf("scheduled prune failure=%v", err)
	}
	values, err := manager.List(ctx)
	if err != nil || len(values) != 1 {
		t.Fatalf("successful backup was not retained after prune failure values=%d err=%v", len(values), err)
	}
	var pending bool
	if err := db.SQL().QueryRowContext(ctx, "SELECT retention_pending FROM controller_backup_state WHERE singleton=1").Scan(&pending); err != nil || !pending {
		t.Fatalf("retention debt pending=%t err=%v", pending, err)
	}
	restarted, err := New(Config{DataDir: dataDir, ControllerID: testControllerID, FarmID: testFarmID, TrustFingerprint: testTrustFingerprint, Clock: manager.clock})
	if err != nil {
		t.Fatal(err)
	}
	result, err := restarted.CreateScheduled(ctx, db)
	if err != nil || result.Code != farmerr.BACKUP_SKIPPED_UNCHANGED {
		t.Fatalf("retention retry result=%+v err=%v", result, err)
	}
	values, err = restarted.List(ctx)
	if err != nil || len(values) != 1 {
		t.Fatalf("retention retry created a duplicate backup values=%d err=%v", len(values), err)
	}
	if err := db.SQL().QueryRowContext(ctx, "SELECT retention_pending FROM controller_backup_state WHERE singleton=1").Scan(&pending); err != nil || pending {
		t.Fatalf("retention debt not cleared pending=%t err=%v", pending, err)
	}
}

func TestSchedulerRetriesFailureAndStopsOnCancellation(t *testing.T) {
	manager, db, _ := newTestManager(t)
	manager.beforePublish = func() error { return errors.New("injected backup failure") }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	codes := make(chan farmerr.Code)
	proceed := make(chan struct{})
	go func() {
		manager.Run(ctx, db, 2*time.Millisecond, func(code farmerr.Code) {
			codes <- code
			select {
			case <-proceed:
			case <-ctx.Done():
			}
		})
		close(done)
	}()
	if code := <-codes; code != farmerr.BACKUP_FAILED {
		t.Fatalf("initial scheduler code=%s", code)
	}
	manager.mu.Lock()
	manager.beforePublish = nil
	manager.mu.Unlock()
	proceed <- struct{}{}
	if code := <-codes; code != farmerr.BACKUP_CREATED {
		t.Fatalf("scheduler retry code=%s", code)
	}
	insertHold(t, db, "scheduler-prune")
	manager.mu.Lock()
	manager.beforePrune = func() error { return errors.New("injected prune failure") }
	manager.mu.Unlock()
	proceed <- struct{}{}
	if code := <-codes; code != farmerr.BACKUP_FAILED {
		t.Fatalf("scheduler prune-failure code=%s", code)
	}
	manager.mu.Lock()
	manager.beforePrune = nil
	manager.mu.Unlock()
	proceed <- struct{}{}
	if code := <-codes; code != farmerr.BACKUP_SKIPPED_UNCHANGED {
		t.Fatalf("scheduler retention-debt retry code=%s", code)
	}
	values, err := manager.List(context.Background())
	if err != nil || len(values) != 2 {
		t.Fatalf("scheduler retention retry created a duplicate backup values=%d err=%v", len(values), err)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("scheduler did not stop after cancellation")
	}
}

func TestProductionPruneRetainsHourlyDailyAndPreservesUnknownArtifacts(t *testing.T) {
	ctx := context.Background()
	manager, db, _ := newTestManager(t)
	base := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	for index := 0; index < 40; index++ {
		when := base.Add(-time.Duration(index) * time.Hour)
		if index >= 30 {
			when = base.Add(-time.Duration(index-27) * 24 * time.Hour)
		}
		manager.clock = func() time.Time { return when }
		if _, err := manager.create(ctx, db, ClassHourly); err != nil {
			t.Fatal(err)
		}
	}
	manager.clock = func() time.Time { return base.Add(-365 * 24 * time.Hour) }
	manual, err := manager.CreateManual(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	safety, err := manager.create(ctx, db, ClassSafety)
	if err != nil {
		t.Fatal(err)
	}
	unknown := filepath.Join(manager.backupDir, "operator-file")
	if err := os.WriteFile(unknown, []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	malformed := filepath.Join(manager.backupDir, "backup_cccccccccccccccccccccccccccccccc")
	if err := os.Mkdir(malformed, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(malformed, "foreign"), []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	before, err := manager.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := Retained(before, defaultHourlyRetention, defaultDailyRetention)
	if err := manager.Prune(ctx); err != nil {
		t.Fatal(err)
	}
	after, err := manager.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(want) || !want[manual.Manifest.BackupID] || !want[safety.Manifest.BackupID] {
		t.Fatalf("production retention got=%d want=%d manual=%t safety=%t", len(after), len(want), want[manual.Manifest.BackupID], want[safety.Manifest.BackupID])
	}
	for _, item := range after {
		if !want[item.BackupID] {
			t.Fatalf("unexpected retained backup %s", item.BackupID)
		}
	}
	if data, err := os.ReadFile(unknown); err != nil || string(data) != "preserve" {
		t.Fatalf("unknown artifact changed data=%q err=%v", data, err)
	}
	if data, err := os.ReadFile(filepath.Join(malformed, "foreign")); err != nil || string(data) != "preserve" {
		t.Fatalf("malformed artifact changed data=%q err=%v", data, err)
	}
}

func TestCorruptRestoreIsRejectedAndLeavesLiveStateIntact(t *testing.T) {
	ctx := context.Background()
	manager, db, dataDir := newTestManager(t)
	insertHold(t, db, "backup")
	backup, err := manager.CreateManual(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().ExecContext(ctx, "UPDATE maintenance_holds SET reason='live',revision=revision+1,updated_at_ns=updated_at_ns+1 WHERE host_id=?", testHostID.String()); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(manager.backupDir, backup.Manifest.BackupID.String(), DatabaseFile)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = file.WriteString("checksum-mismatch")
	_ = file.Close()
	lease, err := controllerlock.Acquire(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	if _, err := manager.Restore(ctx, backup.Manifest.BackupID, lease); code(err) != farmerr.BACKUP_CORRUPT {
		t.Fatalf("corrupt restore error=%v", err)
	}
	reopened, err := controllerdb.Open(ctx, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	var reason string
	if err := reopened.SQL().QueryRowContext(ctx, "SELECT reason FROM maintenance_holds WHERE host_id=?", testHostID.String()).Scan(&reason); err != nil || reason != "live" {
		t.Fatalf("corrupt restore changed live state reason=%q err=%v", reason, err)
	}
}

func TestManifestRejectsTraversalAndUnknownArtifactsArePreserved(t *testing.T) {
	manager, db, _ := newTestManager(t)
	defer db.Close()
	if _, err := ParseBackupID("../farm.db"); err == nil {
		t.Fatal("traversal BackupID accepted")
	}
	unknown := filepath.Join(manager.backupDir, "operator-notes")
	if err := ensurePrivateDir(manager.backupDir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unknown, []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	malformed := filepath.Join(manager.backupDir, "backup_cccccccccccccccccccccccccccccccc")
	if err := os.Mkdir(malformed, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(malformed, "foreign-data"), []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := manager.Prune(context.Background()); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(unknown); err != nil || string(data) != "preserve" {
		t.Fatalf("unknown file was modified data=%q err=%v", data, err)
	}
	if data, err := os.ReadFile(filepath.Join(malformed, "foreign-data")); err != nil || string(data) != "preserve" {
		t.Fatalf("malformed artifact was modified data=%q err=%v", data, err)
	}
}

func newTestManager(t *testing.T) (*Manager, *controllerdb.DB, string) {
	t.Helper()
	dataDir := filepath.Join(t.TempDir(), "controller")
	if err := os.Mkdir(dataDir, 0700); err != nil {
		t.Fatal(err)
	}
	db, err := controllerdb.Open(context.Background(), dataDir)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := New(Config{DataDir: dataDir, ControllerID: testControllerID, FarmID: testFarmID, TrustFingerprint: testTrustFingerprint, Clock: func() time.Time { return time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC) }})
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return manager, db, dataDir
}

func interruptedRestoreWithRetainedOld(t *testing.T) (*Manager, string, *controllerlock.Lease, []byte) {
	t.Helper()
	ctx := context.Background()
	manager, db, dataDir := newTestManager(t)
	insertHold(t, db, "backup")
	backup, err := manager.CreateManual(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().ExecContext(ctx, "UPDATE maintenance_holds SET reason='live',revision=revision+1,updated_at_ns=updated_at_ns+1 WHERE host_id=?", testHostID.String()); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	lease, err := controllerlock.Acquire(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lease.Close() })
	manager.afterRetainOld = func() error { return errors.New("simulated crash after exact original retention") }
	if _, err := manager.Restore(ctx, backup.Manifest.BackupID, lease); code(err) != farmerr.RESTORE_VALIDATION_FAILED {
		t.Fatalf("restore did not stop after retaining original database: %v", err)
	}
	manager.afterRetainOld = nil
	if _, err := os.Lstat(filepath.Join(dataDir, controllerdb.FileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("interrupted restore unexpectedly retained authoritative farm.db: %v", err)
	}
	original, err := os.ReadFile(filepath.Join(dataDir, restoreOldFile))
	if err != nil {
		t.Fatal(err)
	}
	return manager, dataDir, lease, original
}

func interruptedRestoreWithInstalledRestored(t *testing.T) (*Manager, string, *controllerlock.Lease) {
	t.Helper()
	ctx := context.Background()
	manager, db, dataDir := newTestManager(t)
	insertHold(t, db, "backup")
	backup, err := manager.CreateManual(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().ExecContext(ctx, "UPDATE maintenance_holds SET reason='live',revision=revision+1,updated_at_ns=updated_at_ns+1 WHERE host_id=?", testHostID.String()); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	lease, err := controllerlock.Acquire(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lease.Close() })
	manager.afterInstallLink = func() error { return errors.New("simulated crash before install directory sync") }
	if _, err := manager.Restore(ctx, backup.Manifest.BackupID, lease); code(err) != farmerr.RESTORE_VALIDATION_FAILED {
		t.Fatalf("restore did not stop after exact restored-object install: %v", err)
	}
	manager.afterInstallLink = nil
	return manager, dataDir, lease
}

func restoreValidationName(transaction restoreTransaction) string {
	return ".restore-exact-" + strings.TrimPrefix(transaction.BackupID.String(), "backup_")
}

func assertRecoveryEntryExists(t *testing.T, dataDir, name string) {
	t.Helper()
	if _, err := os.Lstat(filepath.Join(dataDir, name)); err != nil {
		t.Fatalf("recovery entry %q is missing: %v", name, err)
	}
}

func assertHoldReason(t *testing.T, dataDir, expected string) {
	t.Helper()
	db, err := controllerdb.Open(context.Background(), dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var reason string
	if err := db.SQL().QueryRow("SELECT reason FROM maintenance_holds WHERE host_id=?", testHostID.String()).Scan(&reason); err != nil || reason != expected {
		t.Fatalf("Hold reason=%q want=%q err=%v", reason, expected, err)
	}
}

func insertHold(t *testing.T, db *controllerdb.DB, reason string) {
	t.Helper()
	now := time.Now().UTC().UnixNano()
	if _, err := db.SQL().Exec("INSERT INTO maintenance_holds(host_id,active,revision,reason,created_at_ns,updated_at_ns) VALUES(?,?,?,?,?,?)", testHostID.String(), true, 1, reason, now, now); err != nil {
		t.Fatal(err)
	}
}

func insertDesiredRuntime(t *testing.T, db *controllerdb.DB) (identity.WorkloadID, identity.ExecutionID) {
	t.Helper()
	catalog, err := packagecatalog.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	service, err := farmconfig.New(db, catalog, farmconfig.Options{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	pool, err := service.CreatePool(ctx, farmmodel.PoolContent{Name: "pool", Address: "pool.example:443", TLS: true, Auth: farmmodel.PoolAuth{Kind: farmmodel.PoolAuthNone}})
	if err != nil {
		t.Fatal(err)
	}
	wallet, err := service.CreateWalletRef(ctx, farmmodel.WalletRefContent{Name: "wallet", Coin: "XMR", Address: "public-payout-address"})
	if err != nil {
		t.Fatal(err)
	}
	manifest := xmrig.Manifest()
	profile, err := service.CreateMiningProfile(ctx, farmmodel.MiningProfileContent{Name: "profile", AdapterID: xmrig.AdapterID, Package: farmmodel.PackageRef{PackageID: manifest.PackageID, Version: manifest.Version}, Mode: farmmodel.ProfileModeMining, Coin: "XMR", Algorithm: "rx/0", PoolID: pool.PoolID, WalletID: wallet.WalletID, LoginPolicy: farmmodel.LoginPolicy{UserTemplate: "${wallet}", WorkerPlacement: farmmodel.WorkerNone}})
	if err != nil {
		t.Fatal(err)
	}
	workload, err := service.CreateDesiredWorkload(ctx, farmmodel.DesiredWorkloadContent{Name: "workload", HostID: testHostID, ProfileID: profile.ProfileID, RunState: farmmodel.DesiredRunning, Resources: farmmodel.ResourceClaim{CPU: true}})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := service.GetCurrentResolvedSnapshot(ctx, workload.WorkloadID)
	if err != nil {
		t.Fatal(err)
	}
	return workload.WorkloadID, snapshot.ExecutionID
}

func insertIncident(t *testing.T, db *controllerdb.DB, state string) {
	t.Helper()
	now := time.Now().UTC().UnixNano()
	_, err := db.SQL().Exec(`INSERT INTO incidents(incident_id,incident_key,incident_type,severity,lifecycle_state,host_id,workload_id,execution_id,device_id,first_observed_at_ns,last_observed_at_ns,resolved_at_ns,occurrence_count,reason_code,source) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, "incident_0123456789abcdef0123456789abcdef", "AGENT_OFFLINE|"+testHostID.String(), "AGENT_OFFLINE", "ERROR", state, testHostID.String(), nil, nil, nil, now, now, nil, 1, "SERVICE_NOT_READY", "CONTROLLER")
	if err != nil {
		t.Fatal(err)
	}
}

func code(err error) farmerr.Code {
	value, _ := farmerr.CodeOf(err)
	return value
}
