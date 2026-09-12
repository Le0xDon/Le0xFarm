//go:build linux

package controllerbackup

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/le0xdon/le0xfarm/internal/controllerdb"
	"github.com/le0xdon/le0xfarm/internal/controllerlock"
	"github.com/le0xdon/le0xfarm/internal/farmerr"
	"github.com/le0xdon/le0xfarm/internal/identity"
	"github.com/le0xdon/le0xfarm/internal/version"
	"golang.org/x/sys/unix"
	_ "modernc.org/sqlite"
)

const (
	defaultHourlyRetention = 24
	defaultDailyRetention  = 7
	maxManifestBytes       = 64 * 1024
)

type Config struct {
	DataDir          string
	BackupDir        string
	ControllerID     identity.ControllerID
	FarmID           identity.FarmID
	TrustFingerprint string
	Clock            func() time.Time
	Lease            *controllerlock.Lease
}

type Manager struct {
	dataDir            string
	backupDir          string
	controllerID       identity.ControllerID
	farmID             identity.FarmID
	trustFingerprint   string
	clock              func() time.Time
	lease              *controllerlock.Lease
	mu                 sync.Mutex
	beforeSnapshot     func()
	beforePublish      func() error
	afterPublish       func() error
	beforeInstall      func() error
	afterRetainOld     func() error
	beforeInstallLink  func() error
	afterInstallLink   func() error
	beforeOldPromotion func() error
	beforePrune        func() error
	beforePruneDelete  func(BackupID) error
}

type RestoreResult struct {
	Code            farmerr.Code
	Backup          Manifest
	SafetyBackupID  BackupID
	HostBarriers    int
	RestartRequired bool
}

func New(config Config) (*Manager, error) {
	if config.DataDir == "" {
		return nil, backupError(farmerr.CONFIG_CONFLICT, "Controller data directory is required", nil)
	}
	dataDir, err := filepath.Abs(config.DataDir)
	if err != nil || config.ControllerID.Validate() != nil || config.FarmID.Validate() != nil || !validTrustFingerprint(config.TrustFingerprint) {
		return nil, backupError(farmerr.CONFIG_CONFLICT, "invalid Controller backup configuration", err)
	}
	backupDir := config.BackupDir
	if backupDir == "" {
		backupDir = filepath.Join(dataDir, "backups")
	}
	backupDir, err = filepath.Abs(backupDir)
	if err != nil || backupDir == dataDir || filepath.Dir(backupDir) != dataDir {
		return nil, backupError(farmerr.CONFIG_CONFLICT, "invalid Controller backup directory", err)
	}
	if config.Clock == nil {
		config.Clock = time.Now
	}
	if config.Lease != nil {
		if err := config.Lease.Validate(dataDir); err != nil {
			return nil, err
		}
	}
	return &Manager{dataDir: dataDir, backupDir: backupDir, controllerID: config.ControllerID, farmID: config.FarmID, trustFingerprint: config.TrustFingerprint, clock: config.Clock, lease: config.Lease}, nil
}

func (manager *Manager) openBackupRoot() (*os.File, error) {
	if manager.lease == nil {
		return openBackupRoot(manager.dataDir, manager.backupDir)
	}
	data, err := manager.lease.OpenDirectory(manager.dataDir)
	if err != nil {
		return nil, err
	}
	defer data.Close()
	return openBackupRootAt(data, manager.dataDir, manager.backupDir)
}

func (manager *Manager) validateBackupRoot(expected fileObjectIdentity) error {
	if manager.lease != nil {
		if err := manager.lease.Validate(manager.dataDir); err != nil {
			return err
		}
		root, err := manager.openBackupRoot()
		if err != nil {
			return err
		}
		defer root.Close()
		actual, err := privateDirectoryIdentity(root)
		if err != nil || actual != expected {
			return errors.New("lease-bound backup root was replaced")
		}
		return nil
	}
	return validateBackupRootPath(manager.dataDir, manager.backupDir, expected)
}

func (manager *Manager) CreateManual(ctx context.Context, db *controllerdb.DB) (CreateResult, error) {
	return manager.create(ctx, db, ClassManual)
}

func (manager *Manager) CreateScheduled(ctx context.Context, db *controllerdb.DB) (CreateResult, error) {
	if !manager.mu.TryLock() {
		return CreateResult{}, backupError(farmerr.BACKUP_BUSY, "Controller backup is already running", nil)
	}
	defer manager.mu.Unlock()
	revision, last, retentionPending, err := backupState(ctx, db.SQL())
	if err != nil {
		return CreateResult{}, backupError(farmerr.BACKUP_FAILED, "cannot read persistent backup state", err)
	}
	if revision == last {
		if !retentionPending {
			return CreateResult{Code: farmerr.BACKUP_SKIPPED_UNCHANGED}, nil
		}
		if err := manager.pruneLocked(ctx); err != nil {
			return CreateResult{}, err
		}
		if err := clearRetentionPending(ctx, db.SQL()); err != nil {
			return CreateResult{}, backupError(farmerr.BACKUP_FAILED, "retention succeeded but its durable state could not be recorded", err)
		}
		return CreateResult{Code: farmerr.BACKUP_SKIPPED_UNCHANGED}, nil
	}
	result, err := manager.createLocked(ctx, db, ClassHourly)
	if err != nil {
		return CreateResult{}, err
	}
	if err := markAutomaticRevision(ctx, db.SQL(), result.Manifest.StateRevision); err != nil {
		return CreateResult{}, backupError(farmerr.BACKUP_FAILED, "backup was created but its change watermark could not be recorded", err)
	}
	// A write after the coherent snapshot is intentionally left pending for the
	// next interval instead of being incorrectly covered by this backup.
	if err := manager.pruneLocked(ctx); err != nil {
		return CreateResult{}, err
	}
	if err := clearRetentionPending(ctx, db.SQL()); err != nil {
		return CreateResult{}, backupError(farmerr.BACKUP_FAILED, "retention succeeded but its durable state could not be recorded", err)
	}
	return result, nil
}

func (manager *Manager) create(ctx context.Context, db *controllerdb.DB, class Class) (CreateResult, error) {
	if !manager.mu.TryLock() {
		return CreateResult{}, backupError(farmerr.BACKUP_BUSY, "Controller backup is already running", nil)
	}
	defer manager.mu.Unlock()
	return manager.createLocked(ctx, db, class)
}

