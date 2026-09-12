//go:build linux

// Package linuxinstall implements the Ubuntu service installation boundary.
// Linux ownership, filesystem layout, and systemd operations intentionally stay here.
package linuxinstall

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

const (
	ServiceUser       = "le0x"
	ControllerService = "le0x-controller.service"
	AgentService      = "le0x-agent.service"
	maxBinaryBytes    = 512 << 20
)

type Role string

const (
	RoleController Role = "controller"
	RoleAgent      Role = "agent"
)

type Account struct {
	UID  int
	GID  int
	GIDs []int
}

type Host interface {
	EnsureServiceAccount(context.Context) (Account, error)
	EnsureGPUAccess(context.Context, Account) error
	SetOwner(string, int, int) error
	ServiceState(context.Context, string) (ServiceState, error)
	Systemctl(context.Context, ...string) error
}

type ServiceState struct {
	Enabled bool
	Active  bool
}

type Config struct {
	Root             string
	Roles            []Role
	ControllerBinary string
	AgentBinary      string
	Enable           bool
	Start            bool
	Host             Host
}

//go:embed systemd/*.service
var units embed.FS

type Installer struct {
	config Config
	root   string
	roles  []Role
	// afterPublish is a deterministic test boundary after each artifact is
	// durably published and before the installation transaction can commit.
	afterPublish func(string) error
	// beforeArtifactRestore is the exact rollback boundary after an artifact
	// rollback copy exists and before it is used to restore the target.
	beforeArtifactRestore func(target, backup string) error
	// afterServiceAction is a deterministic test boundary after systemd has
	// accepted a mutable enable/start action and before the transaction proceeds.
	afterServiceAction func(action, service string) error
}

type artifactRollbackState uint8

const (
	artifactPrepared artifactRollbackState = iota
	artifactPublished
	artifactRestored
	artifactRestoreFailed
)

type artifactBackup struct {
	target  string
	backup  string
	existed bool
	state   artifactRollbackState
}

type serviceRollback struct {
	service         string
	before          ServiceState
	enableAttempted bool
	startAttempted  bool
}

type installTransaction struct {
	installer *Installer
	artifacts []artifactBackup
	services  []serviceRollback
}

func New(config Config) (*Installer, error) {
	if config.Root == "" || config.Host == nil || config.Start && !config.Enable {
		return nil, errors.New("installer requires an absolute root, Host backend, and --enable before --start")
	}
	root, err := filepath.Abs(config.Root)
	if err != nil || root != filepath.Clean(config.Root) {
		return nil, errors.New("installer root must be an absolute clean path")
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("installer root must be an existing real directory")
	}
	seen := map[Role]bool{}
	roles := make([]Role, 0, len(config.Roles))
	for _, role := range config.Roles {
		if role != RoleController && role != RoleAgent {
			return nil, fmt.Errorf("unsupported install role %q", role)
		}
		if !seen[role] {
			seen[role] = true
			roles = append(roles, role)
		}
	}
	if len(roles) == 0 {
		return nil, errors.New("at least one install role is required")
	}
	config.Root = root
	return &Installer{config: config, root: root, roles: roles}, nil
}

func (installer *Installer) Install(ctx context.Context) error {
	for _, role := range installer.roles {
		source := installer.config.AgentBinary
		if role == RoleController {
			source = installer.config.ControllerBinary
		}
		if source == "" {
			return errors.New("each selected role requires its source binary")
		}
		if err := validateBinarySource(source); err != nil {
			return err
		}
	}
	account, err := installer.config.Host.EnsureServiceAccount(ctx)
	if err != nil || account.UID < 0 || account.GID < 0 {
		return errors.Join(errors.New("cannot establish le0x service account"), err)
	}
	if slicesContains(installer.roles, RoleAgent) {
		if err := installer.config.Host.EnsureGPUAccess(ctx, account); err != nil {
			return fmt.Errorf("cannot establish bounded Agent GPU access: %w", err)
		}
	}
	for _, dir := range []struct {
		path string
		mode os.FileMode
	}{{"usr", 0755}, {"usr/local", 0755}, {"usr/local/bin", 0755}, {"etc", 0755}, {"etc/le0xfarm", 0750}, {"etc/systemd", 0755}, {"etc/systemd/system", 0755}, {"var", 0755}, {"var/lib", 0755}, {"var/lib/le0xfarm", 0755}} {
		reconcileMode := dir.path == "etc/le0xfarm" || dir.path == "var/lib/le0xfarm"
		if err := installer.ensureDirectory(dir.path, dir.mode, reconcileMode); err != nil {
			return err
		}
	}
	if err := installer.config.Host.SetOwner(installer.path("etc/le0xfarm"), 0, account.GID); err != nil {
		return err
	}
	transaction, err := installer.beginInstallTransaction(ctx, account)
	if err != nil {
		return err
	}
	abort := func(cause error) error {
		return errors.Join(cause, transaction.rollback(ctx))
	}
	for _, role := range installer.roles {
		if err := installer.installRole(transaction, role, account); err != nil {
			return abort(err)
		}
	}
	if err := installer.config.Host.Systemctl(ctx, "daemon-reload"); err != nil {
		return abort(err)
	}
	for _, role := range installer.roles {
		service := serviceName(role)
		if installer.config.Enable {
			if err := transaction.performServiceAction(ctx, "enable", service); err != nil {
				return abort(err)
			}
		}
		if installer.config.Start {
			if err := transaction.performServiceAction(ctx, "start", service); err != nil {
				return abort(err)
			}
		}
	}
	return transaction.commit()
}

