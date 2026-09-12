// Package packages installs already-delivered archives into an immutable,
// content-verified Agent package store. It never downloads artifacts.
package packages

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/le0xdon/le0xfarm/internal/farmerr"
	"github.com/le0xdon/le0xfarm/internal/identity"
)

const metadataFile = "package.json"

const (
	maxArchiveBytes   int64 = 512 << 20
	maxExtractedBytes int64 = 2 << 30
	maxArchiveEntries       = 4096
)

type Manifest struct {
	PackageID              identity.PackageID `json:"package_id"`
	Name                   string             `json:"name"`
	Version                string             `json:"version"`
	OS                     string             `json:"os"`
	Architecture           string             `json:"architecture"`
	ArchiveSHA256          string             `json:"archive_sha256"`
	ExecutableRelativePath string             `json:"executable_relative_path"`
	SourceRepository       string             `json:"source_repository"`
	SourceURL              string             `json:"source_url"`
	VersionArgs            []string           `json:"version_args"`
	ExpectedVersion        string             `json:"expected_version"`
	UpstreamFeePercent     float64            `json:"upstream_fee_percent"`
}

type Installed struct {
	Manifest       Manifest          `json:"manifest"`
	InstalledAt    time.Time         `json:"installed_at"`
	FileSHA256     map[string]string `json:"file_sha256"`
	ExecutablePath string            `json:"-"`
}

type Store struct {
	root string
	mu   sync.Mutex
	// These hooks sit on the real import boundaries and are used only by
	// deterministic adversarial tests.
	afterArchiveOpened   func() error
	afterArchiveVerified func() error
}

func New(agentDataDir string) *Store { return &Store{root: filepath.Join(agentDataDir, "packages")} }