func (manager *Manager) createLocked(ctx context.Context, db *controllerdb.DB, class Class) (CreateResult, error) {
	if db == nil || !validateClass(class) {
		return CreateResult{}, backupError(farmerr.CONFIG_CONFLICT, "invalid backup request", nil)
	}
	sourcePath, err := filepath.Abs(db.Path())
	if err != nil || sourcePath != filepath.Join(manager.dataDir, controllerdb.FileName) {
		return CreateResult{}, backupError(farmerr.CONFIG_CONFLICT, "backup source does not belong to this Controller data directory", err)
	}
	root, err := manager.openBackupRoot()
	if err != nil {
		return CreateResult{}, preserveTypedError(err, backupError(farmerr.PERMISSION_DENIED, "cannot open Controller backup directory", err))
	}
	defer root.Close()
	rootIdentity, err := privateDirectoryIdentity(root)
	if err != nil {
		return CreateResult{}, backupError(farmerr.PERMISSION_DENIED, "Controller backup directory identity is unsafe", err)
	}
	id, err := newBackupID()
	if err != nil {
		return CreateResult{}, backupError(farmerr.BACKUP_FAILED, "cannot create BackupID", err)
	}
	tempName := ".incomplete-" + strings.TrimPrefix(id.String(), "backup_")
	tempDir, err := createPrivateDirectoryAt(root, tempName)
	if err != nil {
		return CreateResult{}, backupError(farmerr.BACKUP_FAILED, "cannot prepare backup", err)
	}
	defer tempDir.Close()
	defer cleanupStagingDirectory(root, tempDir, tempName)
	databasePath := filepath.Join(procDescriptorPath(tempDir), DatabaseFile)
	if manager.beforeSnapshot != nil {
		manager.beforeSnapshot()
	}
	if _, err := db.SQL().ExecContext(ctx, "VACUUM main INTO ?", databasePath); err != nil {
		return CreateResult{}, backupError(farmerr.BACKUP_FAILED, "cannot create consistent SQLite backup", err)
	}
	database, err := secureExistingRegularAt(tempDir, DatabaseFile)
	if err != nil {
		return CreateResult{}, backupError(farmerr.BACKUP_FAILED, "cannot secure backup database", err)
	}
	if err := database.Close(); err != nil {
		return CreateResult{}, backupError(farmerr.BACKUP_FAILED, "cannot close backup database", err)
	}
	createdAt := manager.clock().UTC()
	if err := stampBackupArtifact(ctx, databasePath, id, class, manager.controllerID, manager.farmID, manager.trustFingerprint, createdAt); err != nil {
		return CreateResult{}, backupError(farmerr.BACKUP_FAILED, "cannot bind backup provenance", err)
	}
	revision, _, _, err := backupStateFile(ctx, databasePath)
	if err != nil {
		return CreateResult{}, backupError(farmerr.BACKUP_CORRUPT, "created backup has invalid persistent state", err)
	}
	database, err = openRegularAt(tempDir, DatabaseFile)
	if err != nil {
		return CreateResult{}, backupError(farmerr.BACKUP_FAILED, "cannot open backup database", err)
	}
	size, checksum, checksumErr := checksumFile(database)
	closeErr := database.Close()
	err = errors.Join(checksumErr, closeErr)
	if err != nil {
		return CreateResult{}, backupError(farmerr.BACKUP_FAILED, "cannot checksum backup database", err)
	}
	manifest := Manifest{FormatVersion: FormatVersion, BackupID: id, Class: class, CreatedAt: createdAt, ControllerID: manager.controllerID, FarmID: manager.farmID, TrustFingerprint: manager.trustFingerprint, ApplicationVersion: version.Version, DatabaseSchema: controllerdb.SchemaVersion, StateRevision: revision, DatabaseFile: DatabaseFile, DatabaseSize: size, DatabaseSHA256: checksum}
	if err := writeManifestAt(tempDir, manifest); err != nil {
		return CreateResult{}, err
	}
	if _, err := verifyArtifactDirectory(ctx, tempDir, id); err != nil {
		return CreateResult{}, err
	}
	if manager.beforePublish != nil {
		if err := manager.beforePublish(); err != nil {
			return CreateResult{}, backupError(farmerr.BACKUP_FAILED, "backup publication was interrupted", err)
		}
	}
	if err := syncArtifactDirectory(tempDir); err != nil {
		return CreateResult{}, backupError(farmerr.BACKUP_FAILED, "cannot durably stage backup", err)
	}
	if _, err := directoryEntryIdentity(root, id.String(), true); !errors.Is(err, unix.ENOENT) {
		return CreateResult{}, backupError(farmerr.BACKUP_FAILED, "BackupID collision", err)
	}
	if err := unix.Renameat(int(root.Fd()), tempName, int(root.Fd()), id.String()); err != nil {
		return CreateResult{}, backupError(farmerr.BACKUP_FAILED, "cannot atomically publish backup", err)
	}
	if manager.afterPublish != nil {
		if err := manager.afterPublish(); err != nil {
			return CreateResult{}, backupError(farmerr.BACKUP_FAILED, "published backup verification was interrupted", err)
		}
	}
	tempIdentity, err := privateDirectoryIdentity(tempDir)
	if err != nil || entryMatches(root, id.String(), tempIdentity, true) != nil {
		return CreateResult{}, backupError(farmerr.BACKUP_FAILED, "published backup directory was substituted", err)
	}
	published, publishedDir, publishedIdentity, err := verifyArtifactAt(ctx, root, id)
	if err != nil {
		return CreateResult{}, backupError(farmerr.BACKUP_FAILED, "published backup failed final verification", err)
	}
	defer publishedDir.Close()
	if publishedIdentity != tempIdentity || published != manifest {
		return CreateResult{}, backupError(farmerr.BACKUP_FAILED, "published backup does not match its staged snapshot", nil)
	}
	if err := entryMatches(root, id.String(), publishedIdentity, true); err != nil {
		return CreateResult{}, backupError(farmerr.BACKUP_FAILED, "published backup entry changed during verification", err)
	}
	if err := syncDirectoryFile(root); err != nil {
		return CreateResult{}, backupError(farmerr.BACKUP_FAILED, "cannot durably publish backup", err)
	}
	if err := manager.validateBackupRoot(rootIdentity); err != nil {
		return CreateResult{}, backupError(farmerr.BACKUP_FAILED, "published backup root lost path continuity", err)
	}
	return CreateResult{Code: farmerr.BACKUP_CREATED, Created: true, Manifest: published}, nil
}

func (manager *Manager) Verify(ctx context.Context, id BackupID) (Manifest, error) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	return manager.verifyLocked(ctx, id)
}

func (manager *Manager) verifyLocked(ctx context.Context, id BackupID) (Manifest, error) {
	if err := id.Validate(); err != nil {
		return Manifest{}, backupError(farmerr.RESTORE_VALIDATION_FAILED, "invalid BackupID", err)
	}
	root, err := manager.openBackupRoot()
	if err != nil {
		return Manifest{}, preserveTypedError(err, backupError(farmerr.PERMISSION_DENIED, "cannot open Controller backup directory", err))
	}
	defer root.Close()
	manifest, artifact, _, err := verifyArtifactAt(ctx, root, id)
	if artifact != nil {
		_ = artifact.Close()
	}
	return manifest, err
}

func (manager *Manager) List(ctx context.Context) ([]Manifest, error) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	return manager.listLocked(ctx)
}

func (manager *Manager) listLocked(ctx context.Context) ([]Manifest, error) {
	root, err := manager.openBackupRoot()
	if err != nil {
		return nil, preserveTypedError(err, backupError(farmerr.PERMISSION_DENIED, "cannot open Controller backup directory", err))
	}
	defer root.Close()
	return listFromRoot(ctx, root)
}

func listFromRoot(ctx context.Context, root *os.File) ([]Manifest, error) {
	entries, err := directoryNames(root)
	if err != nil {
		return nil, backupError(farmerr.BACKUP_FAILED, "cannot list Controller backups", err)
	}
	var result []Manifest
	for _, name := range entries {
		id, err := ParseBackupID(name)
		if err != nil {
			continue
		}
		manifest, artifact, _, err := verifyArtifactAt(ctx, root, id)
		if artifact != nil {
			_ = artifact.Close()
		}
		if err == nil {
			result = append(result, manifest)
		}
	}
	sortManifests(result)
	return result, nil
}

func (manager *Manager) Prune(ctx context.Context) error {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	return manager.pruneLocked(ctx)
}

func (manager *Manager) pruneLocked(ctx context.Context) error {
	if manager.beforePrune != nil {
		if err := manager.beforePrune(); err != nil {
			return backupError(farmerr.BACKUP_FAILED, "Controller backup retention failed", err)
		}
	}
	root, err := manager.openBackupRoot()
	if err != nil {
		return preserveTypedError(err, backupError(farmerr.PERMISSION_DENIED, "cannot open Controller backup directory", err))
	}
	defer root.Close()
	rootIdentity, err := privateDirectoryIdentity(root)
	if err != nil {
		return backupError(farmerr.PERMISSION_DENIED, "Controller backup directory identity is unsafe", err)
	}
	values, err := listFromRoot(ctx, root)
	if err != nil {
		return err
	}
	retained := Retained(values, defaultHourlyRetention, defaultDailyRetention)
	for _, item := range values {
		if retained[item.BackupID] {
			continue
		}
		_, artifact, identity, err := verifyArtifactAt(ctx, root, item.BackupID)
		if err != nil {
			return backupError(farmerr.BACKUP_FAILED, "managed backup changed before pruning", err)
		}
		if manager.beforePruneDelete != nil {
			if hookErr := manager.beforePruneDelete(item.BackupID); hookErr != nil {
				_ = artifact.Close()
				return backupError(farmerr.BACKUP_FAILED, "Controller backup retention was interrupted", hookErr)
			}
		}
		err = removeExpectedArtifactAt(root, artifact, item.BackupID.String(), identity)
		closeErr := artifact.Close()
		if err := errors.Join(err, closeErr); err != nil {
			return backupError(farmerr.BACKUP_FAILED, "cannot prune managed backup", err)
		}
	}
	if err := syncDirectoryFile(root); err != nil {
		return err
	}
	if err := manager.validateBackupRoot(rootIdentity); err != nil {
		return backupError(farmerr.BACKUP_FAILED, "Controller backup root lost path continuity during pruning", err)
	}
	return nil
}

func (manager *Manager) Run(ctx context.Context, db *controllerdb.DB, interval time.Duration, output func(farmerr.Code)) {
	if interval <= 0 {
		interval = time.Hour
	}
	run := func() bool {
		if ctx.Err() != nil {
			return false
		}
		result, err := manager.CreateScheduled(ctx, db)
		if ctx.Err() != nil {
			return false
		}
		if output == nil {
			return true
		}
		if err != nil {
			code, ok := farmerr.CodeOf(err)
			if !ok {
				code = farmerr.BACKUP_FAILED
			}
			output(code)
			return true
		}
		output(result.Code)
		return true
	}
	if !run() {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !run() {
				return
			}
		}
	}
}