func (installer *Installer) beginInstallTransaction(ctx context.Context, account Account) (*installTransaction, error) {
	transaction := &installTransaction{installer: installer}
	if installer.config.Enable {
		for _, role := range installer.roles {
			service := serviceName(role)
			state, err := installer.config.Host.ServiceState(ctx, service)
			if err != nil {
				return nil, errors.Join(err, transaction.cleanupBackups())
			}
			transaction.services = append(transaction.services, serviceRollback{service: service, before: state})
		}
	}
	for _, role := range installer.roles {
		configPath := installer.path("etc/le0xfarm/" + string(role) + ".env")
		if err := validatePreservedConfiguration(configPath, 0640); err != nil {
			return nil, errors.Join(err, transaction.cleanupBackups())
		}
		for _, target := range []string{
			installer.path("usr/local/bin/le0x-" + string(role)),
			installer.path("etc/systemd/system/" + serviceName(role)),
			configPath,
		} {
			backup, err := installer.captureArtifact(target, account)
			if err != nil {
				return nil, errors.Join(err, transaction.cleanupBackups())
			}
			transaction.artifacts = append(transaction.artifacts, backup)
		}
	}
	return transaction, nil
}

func validatePreservedConfiguration(target string, mode os.FileMode) error {
	info, err := os.Lstat(target)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || stat.Nlink != 1 || info.Mode().Perm() != mode {
		return errors.New("installer refuses unsafe existing configuration: " + target)
	}
	return nil
}

func (installer *Installer) captureArtifact(target string, account Account) (artifactBackup, error) {
	result := artifactBackup{target: target}
	info, err := os.Lstat(target)
	if errors.Is(err, os.ErrNotExist) {
		return result, nil
	}
	if err != nil {
		return result, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || stat.Nlink != 1 || info.Size() < 0 || info.Size() > maxBinaryBytes {
		return result, errors.New("installer refuses unsafe existing artifact: " + target)
	}
	sourceFD, err := unix.Open(target, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return result, err
	}
	source := os.NewFile(uintptr(sourceFD), target)
	defer source.Close()
	opened, err := source.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return result, errors.New("installed artifact changed while preparing rollback")
	}
	backup, err := os.CreateTemp(filepath.Dir(target), ".le0x-rollback-"+filepath.Base(target)+"-")
	if err != nil {
		return result, err
	}
	backupName := backup.Name()
	cleanup := true
	defer func() {
		_ = backup.Close()
		if cleanup {
			_ = os.Remove(backupName)
		}
	}()
	if err := backup.Chmod(info.Mode().Perm()); err != nil {
		return result, err
	}
	written, err := io.Copy(backup, io.LimitReader(source, maxBinaryBytes+1))
	if err != nil || written != info.Size() {
		if err == nil {
			err = errors.New("installed artifact changed while preparing rollback")
		}
		return result, err
	}
	if err := installer.config.Host.SetOwner(backupName, int(stat.Uid), int(stat.Gid)); err != nil {
		return result, err
	}
	if err := backup.Sync(); err != nil {
		return result, err
	}
	if err := backup.Close(); err != nil {
		return result, err
	}
	if err := syncDirectory(filepath.Dir(target)); err != nil {
		return result, err
	}
	cleanup = false
	result.backup, result.existed = backupName, true
	return result, nil
}