func (s *Store) Install(ctx context.Context, archivePath string, manifest Manifest) (Installed, error) {
	if err := validateManifest(manifest); err != nil {
		return Installed{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(s.root, 0700); err != nil {
		return Installed{}, typed(farmerr.PERMISSION_DENIED, "cannot create package store", err)
	}
	if err := os.Chmod(s.root, 0700); err != nil {
		return Installed{}, typed(farmerr.PERMISSION_DENIED, "cannot secure package store", err)
	}
	final := s.installDir(manifest)
	parent := filepath.Dir(final)
	if err := os.MkdirAll(parent, 0700); err != nil {
		return Installed{}, typed(farmerr.PERMISSION_DENIED, "cannot create package identity directory", err)
	}
	temp, err := os.MkdirTemp(parent, ".install-")
	if err != nil {
		return Installed{}, typed(farmerr.PERMISSION_DENIED, "cannot create package staging directory", err)
	}
	if err := os.Chmod(temp, 0700); err != nil {
		return Installed{}, err
	}
	parentDirectory, err := openPackageDirectory(parent)
	if err != nil {
		return Installed{}, typed(farmerr.PERMISSION_DENIED, "cannot retain package publication directory", err)
	}
	defer parentDirectory.Close()
	stagingDirectory, err := openPackageDirectory(temp)
	if err != nil {
		return Installed{}, typed(farmerr.PERMISSION_DENIED, "cannot retain package staging directory", err)
	}
	defer stagingDirectory.Close()
	cleanupStaging := true
	defer func() {
		if cleanupStaging {
			_ = cleanupPackageStaging(parentDirectory, filepath.Base(temp), stagingDirectory)
		}
	}()
	verifiedArchive, err := s.stageVerifiedArchive(archivePath, stagingDirectory, manifest.ArchiveSHA256)
	if err != nil {
		return Installed{}, err
	}
	defer verifiedArchive.Close()
	if s.afterArchiveVerified != nil {
		if err := s.afterArchiveVerified(); err != nil {
			return Installed{}, err
		}
	}
	if _, err := verifiedArchive.Seek(0, io.SeekStart); err != nil {
		return Installed{}, typed(farmerr.PACKAGE_HASH_MISMATCH, "cannot rewind verified package archive", err)
	}
	files, err := extractArchive(verifiedArchive, stagingDirectory)
	if err != nil {
		return Installed{}, err
	}
	executableHash := files[filepath.ToSlash(manifest.ExecutableRelativePath)]
	if err := validateExecutable(ctx, stagingDirectory, manifest.ExecutableRelativePath, executableHash, manifest); err != nil {
		return Installed{}, err
	}
	if _, err := os.Stat(final); err == nil {
		installed, loadErr := s.load(manifest.PackageID, manifest.Version)
		if loadErr != nil {
			return Installed{}, loadErr
		}
		if !reflect.DeepEqual(installed.Manifest, manifest) || !reflect.DeepEqual(installed.FileSHA256, files) {
			return Installed{}, typed(farmerr.CONFIG_CONFLICT, "package identity/version already contains different content", nil)
		}
		return installed, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return Installed{}, typed(farmerr.PERMISSION_DENIED, "cannot inspect package destination", err)
	}
	installed := Installed{Manifest: manifest, InstalledAt: time.Now().UTC(), FileSHA256: files}
	data, err := json.MarshalIndent(installed, "", "  ")
	if err != nil {
		return Installed{}, typed(farmerr.INTERNAL_ERROR, "cannot encode package metadata", err)
	}
	data = append(data, '\n')
	if err := writeSyncedAt(stagingDirectory, metadataFile, data, 0600); err != nil {
		return Installed{}, typed(farmerr.PERMISSION_DENIED, "cannot persist package metadata", err)
	}
	if err := stagingDirectory.Sync(); err != nil {
		return Installed{}, err
	}
	if err := os.Rename(temp, final); err != nil {
		if _, statErr := os.Stat(final); statErr == nil {
			installed, loadErr := s.load(manifest.PackageID, manifest.Version)
			if loadErr != nil {
				return Installed{}, loadErr
			}
			if !reflect.DeepEqual(installed.Manifest, manifest) || !reflect.DeepEqual(installed.FileSHA256, files) {
				return Installed{}, typed(farmerr.CONFIG_CONFLICT, "package identity/version was concurrently published with different content", nil)
			}
			return installed, nil
		}
		return Installed{}, typed(farmerr.PERMISSION_DENIED, "cannot publish verified package", err)
	}
	cleanupStaging = false
	if err := syncDir(parent); err != nil {
		return Installed{}, err
	}
	published, err := s.load(manifest.PackageID, manifest.Version)
	if err != nil {
		return Installed{}, err
	}
	if !reflect.DeepEqual(published.Manifest, manifest) || !reflect.DeepEqual(published.FileSHA256, files) {
		return Installed{}, typed(farmerr.PACKAGE_HASH_MISMATCH, "published package does not match the verified archive", nil)
	}
	return published, nil
}

func (s *Store) stageVerifiedArchive(archivePath string, stagingDirectory *os.File, expectedHash string) (*os.File, error) {
	source, expectedSize, err := openPackageArchive(archivePath)
	if err != nil {
		return nil, err
	}
	defer source.Close()
	if s.afterArchiveOpened != nil {
		if err := s.afterArchiveOpened(); err != nil {
			return nil, err
		}
	}
	staged, err := createAnonymousArchive(stagingDirectory)
	if err != nil {
		return nil, typed(farmerr.PERMISSION_DENIED, "cannot create private package archive staging object", err)
	}
	failed := true
	defer func() {
		if failed {
			_ = staged.Close()
		}
	}()
	hash := sha256.New()
	reader := &io.LimitedReader{R: source, N: maxArchiveBytes + 1}
	written, copyErr := io.Copy(io.MultiWriter(staged, hash), reader)
	if copyErr != nil {
		return nil, typed(farmerr.PACKAGE_HASH_MISMATCH, "cannot stage package archive", copyErr)
	}
	if written != expectedSize || written > maxArchiveBytes {
		return nil, typed(farmerr.PACKAGE_HASH_MISMATCH, "package archive changed while it was staged", nil)
	}
	if err := validateOpenPackageArchive(source, expectedSize); err != nil {
		return nil, err
	}
	got := hex.EncodeToString(hash.Sum(nil))
	if !strings.EqualFold(got, expectedHash) {
		return nil, typed(farmerr.PACKAGE_HASH_MISMATCH, "package archive SHA-256 does not match manifest", nil)
	}
	if err := staged.Sync(); err != nil {
		return nil, typed(farmerr.PERMISSION_DENIED, "cannot sync private package archive staging object", err)
	}
	if _, err := staged.Seek(0, io.SeekStart); err != nil {
		return nil, typed(farmerr.PACKAGE_HASH_MISMATCH, "cannot rewind private package archive staging object", err)
	}
	failed = false
	return staged, nil
}

func (s *Store) Lookup(packageID identity.PackageID, version string) (Installed, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.load(packageID, version)
}

func (s *Store) load(packageID identity.PackageID, version string) (Installed, error) {
	if packageID.Validate() != nil || !safeSegment(version) {
		return Installed{}, typed(farmerr.CONFIG_CONFLICT, "invalid package lookup", nil)
	}
	dir := s.installDir(Manifest{PackageID: packageID, Version: version})
	metadataPath := filepath.Join(dir, metadataFile)
	metadataInfo, err := os.Lstat(metadataPath)
	if errors.Is(err, os.ErrNotExist) {
		return Installed{}, typed(farmerr.PACKAGE_NOT_INSTALLED, "package is not installed", nil)
	}
	if err != nil || !metadataInfo.Mode().IsRegular() {
		return Installed{}, typed(farmerr.PERMISSION_DENIED, "cannot read package metadata", err)
	}
	data, err := os.ReadFile(metadataPath)
	if err != nil {
		return Installed{}, typed(farmerr.PERMISSION_DENIED, "cannot read package metadata", err)
	}
	var installed Installed
	if err := json.Unmarshal(data, &installed); err != nil || installed.Manifest.PackageID != packageID || installed.Manifest.Version != version {
		return Installed{}, typed(farmerr.PACKAGE_HASH_MISMATCH, "installed package metadata is invalid", err)
	}
	if err := validateManifest(installed.Manifest); err != nil {
		return Installed{}, typed(farmerr.PACKAGE_HASH_MISMATCH, "installed package manifest is invalid", err)
	}
	if len(installed.FileSHA256) == 0 {
		return Installed{}, typed(farmerr.PACKAGE_HASH_MISMATCH, "installed package file verification metadata is empty", nil)
	}
	executableRelative := filepath.ToSlash(installed.Manifest.ExecutableRelativePath)
	executableExpected, executableRecorded := installed.FileSHA256[executableRelative]
	if !executableRecorded {
		return Installed{}, typed(farmerr.PACKAGE_HASH_MISMATCH, "installed package executable has no verification hash", nil)
	}
	executableVerified := false
	for relative, expected := range installed.FileSHA256 {
		if !safeRelative(relative) || len(expected) != 64 || expected != strings.ToLower(expected) {
			return Installed{}, typed(farmerr.PACKAGE_HASH_MISMATCH, "installed package file manifest is invalid", nil)
		}
		if _, err := hex.DecodeString(expected); err != nil {
			return Installed{}, typed(farmerr.PACKAGE_HASH_MISMATCH, "installed package file manifest is invalid", err)
		}
		path := filepath.Join(dir, filepath.FromSlash(relative))
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() {
			return Installed{}, typed(farmerr.PACKAGE_HASH_MISMATCH, "installed package file is not regular", err)
		}
		got, err := hashFile(path)
		if err != nil || got != expected {
			return Installed{}, typed(farmerr.PACKAGE_HASH_MISMATCH, "installed package content verification failed", err)
		}
		if relative == executableRelative && expected == executableExpected {
			executableVerified = true
		}
	}
	if !executableVerified {
		return Installed{}, typed(farmerr.PACKAGE_HASH_MISMATCH, "installed package executable was not verified", nil)
	}
	installed.ExecutablePath = filepath.Join(dir, filepath.FromSlash(installed.Manifest.ExecutableRelativePath))
	info, err := os.Lstat(installed.ExecutablePath)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return Installed{}, typed(farmerr.PACKAGE_HASH_MISMATCH, "installed package executable is invalid", err)
	}
	return installed, nil
}

func (s *Store) installDir(manifest Manifest) string {
	return filepath.Join(s.root, manifest.PackageID.String(), manifest.Version)
}

func validateManifest(m Manifest) error {
	if m.PackageID.Validate() != nil || m.Name == "" || !safeSegment(m.Version) || m.OS == "" || m.Architecture == "" || len(m.ArchiveSHA256) != 64 || !safeRelative(m.ExecutableRelativePath) {
		return typed(farmerr.CONFIG_CONFLICT, "invalid package manifest", nil)
	}
	if _, err := hex.DecodeString(m.ArchiveSHA256); err != nil {
		return typed(farmerr.CONFIG_CONFLICT, "invalid package archive SHA-256", err)
	}
	return nil
}

func safeSegment(value string) bool {
	return value != "" && value != "." && value != ".." && !strings.ContainsAny(value, `/\\`) && !strings.ContainsRune(value, 0)
}

func safeRelative(name string) bool {
	cleanName := strings.TrimSuffix(name, "/")
	return cleanName != "" && !filepath.IsAbs(cleanName) && filepath.Clean(cleanName) == filepath.FromSlash(cleanName) && cleanName != ".." && !strings.HasPrefix(cleanName, "../") && !strings.ContainsRune(cleanName, 0)
}

func validateExecutable(ctx context.Context, stagingDirectory *os.File, relativePath, expectedHash string, manifest Manifest) error {
	executable, err := openVerifiedExecutable(stagingDirectory, relativePath, expectedHash)
	if err != nil {
		return err
	}
	defer executable.Close()
	if len(manifest.VersionArgs) == 0 || manifest.ExpectedVersion == "" {
		return nil
	}
	checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	output := &boundedOutput{max: 64 * 1024}
	// ExtraFiles installs the already-verified sealed executable as fd 3 in the
	// child. /proc/self/fd/3 therefore cannot be redirected through the mutable
	// staging pathname between validation and exec.
	command := exec.CommandContext(checkCtx, "/proc/self/fd/3", manifest.VersionArgs...)
	command.ExtraFiles = []*os.File{executable}
	command.Stdout, command.Stderr = output, output
	err = command.Run()
	if err != nil || !strings.Contains(output.String(), manifest.ExpectedVersion) {
		return typed(farmerr.CONFIG_CONFLICT, "package executable version does not match manifest", err)
	}
	return nil
}

type boundedOutput struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func (w *boundedOutput) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	remaining := w.max - len(w.buf)
	if remaining > 0 {
		if remaining > len(data) {
			remaining = len(data)
		}
		w.buf = append(w.buf, data[:remaining]...)
	}
	return len(data), nil
}

func (w *boundedOutput) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return string(w.buf)
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func syncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}

func typed(code farmerr.Code, message string, err error) error {
	details := map[string]string{}
	if err != nil {
		details["reason"] = err.Error()
	}
	return farmerr.Error{Code: code, HumanMessage: message, Details: details}
}

func (m Manifest) String() string { return fmt.Sprintf("%s %s", m.Name, m.Version) }