func (manager *Manager) Restore(ctx context.Context, id BackupID, lease *controllerlock.Lease) (RestoreResult, error) {
	if !manager.mu.TryLock() {
		return RestoreResult{}, backupError(farmerr.BACKUP_BUSY, "Controller backup operation is already running", nil)
	}
	defer manager.mu.Unlock()
	if err := lease.Validate(manager.dataDir); err != nil {
		return RestoreResult{}, err
	}
	if err := RecoverInterruptedRestore(ctx, manager.dataDir, lease, manager.controllerID, manager.farmID, manager.trustFingerprint); err != nil {
		return RestoreResult{}, err
	}
	manifest, err := manager.verifyLockedWithoutLock(ctx, id)
	if err != nil {
		return RestoreResult{}, err
	}
	if manifest.ControllerID != manager.controllerID || manifest.FarmID != manager.farmID || manifest.TrustFingerprint != manager.trustFingerprint {
		return RestoreResult{}, backupError(farmerr.RESTORE_IDENTITY_MISMATCH, "backup belongs to a different Controller/Farm trust identity", nil)
	}
	result := RestoreResult{Backup: manifest, RestartRequired: true}
	livePath := filepath.Join(manager.dataDir, controllerdb.FileName)
	if info, statErr := os.Lstat(livePath); statErr == nil {
		if !info.Mode().IsRegular() {
			return RestoreResult{}, backupError(farmerr.RESTORE_VALIDATION_FAILED, "live Controller database is not a regular file", nil)
		}
		liveDB, err := controllerdb.Open(ctx, manager.dataDir)
		if err != nil {
			return RestoreResult{}, backupError(farmerr.RESTORE_VALIDATION_FAILED, "live Controller database cannot be opened safely", err)
		}
		safety, backupErr := manager.createLocked(ctx, liveDB, ClassSafety)
		if backupErr == nil {
			_, backupErr = liveDB.SQL().ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)")
		}
		closeErr := liveDB.Close()
		if backupErr != nil || closeErr != nil {
			return RestoreResult{}, backupError(farmerr.BACKUP_FAILED, "pre-restore safety backup failed", errors.Join(backupErr, closeErr))
		}
		result.SafetyBackupID = safety.Manifest.BackupID
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return RestoreResult{}, backupError(farmerr.RESTORE_VALIDATION_FAILED, "live Controller database cannot be inspected", statErr)
	}
	candidate, candidateFile, candidateIdentity, err := manager.copyRestoreCandidate(ctx, id, manifest)
	if err != nil {
		return RestoreResult{}, err
	}
	defer os.Remove(candidate)
	defer candidateFile.Close()
	if err := validateBoundPath(candidate, candidateFile, candidateIdentity); err != nil {
		return RestoreResult{}, backupError(farmerr.RESTORE_VALIDATION_FAILED, "restore candidate changed before safety preparation", err)
	}
	barriers, err := prepareRestoreCandidate(ctx, candidate, manifest, manager.clock().UTC())
	if err != nil {
		return RestoreResult{}, err
	}
	result.HostBarriers = barriers
	if err := validateBoundPath(candidate, candidateFile, candidateIdentity); err != nil {
		return RestoreResult{}, backupError(farmerr.RESTORE_VALIDATION_FAILED, "prepared restore candidate changed", err)
	}
	if err := candidateFile.Sync(); err != nil {
		return RestoreResult{}, backupError(farmerr.RESTORE_VALIDATION_FAILED, "cannot durably prepare restore candidate", err)
	}
	if manager.beforeInstall != nil {
		if err := manager.beforeInstall(); err != nil {
			return RestoreResult{}, backupError(farmerr.RESTORE_VALIDATION_FAILED, "restore stopped before installation", err)
		}
	}
	if err := validateBoundPath(candidate, candidateFile, candidateIdentity); err != nil {
		return RestoreResult{}, backupError(farmerr.RESTORE_VALIDATION_FAILED, "restore candidate changed before final validation", err)
	}
	if err := validatePreparedRestoreCandidate(ctx, candidate, manifest, barriers); err != nil {
		return RestoreResult{}, backupError(farmerr.RESTORE_VALIDATION_FAILED, "restore candidate lost its safety barrier", err)
	}

	// From this point onward the source pathname is irrelevant. The exact bytes
	// being installed live in an unnamed inode selected only by this descriptor.
	dataDirectory, err := lease.OpenDirectory(manager.dataDir)
	if err != nil {
		return RestoreResult{}, err
	}
	defer dataDirectory.Close()
	anonymous, err := createAnonymousRegular(dataDirectory)
	if err != nil {
		return RestoreResult{}, backupError(farmerr.RESTORE_VALIDATION_FAILED, "Controller filesystem cannot create a descriptor-bound restore candidate", err)
	}
	defer anonymous.Close()
	if _, copiedChecksum, err := copyExact(anonymous, candidateFile); err != nil {
		return RestoreResult{}, backupError(farmerr.RESTORE_VALIDATION_FAILED, "cannot copy prepared restore state into exact install object", err)
	} else if copiedChecksum == "" {
		return RestoreResult{}, backupError(farmerr.RESTORE_VALIDATION_FAILED, "exact restore object checksum is unavailable", nil)
	}
	if err := anonymous.Sync(); err != nil {
		return RestoreResult{}, backupError(farmerr.RESTORE_VALIDATION_FAILED, "cannot durably sync exact install object", err)
	}
	validationName := ".restore-exact-" + strings.TrimPrefix(manifest.BackupID.String(), "backup_")
	validationFile, validationIdentity, err := bindAnonymousRegular(dataDirectory, anonymous, validationName)
	if err != nil {
		return RestoreResult{}, backupError(farmerr.RESTORE_VALIDATION_FAILED, "cannot bind exact restore object for SQLite validation", err)
	}
	defer validationFile.Close()
	defer removeBoundRegularEntryAt(dataDirectory, validationName, validationIdentity)
	if err := validatePreparedRestoreCandidate(ctx, procDescriptorPath(validationFile), manifest, barriers); err != nil {
		return RestoreResult{}, backupError(farmerr.RESTORE_VALIDATION_FAILED, "exact restore object lost its safety barrier", err)
	}
	candidateSize, candidateChecksum, err := checksumFile(validationFile)
	if err != nil {
		return RestoreResult{}, backupError(farmerr.RESTORE_VALIDATION_FAILED, "cannot checksum exact restore object", err)
	}
	candidateBinding := restoreFileBinding{Identity: validationIdentity, Size: candidateSize, SHA256: candidateChecksum}
	if err := installExactRestore(ctx, dataDirectory, anonymous, validationName, candidateBinding, manifest, barriers, manager.afterRetainOld, manager.beforeInstallLink, manager.afterInstallLink, manager.beforeOldPromotion); err != nil {
		return RestoreResult{}, err
	}
	if err := lease.Validate(manager.dataDir); err != nil {
		return RestoreResult{}, err
	}
	result.Code = farmerr.RESTORE_RECONCILIATION_REQUIRED
	return result, nil
}

type restoreTransaction struct {
	BackupID       BackupID
	Restored       restoreFileBinding
	HadLive        bool
	Original       restoreFileBinding
	MarkerIdentity fileObjectIdentity
}

type restoreFileBinding struct {
	Identity fileObjectIdentity
	Size     int64
	SHA256   string
}

type restoreRecoveryHooks struct {
	beforeOldPromotion             func() error
	beforeRestoredDirectorySync    func() error
	afterRestoredDirectorySync     func() error
	afterRestoredValidationCleanup func() error
}

var errRestoreIdentityMismatch = errors.New("restored database trust identity does not match current Controller")

func validateRestoreFileBinding(binding restoreFileBinding) error {
	if binding.Identity.device == 0 || binding.Identity.inode == 0 || binding.Size <= 0 || !validSHA256(binding.SHA256) {
		return errors.New("restore file binding is invalid")
	}
	return nil
}

func validateRestoreTransaction(transaction restoreTransaction, requireMarker bool) error {
	if err := transaction.BackupID.Validate(); err != nil {
		return err
	}
	if err := validateRestoreFileBinding(transaction.Restored); err != nil {
		return err
	}
	if transaction.HadLive {
		if err := validateRestoreFileBinding(transaction.Original); err != nil {
			return err
		}
	} else if transaction.Original != (restoreFileBinding{}) {
		return errors.New("restore transaction has an unexpected original binding")
	}
	if requireMarker && (transaction.MarkerIdentity.device == 0 || transaction.MarkerIdentity.inode == 0) {
		return errors.New("restore marker identity is invalid")
	}
	return nil
}

func createRestoreTransactionMarker(directory *os.File, transaction restoreTransaction) (*os.File, restoreTransaction, error) {
	if err := validateRestoreTransaction(transaction, false); err != nil {
		return nil, restoreTransaction{}, err
	}
	marker, err := createRegularAt(directory, restoreMarkerFile)
	if err != nil {
		return nil, restoreTransaction{}, err
	}
	markerIdentity, err := privateRegularIdentity(marker, 1)
	if err != nil {
		marker.Close()
		return nil, restoreTransaction{}, err
	}
	transaction.MarkerIdentity = markerIdentity
	value := "0"
	if transaction.HadLive {
		value = "1"
	}
	originalChecksum := "-"
	if transaction.HadLive {
		originalChecksum = transaction.Original.SHA256
	}
	contents := strings.Join([]string{
		"M10_RESTORE_V3",
		transaction.BackupID.String(),
		transaction.Restored.SHA256,
		strconv.FormatInt(transaction.Restored.Size, 10),
		strconv.FormatUint(transaction.Restored.Identity.device, 10),
		strconv.FormatUint(transaction.Restored.Identity.inode, 10),
		value,
		originalChecksum,
		strconv.FormatInt(transaction.Original.Size, 10),
		strconv.FormatUint(transaction.Original.Identity.device, 10),
		strconv.FormatUint(transaction.Original.Identity.inode, 10),
		strconv.FormatUint(transaction.MarkerIdentity.device, 10),
		strconv.FormatUint(transaction.MarkerIdentity.inode, 10),
		"",
	}, "\n")
	if _, err := io.WriteString(marker, contents); err != nil {
		marker.Close()
		return nil, restoreTransaction{}, err
	}
	if err := marker.Sync(); err != nil {
		marker.Close()
		return nil, restoreTransaction{}, err
	}
	return marker, transaction, nil
}