func (transaction *installTransaction) markPublished(target string) {
	for index := range transaction.artifacts {
		if transaction.artifacts[index].target == target {
			transaction.artifacts[index].state = artifactPublished
			return
		}
	}
}

func (transaction *installTransaction) markServiceAction(service, action string) {
	for index := range transaction.services {
		if transaction.services[index].service != service {
			continue
		}
		if action == "enable" {
			transaction.services[index].enableAttempted = true
		} else if action == "start" {
			transaction.services[index].startAttempted = true
		}
		return
	}
}

func (transaction *installTransaction) performServiceAction(ctx context.Context, action, service string) error {
	transaction.markServiceAction(service, action)
	if err := transaction.installer.config.Host.Systemctl(ctx, action, service); err != nil {
		return err
	}
	if transaction.installer.afterServiceAction != nil {
		return transaction.installer.afterServiceAction(action, service)
	}
	return nil
}

func (transaction *installTransaction) rollback(ctx context.Context) error {
	var result error
	stopFailed := false
	for index := len(transaction.services) - 1; index >= 0; index-- {
		service := &transaction.services[index]
		if service.startAttempted && !service.before.Active {
			if err := transaction.installer.config.Host.Systemctl(ctx, "stop", service.service); err != nil {
				stopFailed = true
				result = errors.Join(result, fmt.Errorf("cannot stop transaction-started %s before artifact rollback: %w", service.service, err))
			}
		}
	}
	if stopFailed {
		result = errors.Join(result, transaction.restoreEnablement(ctx))
		return errors.Join(result, errors.New("artifact rollback withheld; rollback copies retained for manual recovery"))
	}

	artifactsRestored := true
	for index := len(transaction.artifacts) - 1; index >= 0; index-- {
		artifact := &transaction.artifacts[index]
		if artifact.state != artifactPublished {
			continue
		}
		if transaction.installer.beforeArtifactRestore != nil {
			if err := transaction.installer.beforeArtifactRestore(artifact.target, artifact.backup); err != nil {
				artifact.state = artifactRestoreFailed
				artifactsRestored = false
				result = errors.Join(result, artifactRollbackError(*artifact, err))
				continue
			}
		}
		var err error
		if artifact.existed {
			err = transaction.installer.restoreArtifact(*artifact)
		} else {
			err = os.Remove(artifact.target)
			if errors.Is(err, os.ErrNotExist) {
				err = nil
			}
			if err == nil {
				err = syncDirectory(filepath.Dir(artifact.target))
			}
		}
		if err != nil {
			artifact.state = artifactRestoreFailed
			artifactsRestored = false
			result = errors.Join(result, artifactRollbackError(*artifact, err))
			continue
		}
		artifact.state = artifactRestored
	}

	reloadOK := true
	if transaction.hasPublishedArtifacts() {
		if err := transaction.installer.config.Host.Systemctl(ctx, "daemon-reload"); err != nil {
			reloadOK = false
			result = errors.Join(result, fmt.Errorf("cannot reload restored systemd units: %w", err))
		}
	}
	result = errors.Join(result, transaction.restoreEnablement(ctx))
	if artifactsRestored && reloadOK {
		result = errors.Join(result, transaction.restoreActiveState(ctx))
	}
	if result != nil {
		return errors.Join(result, errors.New("installation rollback incomplete; rollback copies retained for manual recovery"))
	}
	return transaction.cleanupBackups()
}

func artifactRollbackError(artifact artifactBackup, err error) error {
	if artifact.backup == "" {
		return fmt.Errorf("cannot remove newly published %s during rollback: %w", artifact.target, err)
	}
	return fmt.Errorf("cannot restore %s; rollback copy retained at %s: %w", artifact.target, artifact.backup, err)
}

func (transaction *installTransaction) hasPublishedArtifacts() bool {
	for _, artifact := range transaction.artifacts {
		if artifact.state != artifactPrepared {
			return true
		}
	}
	return false
}

func (transaction *installTransaction) restoreEnablement(ctx context.Context) error {
	var result error
	for index := len(transaction.services) - 1; index >= 0; index-- {
		service := transaction.services[index]
		if !service.enableAttempted {
			continue
		}
		action := "disable"
		if service.before.Enabled {
			action = "enable"
		}
		if err := transaction.installer.config.Host.Systemctl(ctx, action, service.service); err != nil {
			result = errors.Join(result, fmt.Errorf("cannot restore %s enablement: %w", service.service, err))
		}
	}
	return result
}

func (transaction *installTransaction) restoreActiveState(ctx context.Context) error {
	var result error
	for index := len(transaction.services) - 1; index >= 0; index-- {
		service := transaction.services[index]
		if !service.startAttempted {
			continue
		}
		action := "stop"
		if service.before.Active {
			action = "start"
		}
		if err := transaction.installer.config.Host.Systemctl(ctx, action, service.service); err != nil {
			result = errors.Join(result, fmt.Errorf("cannot restore %s active state: %w", service.service, err))
		}
	}
	return result
}

func (installer *Installer) restoreArtifact(artifact artifactBackup) error {
	info, err := os.Lstat(artifact.backup)
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || stat.Nlink != 1 || info.Size() < 0 || info.Size() > maxBinaryBytes {
		return errors.New("rollback copy is not a bounded single-link regular file")
	}
	fd, err := unix.Open(artifact.backup, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	source := os.NewFile(uintptr(fd), artifact.backup)
	defer source.Close()
	opened, err := source.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return errors.New("rollback copy changed while being opened")
	}
	return installer.replaceFrom(artifact.target, io.LimitReader(source, maxBinaryBytes+1), info.Size(), info.Mode().Perm(), int(stat.Uid), int(stat.Gid), nil)
}

func (transaction *installTransaction) commit() error { return transaction.cleanupBackups() }

func (transaction *installTransaction) cleanupBackups() error {
	var result error
	directories := make(map[string]struct{})
	for index := range transaction.artifacts {
		artifact := &transaction.artifacts[index]
		if artifact.backup == "" {
			continue
		}
		if err := os.Remove(artifact.backup); err != nil && !errors.Is(err, os.ErrNotExist) {
			result = errors.Join(result, err)
			continue
		}
		directories[filepath.Dir(artifact.backup)] = struct{}{}
		artifact.backup = ""
	}
	for directory := range directories {
		result = errors.Join(result, syncDirectory(directory))
	}
	return result
}

func (installer *Installer) installRole(transaction *installTransaction, role Role, account Account) error {
	state := "var/lib/le0xfarm/" + string(role)
	if err := installer.ensureDirectory(state, 0700, true); err != nil {
		return err
	}
	if err := installer.config.Host.SetOwner(installer.path(state), account.UID, account.GID); err != nil {
		return err
	}
	var source, binary, environment string
	switch role {
	case RoleController:
		source, binary = installer.config.ControllerBinary, "le0x-controller"
		environment = "LE0X_CONTROLLER_LISTEN=127.0.0.1:50051\n"
	case RoleAgent:
		source, binary = installer.config.AgentBinary, "le0x-agent"
		environment = "LE0X_AGENT_CONTROLLER=127.0.0.1:50051\n"
	}
	binaryTarget := installer.path("usr/local/bin/" + binary)
	if err := installer.installBinary(source, binaryTarget, transaction.markPublished); err != nil {
		return err
	}
	unit, err := units.ReadFile("systemd/" + serviceName(role))
	if err != nil {
		return err
	}
	unitTarget := installer.path("etc/systemd/system/" + serviceName(role))
	if err := installer.replaceFile(unitTarget, unit, 0644, 0, 0, transaction.markPublished); err != nil {
		return err
	}
	configTarget := installer.path("etc/le0xfarm/" + string(role) + ".env")
	return installer.createPreservedFile(configTarget, []byte(environment), 0640, 0, account.GID, transaction.markPublished)
}

func (installer *Installer) Uninstall(ctx context.Context) error {
	for _, role := range installer.roles {
		service := serviceName(role)
		if err := installer.config.Host.Systemctl(ctx, "disable", "--now", service); err != nil {
			return err
		}
		if err := installer.removeInfrastructure("etc/systemd/system/" + service); err != nil {
			return err
		}
		if err := installer.removeInfrastructure("usr/local/bin/le0x-" + string(role)); err != nil {
			return err
		}
	}
	if err := installer.config.Host.Systemctl(ctx, "daemon-reload"); err != nil {
		return err
	}
	for _, role := range installer.roles {
		if err := installer.config.Host.Systemctl(ctx, "reset-failed", serviceName(role)); err != nil {
			return err
		}
	}
	return nil
}