func readRestoreTransactionMarker(directory *os.File) (restoreTransaction, error) {
	marker, err := openRegularAt(directory, restoreMarkerFile)
	if err != nil {
		return restoreTransaction{}, err
	}
	defer marker.Close()
	data, err := io.ReadAll(io.LimitReader(marker, 1024))
	if err != nil || len(data) >= 1024 {
		return restoreTransaction{}, errors.New("restore transaction marker exceeds its bound")
	}
	parts := strings.Split(string(data), "\n")
	if len(parts) != 14 || parts[0] != "M10_RESTORE_V3" || parts[13] != "" || parts[6] != "0" && parts[6] != "1" {
		return restoreTransaction{}, errors.New("restore transaction marker is malformed")
	}
	id, err := ParseBackupID(parts[1])
	if err != nil {
		return restoreTransaction{}, err
	}
	parseInt := func(value string) (int64, error) { return strconv.ParseInt(value, 10, 64) }
	parseUint := func(value string) (uint64, error) { return strconv.ParseUint(value, 10, 64) }
	restoredSize, err := parseInt(parts[3])
	if err != nil {
		return restoreTransaction{}, err
	}
	restoredDevice, err := parseUint(parts[4])
	if err != nil {
		return restoreTransaction{}, err
	}
	restoredInode, err := parseUint(parts[5])
	if err != nil {
		return restoreTransaction{}, err
	}
	originalSize, err := parseInt(parts[8])
	if err != nil {
		return restoreTransaction{}, err
	}
	originalDevice, err := parseUint(parts[9])
	if err != nil {
		return restoreTransaction{}, err
	}
	originalInode, err := parseUint(parts[10])
	if err != nil {
		return restoreTransaction{}, err
	}
	markerDevice, err := parseUint(parts[11])
	if err != nil {
		return restoreTransaction{}, err
	}
	markerInode, err := parseUint(parts[12])
	if err != nil {
		return restoreTransaction{}, err
	}
	transaction := restoreTransaction{
		BackupID:       id,
		Restored:       restoreFileBinding{Identity: fileObjectIdentity{device: restoredDevice, inode: restoredInode}, Size: restoredSize, SHA256: parts[2]},
		HadLive:        parts[6] == "1",
		Original:       restoreFileBinding{Identity: fileObjectIdentity{device: originalDevice, inode: originalInode}, Size: originalSize},
		MarkerIdentity: fileObjectIdentity{device: markerDevice, inode: markerInode},
	}
	if transaction.HadLive {
		transaction.Original.SHA256 = parts[7]
	} else if parts[7] != "-" {
		return restoreTransaction{}, errors.New("restore transaction original binding is malformed")
	}
	if err := validateRestoreTransaction(transaction, true); err != nil {
		return restoreTransaction{}, err
	}
	actualMarker, err := privateRegularIdentity(marker, 1)
	if err != nil || actualMarker != transaction.MarkerIdentity {
		return restoreTransaction{}, errors.New("restore transaction marker object was replaced")
	}
	return transaction, nil
}

func installExactRestore(ctx context.Context, directory, candidate *os.File, validationName string, candidateBinding restoreFileBinding, manifest Manifest, barriers int, afterRetainOld, beforeLink, afterLink, beforeOldPromotion func() error) error {
	if matches, err := databaseFileMatches(candidate, candidateBinding, 1); err != nil || !matches {
		return backupError(farmerr.RESTORE_VALIDATION_FAILED, "exact restore object changed before installation", err)
	}
	if err := entryMatches(directory, validationName, candidateBinding.Identity, false); err != nil {
		return backupError(farmerr.RESTORE_VALIDATION_FAILED, "exact restore validation entry changed before installation", err)
	}
	for _, name := range []string{controllerdb.FileName + "-wal", controllerdb.FileName + "-shm"} {
		if err := removeRegularEntryAt(directory, name); err != nil {
			return backupError(farmerr.RESTORE_VALIDATION_FAILED, "unsafe SQLite sidecar blocks restore", err)
		}
	}
	if exists, err := anyEntryExists(directory, restoreOldFile); err != nil || exists {
		return backupError(farmerr.RESTORE_VALIDATION_FAILED, "unfinished restore recovery state blocks installation", err)
	}
	if exists, err := anyEntryExists(directory, restoreMarkerFile); err != nil || exists {
		return backupError(farmerr.RESTORE_VALIDATION_FAILED, "unfinished restore transaction marker blocks installation", err)
	}
	original, hadLive, err := openOptionalPrivateRegularAt(directory, controllerdb.FileName)
	if err != nil {
		return backupError(farmerr.RESTORE_VALIDATION_FAILED, "live Controller database is unsafe", err)
	}
	if original != nil {
		defer original.Close()
	}
	transaction := restoreTransaction{BackupID: manifest.BackupID, Restored: candidateBinding, HadLive: hadLive}
	if hadLive {
		transaction.Original, err = bindDatabaseFile(ctx, original, 1)
		if err != nil {
			return backupError(farmerr.RESTORE_VALIDATION_FAILED, "original Controller database cannot be bound for rollback", err)
		}
		if err := original.Sync(); err != nil {
			return backupError(farmerr.RESTORE_VALIDATION_FAILED, "cannot durably bind original Controller database", err)
		}
	}
	marker, transaction, err := createRestoreTransactionMarker(directory, transaction)
	if err != nil {
		return backupError(farmerr.RESTORE_VALIDATION_FAILED, "cannot establish durable restore transaction marker", err)
	}
	if err := marker.Close(); err != nil {
		return backupError(farmerr.RESTORE_VALIDATION_FAILED, "cannot close durable restore transaction marker", err)
	}
	if err := directory.Sync(); err != nil {
		return backupError(farmerr.RESTORE_VALIDATION_FAILED, "cannot sync restore transaction marker", err)
	}
	if hadLive {
		if err := unix.Linkat(int(original.Fd()), "", int(directory.Fd()), restoreOldFile, unix.AT_EMPTY_PATH); err != nil {
			return backupError(farmerr.RESTORE_VALIDATION_FAILED, "cannot retain original Controller database", err)
		}
		if err := regularEntryMatchesLinks(directory, controllerdb.FileName, transaction.Original.Identity, 2); err != nil {
			return backupError(farmerr.RESTORE_VALIDATION_FAILED, "original Controller database changed while being retained", err)
		}
		if err := regularEntryMatchesLinks(directory, restoreOldFile, transaction.Original.Identity, 2); err != nil {
			return backupError(farmerr.RESTORE_VALIDATION_FAILED, "retained Controller database does not name the exact original", err)
		}
		if err := directory.Sync(); err != nil {
			return backupError(farmerr.RESTORE_VALIDATION_FAILED, "cannot durably retain original Controller database", err)
		}
		if err := removeBoundRegularEntryAt(directory, controllerdb.FileName, transaction.Original.Identity); err != nil {
			return backupError(farmerr.RESTORE_VALIDATION_FAILED, "cannot release original Controller database name", err)
		}
		if matches, err := databaseFileMatches(original, transaction.Original, 1); err != nil || !matches {
			return backupError(farmerr.RESTORE_VALIDATION_FAILED, "retained original Controller database lost its binding", err)
		}
		if err := regularEntryMatchesLinks(directory, restoreOldFile, transaction.Original.Identity, 1); err != nil {
			return backupError(farmerr.RESTORE_VALIDATION_FAILED, "retained original Controller database entry lost its binding", err)
		}
		if err := directory.Sync(); err != nil {
			return backupError(farmerr.RESTORE_VALIDATION_FAILED, "cannot durably retain original Controller database", err)
		}
	}
	if afterRetainOld != nil {
		if err := afterRetainOld(); err != nil {
			return backupError(farmerr.RESTORE_VALIDATION_FAILED, "restore interrupted after retaining original database", err)
		}
	}
	if beforeLink != nil {
		if err := beforeLink(); err != nil {
			rollbackErr := recoverRestoreTransaction(ctx, directory, transaction, manifest.ControllerID, manifest.FarmID, manifest.TrustFingerprint, restoreRecoveryHooks{beforeOldPromotion: beforeOldPromotion})
			return backupError(farmerr.RESTORE_VALIDATION_FAILED, "restore stopped at exact installation boundary", errors.Join(err, rollbackErr))
		}
	}
	// linkat(AT_EMPTY_PATH) selects candidate by its already-open descriptor;
	// there is no source pathname for an attacker to substitute. A raced
	// destination entry makes this fail with EEXIST rather than installing it.
	if err := unix.Linkat(int(candidate.Fd()), "", int(directory.Fd()), controllerdb.FileName, unix.AT_EMPTY_PATH); err != nil {
		rollbackErr := recoverRestoreTransaction(ctx, directory, transaction, manifest.ControllerID, manifest.FarmID, manifest.TrustFingerprint, restoreRecoveryHooks{beforeOldPromotion: beforeOldPromotion})
		return backupError(farmerr.RESTORE_VALIDATION_FAILED, "cannot install exact restored database", errors.Join(err, rollbackErr))
	}
	if afterLink != nil {
		if err := afterLink(); err != nil {
			return backupError(farmerr.RESTORE_VALIDATION_FAILED, "restore interrupted after exact installation", err)
		}
	}
	if err := directory.Sync(); err != nil {
		return backupError(farmerr.RESTORE_VALIDATION_FAILED, "cannot durably publish restored database", err)
	}
	installedFile, err := openRegularAtLinks(directory, controllerdb.FileName, 2)
	if err != nil {
		return backupError(farmerr.RESTORE_VALIDATION_FAILED, "cannot bind installed restored database", err)
	}
	installedLinkedIdentity, err := privateRegularIdentity(installedFile, 2)
	if err != nil || installedLinkedIdentity != candidateBinding.Identity {
		installedFile.Close()
		return backupError(farmerr.RESTORE_VALIDATION_FAILED, "installed restore object does not match exact candidate", err)
	}
	if err := validateDatabaseBinding(ctx, installedFile, candidateBinding, 2); err != nil {
		installedFile.Close()
		return backupError(farmerr.RESTORE_VALIDATION_FAILED, "installed restore checksum changed", err)
	}
	if err := validatePreparedRestoreCandidate(ctx, procDescriptorPath(installedFile), manifest, barriers); err != nil {
		installedFile.Close()
		return backupError(farmerr.RESTORE_VALIDATION_FAILED, "installed restore object failed final barrier validation", err)
	}
	if err := installedFile.Close(); err != nil {
		return backupError(farmerr.RESTORE_VALIDATION_FAILED, "cannot close installed restore validation descriptor", err)
	}
	if err := removeBoundRegularEntryAt(directory, validationName, candidateBinding.Identity); err != nil {
		return backupError(farmerr.RESTORE_VALIDATION_FAILED, "cannot remove exact restore validation name", err)
	}
	installedIdentity, err := privateRegularIdentity(candidate, 1)
	if err != nil || installedIdentity != candidateBinding.Identity || entryMatches(directory, controllerdb.FileName, installedIdentity, false) != nil {
		return backupError(farmerr.RESTORE_VALIDATION_FAILED, "installed restore object identity changed", err)
	}
	if hadLive {
		if err := validateNamedDatabaseBinding(ctx, directory, restoreOldFile, transaction.Original, 1); err != nil {
			return backupError(farmerr.RESTORE_VALIDATION_FAILED, "retained original database changed before finalization", err)
		}
		if err := removeBoundRegularEntryAt(directory, restoreOldFile, transaction.Original.Identity); err != nil {
			return backupError(farmerr.RESTORE_VALIDATION_FAILED, "cannot finalize original database retention", err)
		}
	}
	if err := removeBoundRegularEntryAt(directory, restoreMarkerFile, transaction.MarkerIdentity); err != nil {
		return backupError(farmerr.RESTORE_VALIDATION_FAILED, "cannot finalize restore transaction marker", err)
	}
	if err := directory.Sync(); err != nil {
		return backupError(farmerr.RESTORE_VALIDATION_FAILED, "cannot durably finalize restore", err)
	}
	return nil
}

// RecoverInterruptedRestore is called before opening farm.db. It recognizes
// only the fixed M10 transaction names and either restores the original DB,
// accepts a schema-valid barrier-bearing installed DB, or fails closed.
func RecoverInterruptedRestore(ctx context.Context, dataDir string, lease *controllerlock.Lease, controllerID identity.ControllerID, farmID identity.FarmID, trustFingerprint string) error {
	directory, err := lease.OpenDirectory(dataDir)
	if err != nil {
		return err
	}
	defer directory.Close()
	markerExists, err := anyEntryExists(directory, restoreMarkerFile)
	if err != nil {
		return backupError(farmerr.RESTORE_VALIDATION_FAILED, "cannot inspect restore recovery marker", err)
	}
	oldExists, err := anyEntryExists(directory, restoreOldFile)
	if err != nil {
		return backupError(farmerr.RESTORE_VALIDATION_FAILED, "cannot inspect retained Controller database", err)
	}
	if !markerExists && !oldExists {
		return nil
	}
	if !markerExists {
		return backupError(farmerr.RESTORE_VALIDATION_FAILED, "orphan restore recovery state requires operator inspection", nil)
	}
	transaction, err := readRestoreTransactionMarker(directory)
	if err != nil {
		return backupError(farmerr.RESTORE_VALIDATION_FAILED, "restore recovery marker is invalid", err)
	}
	if err := recoverRestoreTransaction(ctx, directory, transaction, controllerID, farmID, trustFingerprint, restoreRecoveryHooks{}); err != nil {
		if errors.Is(err, errRestoreIdentityMismatch) {
			return backupError(farmerr.RESTORE_IDENTITY_MISMATCH, "interrupted restore belongs to a different Controller/Farm trust identity", err)
		}
		return backupError(farmerr.RESTORE_VALIDATION_FAILED, "incomplete restore cannot be recovered safely", err)
	}
	return nil
}

func openOptionalPrivateRegularAt(directory *os.File, name string) (*os.File, bool, error) {
	file, err := openPrivateRegularAt(directory, name)
	if errors.Is(err, unix.ENOENT) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return file, true, nil
}

func linkCountAllowed(actual uint64, allowed []uint64) bool {
	for _, value := range allowed {
		if actual == value {
			return true
		}
	}
	return false
}

func databaseFileMatches(file *os.File, binding restoreFileBinding, links ...uint64) (bool, error) {
	identity, actualLinks, size, err := privateRegularDetails(file)
	if err != nil {
		return false, err
	}
	if identity != binding.Identity || size != binding.Size || !linkCountAllowed(actualLinks, links) {
		return false, nil
	}
	actualSize, checksum, err := checksumFile(file)
	if err != nil {
		return false, err
	}
	return actualSize == binding.Size && checksum == binding.SHA256, nil
}

func validateDatabaseBinding(ctx context.Context, file *os.File, binding restoreFileBinding, links ...uint64) error {
	matches, err := databaseFileMatches(file, binding, links...)
	if err != nil {
		return err
	}
	if !matches {
		return errors.New("database object does not match its durable restore binding")
	}
	db, err := openSQLite(procDescriptorPath(file), true)
	if err != nil {
		return err
	}
	if err := controllerdb.ValidateSchema(ctx, db); err != nil {
		_ = db.Close()
		return err
	}
	if err := integrityCheck(ctx, db); err != nil {
		_ = db.Close()
		return err
	}
	if err := db.Close(); err != nil {
		return err
	}
	matches, err = databaseFileMatches(file, binding, links...)
	if err != nil {
		return err
	}
	if !matches {
		return errors.New("database object changed during restore validation")
	}
	return nil
}

func bindDatabaseFile(ctx context.Context, file *os.File, links uint64) (restoreFileBinding, error) {
	identity, actualLinks, size, err := privateRegularDetails(file)
	if err != nil || actualLinks != links || size <= 0 {
		return restoreFileBinding{}, errors.New("database object cannot be bound")
	}
	actualSize, checksum, err := checksumFile(file)
	if err != nil || actualSize != size || !validSHA256(checksum) {
		return restoreFileBinding{}, errors.New("database object cannot be checksummed")
	}
	binding := restoreFileBinding{Identity: identity, Size: size, SHA256: checksum}
	if err := validateDatabaseBinding(ctx, file, binding, links); err != nil {
		return restoreFileBinding{}, err
	}
	return binding, nil
}

func validateNamedDatabaseBinding(ctx context.Context, directory *os.File, name string, binding restoreFileBinding, links ...uint64) error {
	file, err := openPrivateRegularAt(directory, name)
	if err != nil {
		return err
	}
	defer file.Close()
	return validateDatabaseBinding(ctx, file, binding, links...)
}

func removeRestoreValidationName(directory *os.File, transaction restoreTransaction, requiredLinks uint64) error {
	name := ".restore-exact-" + strings.TrimPrefix(transaction.BackupID.String(), "backup_")
	exists, err := anyEntryExists(directory, name)
	if err != nil || !exists {
		return err
	}
	if err := regularEntryMatchesLinks(directory, name, transaction.Restored.Identity, requiredLinks); err != nil {
		// An unrelated replacement is never removed. It is not authoritative
		// once farm.db itself has positively matched a transaction side.
		return nil
	}
	return removeBoundRegularEntryAt(directory, name, transaction.Restored.Identity)
}

func clearRestoreMarker(directory *os.File, transaction restoreTransaction) error {
	if err := removeBoundRegularEntryAt(directory, restoreMarkerFile, transaction.MarkerIdentity); err != nil {
		return err
	}
	return directory.Sync()
}