func (installer *Installer) path(relative string) string {
	return filepath.Join(installer.root, filepath.FromSlash(relative))
}

func (installer *Installer) ensureDirectory(relative string, mode os.FileMode, reconcileMode bool) error {
	path := installer.path(relative)
	created := false
	if err := os.Mkdir(path, mode); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	} else if err == nil {
		created = true
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("installer directory is not a real directory: " + path)
	}
	if (created || reconcileMode) && info.Mode().Perm() != mode {
		if err := os.Chmod(path, mode); err != nil {
			return err
		}
	}
	return nil
}

func (installer *Installer) installBinary(source, target string, published func(string)) error {
	if err := validateBinarySource(source); err != nil {
		return err
	}
	info, err := os.Lstat(source)
	if err != nil {
		return err
	}
	fd, err := unix.Open(source, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), source)
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) || opened.Size() != info.Size() {
		return errors.New("source binary changed while being opened")
	}
	return installer.replaceFrom(target, io.LimitReader(file, maxBinaryBytes+1), info.Size(), 0755, 0, 0, published)
}

func validateBinarySource(source string) error {
	info, err := os.Lstat(source)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0111 == 0 || info.Size() <= 0 || info.Size() > maxBinaryBytes {
		return errors.New("source binary must be a bounded regular executable file")
	}
	return nil
}

func (installer *Installer) replaceFile(target string, data []byte, mode os.FileMode, uid, gid int, published func(string)) error {
	return installer.replaceFrom(target, strings.NewReader(string(data)), int64(len(data)), mode, uid, gid, published)
}

func (installer *Installer) replaceFrom(target string, source io.Reader, expected int64, mode os.FileMode, uid, gid int, published func(string)) error {
	if info, err := os.Lstat(target); err == nil {
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || !info.Mode().IsRegular() || stat.Nlink != 1 {
			return errors.New("installer refuses to replace an unsafe destination: " + target)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(target), ".le0x-install-")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	defer func() {
		if tempName != "" {
			_ = os.Remove(tempName)
		}
	}()
	if err := temp.Chmod(mode); err != nil {
		temp.Close()
		return err
	}
	written, copyErr := io.Copy(temp, source)
	if copyErr == nil && written != expected {
		copyErr = errors.New("installer source changed while being copied")
	}
	if copyErr == nil {
		copyErr = installer.config.Host.SetOwner(tempName, uid, gid)
	}
	if copyErr == nil {
		copyErr = temp.Sync()
	}
	closeErr := temp.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		return err
	}
	if err := os.Rename(tempName, target); err != nil {
		return err
	}
	tempName = ""
	if published != nil {
		published(target)
	}
	if err := syncDirectory(filepath.Dir(target)); err != nil {
		return err
	}
	if published != nil && installer.afterPublish != nil {
		return installer.afterPublish(target)
	}
	return nil
}

func (installer *Installer) createPreservedFile(target string, data []byte, mode os.FileMode, uid, gid int, published func(string)) error {
	if info, err := os.Lstat(target); err == nil {
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || !info.Mode().IsRegular() || stat.Nlink != 1 {
			return errors.New("installer refuses unsafe existing configuration: " + target)
		}
		if info.Mode().Perm() != mode {
			return errors.New("installer refuses existing configuration with unsafe permissions: " + target)
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(target), ".le0x-config-")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	defer func() {
		if tempName != "" {
			_ = os.Remove(tempName)
		}
	}()
	if err := temp.Chmod(mode); err != nil {
		temp.Close()
		return err
	}
	_, writeErr := temp.Write(data)
	if writeErr == nil {
		writeErr = installer.config.Host.SetOwner(tempName, uid, gid)
	}
	if writeErr == nil {
		writeErr = temp.Sync()
	}
	closeErr := temp.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return err
	}
	if err := os.Link(tempName, target); err != nil {
		if errors.Is(err, os.ErrExist) {
			return installer.createPreservedFile(target, data, mode, uid, gid, published)
		}
		return err
	}
	if err := os.Remove(tempName); err != nil {
		return err
	}
	tempName = ""
	if published != nil {
		published(target)
	}
	if err := syncDirectory(filepath.Dir(target)); err != nil {
		return err
	}
	if published != nil && installer.afterPublish != nil {
		return installer.afterPublish(target)
	}
	return nil
}

func (installer *Installer) removeInfrastructure(relative string) error {
	path := installer.path(relative)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || stat.Nlink != 1 {
		return errors.New("installer refuses to remove unsafe infrastructure: " + path)
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func serviceName(role Role) string {
	if role == RoleController {
		return ControllerService
	}
	return AgentService
}

func slicesContains(values []Role, want Role) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

type gpuDevicePermission struct {
	path       string
	uid, gid   int
	permission os.FileMode
}

func discoverGPUDevices(root string) ([]gpuDevicePermission, error) {
	patterns := []string{
		filepath.Join(root, "dri", "renderD*"),
		filepath.Join(root, "dri", "card*"),
		filepath.Join(root, "nvidia*"),
	}
	seen := make(map[string]struct{})
	var result []gpuDevicePermission
	for _, pattern := range patterns {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			return nil, err
		}
		for _, path := range matches {
			if _, ok := seen[path]; ok || !supportedGPUDeviceName(root, path) {
				continue
			}
			seen[path] = struct{}{}
			info, err := os.Lstat(path)
			if err != nil {
				return nil, err
			}
			stat, ok := info.Sys().(*syscall.Stat_t)
			if !ok || info.Mode()&os.ModeDevice == 0 || info.Mode()&os.ModeCharDevice == 0 {
				return nil, fmt.Errorf("GPU runtime path is not a character device: %s", path)
			}
			result = append(result, gpuDevicePermission{path: path, uid: int(stat.Uid), gid: int(stat.Gid), permission: info.Mode().Perm()})
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].path < result[j].path })
	return result, nil
}

func supportedGPUDeviceName(root, path string) bool {
	base := filepath.Base(path)
	if filepath.Dir(path) == filepath.Join(root, "dri") {
		return numericSuffix(base, "renderD") || numericSuffix(base, "card")
	}
	switch base {
	case "nvidiactl", "nvidia-uvm", "nvidia-uvm-tools", "nvidia-modeset":
		return true
	default:
		return numericSuffix(base, "nvidia")
	}
}