func recoverRestoreTransaction(ctx context.Context, directory *os.File, transaction restoreTransaction, expectedController identity.ControllerID, expectedFarm identity.FarmID, expectedTrust string, hooks restoreRecoveryHooks) error {
	if err := validateRestoreTransaction(transaction, true); err != nil {
		return err
	}
	if err := regularEntryMatchesLinks(directory, restoreMarkerFile, transaction.MarkerIdentity, 1); err != nil {
		return errors.New("restore transaction marker no longer names its bound object")
	}
	live, liveExists, err := openOptionalPrivateRegularAt(directory, controllerdb.FileName)
	if err != nil {
		return err
	}
	if live != nil {
		defer live.Close()
	}
	old, oldExists, err := openOptionalPrivateRegularAt(directory, restoreOldFile)
	if err != nil {
		return err
	}
	if old != nil {
		defer old.Close()
	}

	if liveExists {
		restored, err := databaseFileMatches(live, transaction.Restored, 1, 2)
		if err != nil {
			return err
		}
		if restored {
			if _, err := installedRestoreMatches(ctx, live, transaction, expectedController, expectedFarm, expectedTrust, 1, 2); err != nil {
				return err
			}
			_, liveLinks, _, err := privateRegularDetails(live)
			if err != nil {
				return err
			}
			// The authoritative restored name may have been linked immediately
			// before an interruption. Make that name durable before removing any
			// transaction evidence. Cleanup has its own directory sync when the
			// marker is removed below.
			if hooks.beforeRestoredDirectorySync != nil {
				if err := hooks.beforeRestoredDirectorySync(); err != nil {
					return err
				}
			}
			if err := directory.Sync(); err != nil {
				return errors.Join(errors.New("cannot make restored Controller database authoritative before recovery cleanup"), err)
			}
			if hooks.afterRestoredDirectorySync != nil {
				if err := hooks.afterRestoredDirectorySync(); err != nil {
					return err
				}
			}
			if liveLinks == 2 {
				if err := removeRestoreValidationName(directory, transaction, 2); err != nil {
					return errors.New("installed restore has an unrecognized additional link")
				}
			} else {
				name := ".restore-exact-" + strings.TrimPrefix(transaction.BackupID.String(), "backup_")
				if exists, err := anyEntryExists(directory, name); err != nil || exists {
					return errors.New("restore validation entry contradicts installed database identity")
				}
			}
			if hooks.afterRestoredValidationCleanup != nil {
				if err := hooks.afterRestoredValidationCleanup(); err != nil {
					return err
				}
			}
			if oldExists {
				if !transaction.HadLive {
					return errors.New("unexpected retained database exists for an empty restore")
				}
				if err := validateDatabaseBinding(ctx, old, transaction.Original, 1); err != nil {
					return errors.New("retained original database does not match its transaction binding")
				}
				if err := removeBoundRegularEntryAt(directory, restoreOldFile, transaction.Original.Identity); err != nil {
					return err
				}
			}
			if err := validateDatabaseBinding(ctx, live, transaction.Restored, 1); err != nil {
				return err
			}
			// Keep the transaction marker until removal of every other recovery
			// entry is durable. If the marker unlink is interrupted, startup sees
			// either the same transaction again or the completed restored state.
			if err := directory.Sync(); err != nil {
				return errors.Join(errors.New("cannot durably clean restored Controller recovery entries"), err)
			}
			return clearRestoreMarker(directory, transaction)
		}

		if transaction.HadLive {
			original, err := databaseFileMatches(live, transaction.Original, 1, 2)
			if err != nil {
				return err
			}
			if original {
				_, liveLinks, _, err := privateRegularDetails(live)
				if err != nil {
					return err
				}
				if oldExists {
					if liveLinks != 2 {
						return errors.New("retained original database is not the exact second link recorded by the transaction")
					}
					if err := validateDatabaseBinding(ctx, old, transaction.Original, 2); err != nil {
						return errors.New("retained original database is not the exact second link recorded by the transaction")
					}
					if err := regularEntryMatchesLinks(directory, restoreOldFile, transaction.Original.Identity, 2); err != nil {
						return errors.New("retained original database is not the exact second link recorded by the transaction")
					}
					if err := removeBoundRegularEntryAt(directory, restoreOldFile, transaction.Original.Identity); err != nil {
						return err
					}
				} else if liveLinks != 1 {
					return errors.New("original database has an unexplained link")
				}
				if err := removeRestoreValidationName(directory, transaction, 1); err != nil {
					return err
				}
				if err := validateDatabaseBinding(ctx, live, transaction.Original, 1); err != nil {
					return err
				}
				return clearRestoreMarker(directory, transaction)
			}
		}
		return errors.New("authoritative Controller database matches neither side of the restore transaction")
	}

	if !transaction.HadLive {
		if oldExists {
			return errors.New("unexpected retained database exists for an empty restore")
		}
		if err := removeRestoreValidationName(directory, transaction, 1); err != nil {
			return err
		}
		return clearRestoreMarker(directory, transaction)
	}
	if !oldExists {
		return errors.New("restore transaction has neither its original nor restored database")
	}
	if err := validateDatabaseBinding(ctx, old, transaction.Original, 1); err != nil {
		return errors.New("retained original database does not match its durable transaction binding")
	}
	if hooks.beforeOldPromotion != nil {
		if err := hooks.beforeOldPromotion(); err != nil {
			return err
		}
	}
	// The already-open, checksum-validated original inode is selected directly.
	// Namespace replacement of restoreOldFile after validation cannot substitute
	// the source object installed here.
	if err := unix.Linkat(int(old.Fd()), "", int(directory.Fd()), controllerdb.FileName, unix.AT_EMPTY_PATH); err != nil {
		return err
	}
	if err := directory.Sync(); err != nil {
		return err
	}
	if err := validateNamedDatabaseBinding(ctx, directory, controllerdb.FileName, transaction.Original, 1, 2); err != nil {
		return err
	}
	if currentOld, exists, err := openOptionalPrivateRegularAt(directory, restoreOldFile); err != nil {
		return err
	} else if exists {
		defer currentOld.Close()
		matches, err := databaseFileMatches(currentOld, transaction.Original, 2)
		if err != nil || !matches {
			return errors.New("restore-old namespace changed after exact original validation")
		}
		if err := removeBoundRegularEntryAt(directory, restoreOldFile, transaction.Original.Identity); err != nil {
			return err
		}
	}
	if err := removeRestoreValidationName(directory, transaction, 1); err != nil {
		return err
	}
	if err := validateNamedDatabaseBinding(ctx, directory, controllerdb.FileName, transaction.Original, 1); err != nil {
		return err
	}
	return clearRestoreMarker(directory, transaction)
}