func numericSuffix(value, prefix string) bool {
	suffix := strings.TrimPrefix(value, prefix)
	if suffix == "" || suffix == value {
		return false
	}
	for _, character := range suffix {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func planGPUAccess(account Account, devices []gpuDevicePermission, groupNames map[int]string) ([]string, error) {
	groups := make(map[int]struct{}, len(account.GIDs)+1)
	groups[account.GID] = struct{}{}
	for _, gid := range account.GIDs {
		groups[gid] = struct{}{}
	}
	planned := make(map[string]struct{})
	for _, device := range devices {
		if device.uid == account.UID && device.permission&0600 == 0600 || device.permission&0006 == 0006 {
			continue
		}
		if _, ok := groups[device.gid]; ok && device.permission&0060 == 0060 {
			continue
		}
		group := groupNames[device.gid]
		if (group != "render" && group != "video") || device.permission&0060 != 0060 {
			return nil, fmt.Errorf("GPU device %s is not accessible to le0x through a bounded render/video group", device.path)
		}
		planned[group] = struct{}{}
		groups[device.gid] = struct{}{}
	}
	result := make([]string, 0, len(planned))
	for group := range planned {
		result = append(result, group)
	}
	sort.Strings(result)
	return result, nil
}

// LocalHost performs only fixed Ubuntu account and systemd operations. It never invokes a shell.
type LocalHost struct{}

func (LocalHost) EnsureGPUAccess(ctx context.Context, account Account) error {
	devices, err := discoverGPUDevices("/dev")
	if err != nil || len(devices) == 0 {
		return err
	}
	groupNames := make(map[int]string)
	for _, device := range devices {
		if _, exists := groupNames[device.gid]; exists {
			continue
		}
		group, lookupErr := user.LookupGroupId(strconv.Itoa(device.gid))
		if lookupErr == nil && (group.Name == "render" || group.Name == "video") {
			groupNames[device.gid] = group.Name
		}
	}
	groups, err := planGPUAccess(account, devices, groupNames)
	if err != nil || len(groups) == 0 {
		return err
	}
	command := exec.CommandContext(ctx, "/usr/sbin/usermod", "--append", "--groups", strings.Join(groups, ","), ServiceUser)
	if err := command.Run(); err != nil {
		return fmt.Errorf("cannot add le0x to required GPU access groups: %w", err)
	}
	return nil
}

func (LocalHost) EnsureServiceAccount(ctx context.Context) (Account, error) {
	entry, err := lookupServiceAccount(ctx)
	if err == nil {
		return entry, nil
	}
	var unknown user.UnknownUserError
	if !errors.As(err, &unknown) {
		return Account{}, err
	}
	if os.Geteuid() != 0 {
		return Account{}, errors.New("installation under / requires root")
	}
	command := exec.CommandContext(ctx, "/usr/sbin/useradd", "--system", "--user-group", "--home-dir", "/nonexistent", "--no-create-home", "--shell", "/usr/sbin/nologin", ServiceUser)
	if runErr := command.Run(); runErr != nil {
		return Account{}, fmt.Errorf("useradd failed: %w", runErr)
	}
	return lookupServiceAccount(ctx)
}

func lookupServiceAccount(ctx context.Context) (Account, error) {
	account, err := user.Lookup(ServiceUser)
	if err != nil {
		return Account{}, err
	}
	uid, err := strconv.Atoi(account.Uid)
	if err != nil {
		return Account{}, err
	}
	gid, err := strconv.Atoi(account.Gid)
	if err != nil {
		return Account{}, err
	}
	groupIDs, err := account.GroupIds()
	if err != nil {
		return Account{}, err
	}
	gids := make([]int, 0, len(groupIDs))
	for _, value := range groupIDs {
		parsed, parseErr := strconv.Atoi(value)
		if parseErr != nil {
			return Account{}, parseErr
		}
		gids = append(gids, parsed)
	}
	group, err := user.LookupGroupId(account.Gid)
	if err != nil {
		return Account{}, err
	}
	output, err := exec.CommandContext(ctx, "/usr/bin/getent", "passwd", ServiceUser).Output()
	if err != nil {
		return Account{}, err
	}
	fields := strings.Split(strings.TrimSpace(string(output)), ":")
	shadow, err := exec.CommandContext(ctx, "/usr/bin/getent", "shadow", ServiceUser).Output()
	if err != nil {
		return Account{}, errors.New("cannot verify that the existing le0x account password is locked")
	}
	shadowFields := strings.Split(strings.TrimSpace(string(shadow)), ":")
	minimum, maximum, err := configuredSystemUIDRange("/etc/login.defs")
	if err != nil {
		return Account{}, err
	}
	if err := validateServiceAccountRecord(account.Uid, account.Gid, group.Name, fields, shadowFields, minimum, maximum); err != nil {
		return Account{}, err
	}
	return Account{UID: uid, GID: gid, GIDs: gids}, nil
}

func validateServiceAccountRecord(uidValue, gidValue, primaryGroup string, passwdFields, shadowFields []string, minimum, maximum int) error {
	uid, err := strconv.Atoi(uidValue)
	if err != nil || primaryGroup != ServiceUser {
		return errors.New("existing le0x account does not have a dedicated le0x primary group")
	}
	if len(passwdFields) != 7 || passwdFields[0] != ServiceUser || passwdFields[2] != uidValue || passwdFields[3] != gidValue || passwdFields[5] != "/nonexistent" || passwdFields[6] != "/usr/sbin/nologin" && passwdFields[6] != "/sbin/nologin" && passwdFields[6] != "/bin/false" {
		return errors.New("existing le0x account is not a no-login system account")
	}
	if len(shadowFields) < 2 || shadowFields[0] != ServiceUser || (!strings.HasPrefix(shadowFields[1], "!") && !strings.HasPrefix(shadowFields[1], "*")) {
		return errors.New("existing le0x account does not have a locked password")
	}
	if uid < minimum || uid > maximum {
		return errors.New("existing le0x account is outside the configured system UID range")
	}
	return nil
}

func configuredSystemUIDRange(path string) (int, int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, err
	}
	values := map[string]int{}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(strings.SplitN(line, "#", 2)[0])
		if len(fields) != 2 || (fields[0] != "SYS_UID_MIN" && fields[0] != "SYS_UID_MAX" && fields[0] != "UID_MIN") {
			continue
		}
		value, parseErr := strconv.Atoi(fields[1])
		if parseErr != nil || value < 0 {
			return 0, 0, errors.New("invalid system UID range")
		}
		values[fields[0]] = value
	}
	minimum, minOK := values["SYS_UID_MIN"]
	maximum, maxOK := values["SYS_UID_MAX"]
	if !minOK && !maxOK {
		if regularMinimum, ok := values["UID_MIN"]; ok && regularMinimum > 1 {
			minimum, maximum, minOK, maxOK = 1, regularMinimum-1, true, true
		}
	}
	if !minOK || !maxOK || minimum > maximum {
		return 0, 0, errors.New("system UID range is unavailable")
	}
	return minimum, maximum, nil
}

func (LocalHost) SetOwner(path string, uid, gid int) error { return os.Chown(path, uid, gid) }

func (LocalHost) ServiceState(ctx context.Context, service string) (ServiceState, error) {
	if service != ControllerService && service != AgentService {
		return ServiceState{}, errors.New("refusing service-state query for an unsupported unit")
	}
	enabled, err := querySystemctlState(ctx, "is-enabled", service, map[string]bool{
		"enabled": true, "enabled-runtime": true, "disabled": false, "not-found": false,
	})
	if err != nil {
		return ServiceState{}, err
	}
	active, err := querySystemctlState(ctx, "is-active", service, map[string]bool{
		"active": true, "inactive": false, "failed": false, "unknown": false,
	})
	if err != nil {
		return ServiceState{}, err
	}
	return ServiceState{Enabled: enabled, Active: active}, nil
}

func querySystemctlState(ctx context.Context, action, service string, accepted map[string]bool) (bool, error) {
	command := exec.CommandContext(ctx, "/usr/bin/systemctl", action, service)
	command.Env = append(os.Environ(), "LC_ALL=C")
	output, runErr := command.Output()
	state := strings.TrimSpace(string(output))
	value, ok := accepted[state]
	if !ok {
		return false, fmt.Errorf("cannot establish prior %s state for %s: %w", action, service, runErr)
	}
	return value, nil
}

func (LocalHost) Systemctl(ctx context.Context, args ...string) error {
	if !validSystemctl(args) {
		return errors.New("refusing unsupported systemctl operation")
	}
	command := exec.CommandContext(ctx, "/usr/bin/systemctl", args...)
	if err := command.Run(); err != nil {
		return fmt.Errorf("systemctl failed: %w", err)
	}
	return nil
}

func validSystemctl(args []string) bool {
	if len(args) == 1 && args[0] == "daemon-reload" {
		return true
	}
	if len(args) == 2 && (args[0] == "enable" || args[0] == "disable" || args[0] == "start" || args[0] == "stop" || args[0] == "reset-failed") {
		return args[1] == ControllerService || args[1] == AgentService
	}
	return len(args) == 3 && args[0] == "disable" && args[1] == "--now" && (args[2] == ControllerService || args[2] == AgentService)
}

// StagingHost applies no host account, ownership, or service-manager changes.
// It exists only for an explicit non-/ destination root used by packaging/tests.
type StagingHost struct {
	Actions []string
	States  map[string]ServiceState
}

func (host *StagingHost) EnsureServiceAccount(context.Context) (Account, error) {
	host.Actions = append(host.Actions, "ensure-account:"+ServiceUser)
	return Account{UID: os.Geteuid(), GID: os.Getegid()}, nil
}

func (host *StagingHost) EnsureGPUAccess(context.Context, Account) error {
	host.Actions = append(host.Actions, "gpu-access-preflight")
	return nil
}

func (host *StagingHost) SetOwner(path string, _, _ int) error {
	host.Actions = append(host.Actions, "owner:"+path)
	return nil
}

func (host *StagingHost) ServiceState(_ context.Context, service string) (ServiceState, error) {
	if service != ControllerService && service != AgentService {
		return ServiceState{}, errors.New("refusing staged service-state query for an unsupported unit")
	}
	host.Actions = append(host.Actions, "systemctl-state:"+service)
	return host.States[service], nil
}

func (host *StagingHost) Systemctl(_ context.Context, args ...string) error {
	if !validSystemctl(args) {
		return errors.New("refusing unsupported staged systemctl operation")
	}
	host.Actions = append(host.Actions, "systemctl:"+strings.Join(args, " "))
	if host.States == nil {
		host.States = make(map[string]ServiceState)
	}
	var service string
	if len(args) >= 2 {
		service = args[len(args)-1]
	}
	state := host.States[service]
	switch args[0] {
	case "enable":
		state.Enabled = true
	case "disable":
		state.Enabled = false
		if len(args) == 3 && args[1] == "--now" {
			state.Active = false
		}
	case "start":
		state.Active = true
	case "stop":
		state.Active = false
	}
	host.States[service] = state
	return nil
}