func installedRestoreMatches(ctx context.Context, live *os.File, transaction restoreTransaction, expectedController identity.ControllerID, expectedFarm identity.FarmID, expectedTrust string, links ...uint64) (bool, error) {
	if err := validateDatabaseBinding(ctx, live, transaction.Restored, links...); err != nil {
		return false, err
	}
	db, err := openSQLite(procDescriptorPath(live), true)
	if err != nil {
		return false, err
	}
	defer db.Close()
	if err := controllerdb.ValidateSchema(ctx, db); err != nil {
		return false, err
	}
	if err := integrityCheck(ctx, db); err != nil {
		return false, err
	}
	var required bool
	var restoredID, controllerID, farmID, trustFingerprint string
	if err := db.QueryRowContext(ctx, `SELECT restore_required,restore_backup_id,source_controller_id,source_farm_id,source_trust_fingerprint FROM controller_backup_state WHERE singleton=1`).Scan(&required, &restoredID, &controllerID, &farmID, &trustFingerprint); err != nil {
		return false, err
	}
	if restoredID != transaction.BackupID.String() || !validTrustFingerprint(trustFingerprint) {
		return false, errors.New("installed restore provenance is invalid")
	}
	if controllerID != expectedController.String() || farmID != expectedFarm.String() || trustFingerprint != expectedTrust {
		return false, errRestoreIdentityMismatch
	}
	if _, err := identity.ParseControllerID(controllerID); err != nil {
		return false, err
	}
	if _, err := identity.ParseFarmID(farmID); err != nil {
		return false, err
	}
	var expectedHosts, barriers, validBarriers int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM (SELECT host_id FROM desired_workloads UNION SELECT host_id FROM resolved_execution_snapshots UNION SELECT host_id FROM maintenance_holds WHERE active=1)`).Scan(&expectedHosts); err != nil {
		return false, err
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM restore_host_barriers`).Scan(&barriers); err != nil {
		return false, err
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM restore_host_barriers WHERE backup_id=? AND status='PENDING' AND reason_code=? AND host_id IN (SELECT host_id FROM desired_workloads UNION SELECT host_id FROM resolved_execution_snapshots UNION SELECT host_id FROM maintenance_holds WHERE active=1)`, transaction.BackupID.String(), farmerr.RESTORE_RECONCILIATION_REQUIRED).Scan(&validBarriers); err != nil {
		return false, err
	}
	if required != (expectedHosts != 0) || barriers != expectedHosts || validBarriers != expectedHosts {
		return false, errors.New("installed restore Host barriers are incomplete")
	}
	return true, nil
}

func validSHA256(value string) bool {
	if len(value) != sha256.Size*2 || strings.ToLower(value) != value {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}

func validatePreparedRestoreCandidate(ctx context.Context, path string, manifest Manifest, expectedBarriers int) error {
	db, err := openSQLite(path, true)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := controllerdb.ValidateSchema(ctx, db); err != nil {
		return err
	}
	if err := integrityCheck(ctx, db); err != nil {
		return err
	}
	var required bool
	var backupID, controllerID, farmID, trustFingerprint string
	if err := db.QueryRowContext(ctx, `SELECT restore_required,restore_backup_id,source_controller_id,source_farm_id,source_trust_fingerprint FROM controller_backup_state WHERE singleton=1`).Scan(&required, &backupID, &controllerID, &farmID, &trustFingerprint); err != nil {
		return err
	}
	if required != (expectedBarriers != 0) || backupID != manifest.BackupID.String() || controllerID != manifest.ControllerID.String() || farmID != manifest.FarmID.String() || trustFingerprint != manifest.TrustFingerprint {
		return errors.New("restore provenance or global barrier is inconsistent")
	}
	var barrierCount, totalBarriers int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM restore_host_barriers WHERE backup_id=? AND status='PENDING' AND reason_code=?`, manifest.BackupID.String(), farmerr.RESTORE_RECONCILIATION_REQUIRED).Scan(&barrierCount); err != nil {
		return err
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM restore_host_barriers`).Scan(&totalBarriers); err != nil {
		return err
	}
	if barrierCount != expectedBarriers || totalBarriers != expectedBarriers {
		return errors.New("restore Host barriers are incomplete")
	}
	return nil
}

func (manager *Manager) verifyLockedWithoutLock(ctx context.Context, id BackupID) (Manifest, error) {
	if err := id.Validate(); err != nil {
		return Manifest{}, backupError(farmerr.RESTORE_VALIDATION_FAILED, "invalid BackupID", err)
	}
	root, err := manager.openBackupRoot()
	if err != nil {
		return Manifest{}, preserveTypedError(err, backupError(farmerr.PERMISSION_DENIED, "cannot open Controller backup directory", err))
	}
	defer root.Close()
	manifest, artifact, _, err := verifyArtifactAt(ctx, root, id)
	if artifact != nil {
		_ = artifact.Close()
	}
	return manifest, err
}

func (manager *Manager) copyRestoreCandidate(ctx context.Context, id BackupID, manifest Manifest) (string, *os.File, boundFileIdentity, error) {
	root, err := manager.openBackupRoot()
	if err != nil {
		return "", nil, boundFileIdentity{}, preserveTypedError(err, backupError(farmerr.RESTORE_VALIDATION_FAILED, "cannot bind backup root for restore", err))
	}
	defer root.Close()
	verified, artifact, _, err := verifyArtifactAt(ctx, root, id)
	if err != nil || verified != manifest {
		if artifact != nil {
			_ = artifact.Close()
		}
		return "", nil, boundFileIdentity{}, backupError(farmerr.RESTORE_VALIDATION_FAILED, "backup changed before restore copy", err)
	}
	defer artifact.Close()
	source, err := openRegularAt(artifact, DatabaseFile)
	if err != nil {
		return "", nil, boundFileIdentity{}, backupError(farmerr.RESTORE_VALIDATION_FAILED, "backup database is unsafe", err)
	}
	defer source.Close()
	target, err := os.CreateTemp(manager.dataDir, ".restore-candidate-*.db")
	if err != nil {
		return "", nil, boundFileIdentity{}, backupError(farmerr.RESTORE_VALIDATION_FAILED, "cannot prepare restore candidate", err)
	}
	targetPath := target.Name()
	if err := target.Chmod(0600); err != nil {
		target.Close()
		os.Remove(targetPath)
		return "", nil, boundFileIdentity{}, backupError(farmerr.PERMISSION_DENIED, "cannot secure restore candidate", err)
	}
	hash := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(target, hash), source)
	syncErr := target.Sync()
	if err := errors.Join(copyErr, syncErr); err != nil || written != manifest.DatabaseSize || hex.EncodeToString(hash.Sum(nil)) != manifest.DatabaseSHA256 {
		target.Close()
		os.Remove(targetPath)
		return "", nil, boundFileIdentity{}, backupError(farmerr.RESTORE_VALIDATION_FAILED, "backup changed while preparing restore", err)
	}
	identity, err := boundIdentity(target)
	if err != nil {
		target.Close()
		os.Remove(targetPath)
		return "", nil, boundFileIdentity{}, backupError(farmerr.RESTORE_VALIDATION_FAILED, "restore candidate identity is unsafe", err)
	}
	return targetPath, target, identity, nil
}

func prepareRestoreCandidate(ctx context.Context, path string, manifest Manifest, restoredAt time.Time) (int, error) {
	db, err := openSQLite(path, false)
	if err != nil {
		return 0, backupError(farmerr.RESTORE_VALIDATION_FAILED, "cannot open restore candidate", err)
	}
	defer func() {
		if db != nil {
			_ = db.Close()
		}
	}()
	if err := controllerdb.ValidateSchema(ctx, db); err != nil {
		return 0, backupError(farmerr.RESTORE_VALIDATION_FAILED, "restore candidate schema is incompatible", err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, backupError(farmerr.RESTORE_VALIDATION_FAILED, "cannot begin restore preparation", err)
	}
	now := restoredAt.UTC().UnixNano()
	if _, err := tx.ExecContext(ctx, "DELETE FROM restore_host_barriers"); err != nil {
		tx.Rollback()
		return 0, backupError(farmerr.RESTORE_VALIDATION_FAILED, "cannot reset restore barriers", err)
	}
	rows, err := tx.QueryContext(ctx, `SELECT host_id FROM desired_workloads UNION SELECT host_id FROM resolved_execution_snapshots UNION SELECT host_id FROM maintenance_holds WHERE active=1 ORDER BY host_id`)
	if err != nil {
		tx.Rollback()
		return 0, backupError(farmerr.RESTORE_VALIDATION_FAILED, "cannot enumerate restored Hosts", err)
	}
	var hosts []string
	for rows.Next() {
		var host string
		if err := rows.Scan(&host); err != nil {
			rows.Close()
			tx.Rollback()
			return 0, backupError(farmerr.RESTORE_VALIDATION_FAILED, "restored Host identity is malformed", err)
		}
		if _, err := identity.ParseHostID(host); err != nil {
			rows.Close()
			tx.Rollback()
			return 0, backupError(farmerr.RESTORE_VALIDATION_FAILED, "restored Host identity is malformed", err)
		}
		hosts = append(hosts, host)
	}
	if err := rows.Close(); err != nil {
		tx.Rollback()
		return 0, backupError(farmerr.RESTORE_VALIDATION_FAILED, "cannot enumerate restored Hosts", err)
	}
	for _, host := range hosts {
		if _, err := tx.ExecContext(ctx, `INSERT INTO restore_host_barriers(host_id,backup_id,revision,status,reason_code,updated_at_ns) VALUES(?,?,?,?,?,?)`, host, manifest.BackupID.String(), 1, "PENDING", farmerr.RESTORE_RECONCILIATION_REQUIRED, now); err != nil {
			tx.Rollback()
			return 0, backupError(farmerr.RESTORE_VALIDATION_FAILED, "cannot establish restore Host barrier", err)
		}
	}
	required := len(hosts) != 0
	if _, err := tx.ExecContext(ctx, `UPDATE controller_backup_state SET persistent_revision=persistent_revision+1,last_automatic_revision=0,restore_required=?,restore_backup_id=?,source_controller_id=?,source_farm_id=?,source_trust_fingerprint=?,restored_at_ns=?,retention_pending=1,artifact_backup_id='',artifact_class='',artifact_controller_id='',artifact_farm_id='',artifact_trust_fingerprint='',artifact_created_at_ns=0 WHERE singleton=1`, required, manifest.BackupID.String(), manifest.ControllerID.String(), manifest.FarmID.String(), manifest.TrustFingerprint, now); err != nil {
		tx.Rollback()
		return 0, backupError(farmerr.RESTORE_VALIDATION_FAILED, "cannot establish restore safety state", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, backupError(farmerr.RESTORE_VALIDATION_FAILED, "cannot commit restore safety state", err)
	}
	if err := integrityCheck(ctx, db); err != nil {
		return 0, backupError(farmerr.RESTORE_VALIDATION_FAILED, "prepared restore candidate failed integrity validation", err)
	}
	if err := db.Close(); err != nil {
		return 0, backupError(farmerr.RESTORE_VALIDATION_FAILED, "cannot finalize restore candidate", err)
	}
	db = nil
	return len(hosts), nil
}

func verifyArtifactAt(ctx context.Context, root *os.File, expected BackupID) (Manifest, *os.File, fileObjectIdentity, error) {
	artifact, err := openPrivateDirectoryAt(root, expected.String())
	if err != nil {
		return Manifest{}, nil, fileObjectIdentity{}, backupError(farmerr.BACKUP_CORRUPT, "backup artifact directory is unsafe", err)
	}
	identity, err := privateDirectoryIdentity(artifact)
	if err != nil {
		artifact.Close()
		return Manifest{}, nil, fileObjectIdentity{}, backupError(farmerr.BACKUP_CORRUPT, "backup artifact directory is unsafe", err)
	}
	manifest, err := verifyArtifactDirectory(ctx, artifact, expected)
	if err != nil {
		artifact.Close()
		return Manifest{}, nil, fileObjectIdentity{}, err
	}
	return manifest, artifact, identity, nil
}

func verifyArtifactDirectory(ctx context.Context, artifact *os.File, expected BackupID) (Manifest, error) {
	entries, err := directoryNames(artifact)
	if err != nil || len(entries) != 2 || !containsExact(entries, DatabaseFile) || !containsExact(entries, ManifestFile) {
		return Manifest{}, backupError(farmerr.BACKUP_CORRUPT, "backup artifact has unexpected files", err)
	}
	manifestFile, err := openRegularAt(artifact, ManifestFile)
	if err != nil {
		return Manifest{}, backupError(farmerr.BACKUP_CORRUPT, "backup manifest is unsafe", err)
	}
	manifest, parseErr := readManifestFile(manifestFile)
	closeErr := manifestFile.Close()
	err = errors.Join(parseErr, closeErr)
	if err != nil || manifest.BackupID != expected {
		return Manifest{}, backupError(farmerr.BACKUP_CORRUPT, "backup manifest is invalid", err)
	}
	database, err := openRegularAt(artifact, DatabaseFile)
	if err != nil {
		return Manifest{}, backupError(farmerr.BACKUP_CORRUPT, "backup database is unsafe", err)
	}
	defer database.Close()
	size, checksum, err := checksumFile(database)
	if err != nil || size != manifest.DatabaseSize || checksum != manifest.DatabaseSHA256 {
		return Manifest{}, backupError(farmerr.BACKUP_CORRUPT, "backup database checksum does not match manifest", err)
	}
	if manifest.DatabaseSchema != controllerdb.SchemaVersion {
		return Manifest{}, backupError(farmerr.BACKUP_CORRUPT, "backup database schema is unsupported", nil)
	}
	databasePath := procDescriptorPath(database)
	revision, _, _, err := backupStateFile(ctx, databasePath)
	if err != nil || revision != manifest.StateRevision {
		return Manifest{}, backupError(farmerr.BACKUP_CORRUPT, "backup persistent-state revision is invalid", err)
	}
	artifactID, class, controllerID, farmID, trustFingerprint, createdAt, err := backupArtifactProvenance(ctx, databasePath)
	if err != nil || artifactID != manifest.BackupID.String() || class != string(manifest.Class) || controllerID != manifest.ControllerID.String() || farmID != manifest.FarmID.String() || trustFingerprint != manifest.TrustFingerprint || !createdAt.Equal(manifest.CreatedAt) {
		return Manifest{}, backupError(farmerr.BACKUP_CORRUPT, "backup provenance does not match its database", err)
	}
	// Recheck the exact held database after SQLite semantic validation. A
	// concurrent in-place write cannot make one checksum authorize different
	// bytes at publication/verification time.
	finalSize, finalChecksum, finalErr := checksumFile(database)
	if finalErr != nil || finalSize != manifest.DatabaseSize || finalChecksum != manifest.DatabaseSHA256 {
		return Manifest{}, backupError(farmerr.BACKUP_CORRUPT, "backup database changed during verification", finalErr)
	}
	return manifest, nil
}

func readManifestFile(file *os.File) (Manifest, error) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return Manifest{}, err
	}
	reader := bufio.NewReader(io.LimitReader(file, maxManifestBytes+1))
	data, err := io.ReadAll(reader)
	if err != nil || len(data) > maxManifestBytes {
		return Manifest{}, errors.New("manifest exceeds its bound")
	}
	if err := rejectDuplicateManifestFields(data); err != nil {
		return Manifest{}, err
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	var manifest Manifest
	if err := decoder.Decode(&manifest); err != nil {
		return Manifest{}, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return Manifest{}, errors.New("manifest has trailing data")
	}
	if err := validateManifest(manifest); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

func writeManifest(path string, manifest Manifest) error {
	if err := validateManifest(manifest); err != nil {
		return backupError(farmerr.BACKUP_FAILED, "cannot encode invalid backup manifest", err)
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return backupError(farmerr.BACKUP_FAILED, "cannot encode backup manifest", err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return backupError(farmerr.BACKUP_FAILED, "cannot create backup manifest", err)
	}
	_, writeErr := file.Write(append(data, '\n'))
	syncErr := file.Sync()
	closeErr := file.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return backupError(farmerr.BACKUP_FAILED, "cannot persist backup manifest", err)
	}
	return nil
}

func writeManifestAt(directory *os.File, manifest Manifest) error {
	if err := validateManifest(manifest); err != nil {
		return backupError(farmerr.BACKUP_FAILED, "cannot encode invalid backup manifest", err)
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return backupError(farmerr.BACKUP_FAILED, "cannot encode backup manifest", err)
	}
	file, err := createRegularAt(directory, ManifestFile)
	if err != nil {
		return backupError(farmerr.BACKUP_FAILED, "cannot create backup manifest", err)
	}
	_, writeErr := file.Write(append(data, '\n'))
	syncErr := file.Sync()
	closeErr := file.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return backupError(farmerr.BACKUP_FAILED, "cannot persist backup manifest", err)
	}
	return nil
}

func backupState(ctx context.Context, db *sql.DB) (uint64, uint64, bool, error) {
	var revision, last uint64
	var retentionPending bool
	err := db.QueryRowContext(ctx, "SELECT persistent_revision,last_automatic_revision,retention_pending FROM controller_backup_state WHERE singleton=1").Scan(&revision, &last, &retentionPending)
	return revision, last, retentionPending, err
}

func backupStateFile(ctx context.Context, path string) (uint64, uint64, bool, error) {
	db, err := openSQLite(path, true)
	if err != nil {
		return 0, 0, false, err
	}
	defer db.Close()
	if err := integrityCheck(ctx, db); err != nil {
		return 0, 0, false, err
	}
	if err := controllerdb.ValidateSchema(ctx, db); err != nil {
		return 0, 0, false, err
	}
	return backupState(ctx, db)
}

func stampBackupArtifact(ctx context.Context, path string, id BackupID, class Class, controllerID identity.ControllerID, farmID identity.FarmID, trustFingerprint string, createdAt time.Time) error {
	db, err := openSQLite(path, false)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := controllerdb.ValidateSchema(ctx, db); err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, `UPDATE controller_backup_state SET artifact_backup_id=?,artifact_class=?,artifact_controller_id=?,artifact_farm_id=?,artifact_trust_fingerprint=?,artifact_created_at_ns=? WHERE singleton=1`, id.String(), class, controllerID.String(), farmID.String(), trustFingerprint, createdAt.UnixNano())
	return err
}

func backupArtifactProvenance(ctx context.Context, path string) (string, string, string, string, string, time.Time, error) {
	db, err := openSQLite(path, true)
	if err != nil {
		return "", "", "", "", "", time.Time{}, err
	}
	defer db.Close()
	var id, class, controllerID, farmID, trustFingerprint string
	var createdAt int64
	err = db.QueryRowContext(ctx, `SELECT artifact_backup_id,artifact_class,artifact_controller_id,artifact_farm_id,artifact_trust_fingerprint,artifact_created_at_ns FROM controller_backup_state WHERE singleton=1`).Scan(&id, &class, &controllerID, &farmID, &trustFingerprint, &createdAt)
	return id, class, controllerID, farmID, trustFingerprint, time.Unix(0, createdAt).UTC(), err
}

func markAutomaticRevision(ctx context.Context, db *sql.DB, revision uint64) error {
	_, err := db.ExecContext(ctx, `UPDATE controller_backup_state SET last_automatic_revision=CASE WHEN persistent_revision=? THEN ? ELSE last_automatic_revision END,retention_pending=1 WHERE singleton=1`, revision, revision)
	return err
}

func clearRetentionPending(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `UPDATE controller_backup_state SET retention_pending=0 WHERE singleton=1`)
	return err
}

func integrityCheck(ctx context.Context, db *sql.DB) error {
	var result string
	if err := db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&result); err != nil {
		return err
	}
	if result != "ok" {
		return errors.New("SQLite integrity check failed")
	}
	return nil
}

func openSQLite(path string, readOnly bool) (*sql.DB, error) {
	query := "_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)"
	if readOnly {
		query = "mode=ro&immutable=1&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)"
	} else {
		query += "&_pragma=journal_mode(DELETE)&_pragma=synchronous(FULL)"
	}
	dsn := (&url.URL{Scheme: "file", Path: path, RawQuery: query}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func checksumFile(file *os.File) (int64, string, error) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return 0, "", err
	}
	hash := sha256.New()
	size, err := io.Copy(hash, file)
	return size, hex.EncodeToString(hash.Sum(nil)), err
}

type boundFileIdentity struct {
	device uint64
	inode  uint64
}

func boundIdentity(file *os.File) (boundFileIdentity, error) {
	info, err := file.Stat()
	if err != nil {
		return boundFileIdentity{}, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || !ok || stat.Nlink != 1 {
		return boundFileIdentity{}, errors.New("restore candidate must remain one exclusive regular mode-0600 file")
	}
	return boundFileIdentity{device: uint64(stat.Dev), inode: stat.Ino}, nil
}

func validateBoundPath(path string, file *os.File, expected boundFileIdentity) error {
	held, err := boundIdentity(file)
	if err != nil || held != expected {
		return errors.New("restore candidate descriptor identity changed")
	}
	var pathStat syscall.Stat_t
	if err := syscall.Lstat(path, &pathStat); err != nil {
		return err
	}
	if pathStat.Mode&syscall.S_IFMT != syscall.S_IFREG || pathStat.Mode&0777 != 0600 || pathStat.Nlink != 1 {
		return errors.New("restore candidate path is not an exclusive private regular file")
	}
	current := boundFileIdentity{device: uint64(pathStat.Dev), inode: pathStat.Ino}
	if current != expected {
		return errors.New("restore candidate path no longer names the verified inode")
	}
	return nil
}

func ensurePrivateDir(path string) error {
	if err := os.Mkdir(path, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return backupError(farmerr.PERMISSION_DENIED, "cannot create Controller backup directory", err)
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		return backupError(farmerr.PERMISSION_DENIED, "Controller backup directory must be a real mode-0700 directory", err)
	}
	return nil
}

func newBackupID() (BackupID, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return ParseBackupID("backup_" + hex.EncodeToString(random[:]))
}

func backupError(code farmerr.Code, message string, cause error) error {
	details := map[string]string{}
	if cause != nil {
		details["reason"] = "local backup operation failed"
	}
	return farmerr.Error{Code: code, HumanMessage: message, Details: details}
}

func preserveTypedError(cause, fallback error) error {
	if _, ok := farmerr.CodeOf(cause); ok {
		return cause
	}
	return fallback
}
