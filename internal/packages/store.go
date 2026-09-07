// Package packages installs already-delivered archives into an immutable,
// content-verified Agent package store. It never downloads artifacts.
package packages

import (
	"archive/tar"
	"compress/gzip"
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
}

func New(agentDataDir string) *Store { return &Store{root: filepath.Join(agentDataDir, "packages")} }

func (s *Store) Install(ctx context.Context, archivePath string, manifest Manifest) (Installed, error) {
	if err := validateManifest(manifest); err != nil {
		return Installed{}, err
	}
	hash, err := hashFile(archivePath)
	if err != nil {
		return Installed{}, typed(farmerr.MISSING_DEPENDENCY, "package archive is unavailable", err)
	}
	if !strings.EqualFold(hash, manifest.ArchiveSHA256) {
		return Installed{}, typed(farmerr.PACKAGE_HASH_MISMATCH, "package archive SHA-256 does not match manifest", nil)
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
	if _, err := os.Stat(final); err == nil {
		installed, loadErr := s.load(manifest.PackageID, manifest.Version)
		if loadErr != nil {
			return Installed{}, loadErr
		}
		if !reflect.DeepEqual(installed.Manifest, manifest) {
			return Installed{}, typed(farmerr.CONFIG_CONFLICT, "package identity/version already contains different content", nil)
		}
		return installed, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return Installed{}, typed(farmerr.PERMISSION_DENIED, "cannot inspect package destination", err)
	}
	parent := filepath.Dir(final)
	if err := os.MkdirAll(parent, 0700); err != nil {
		return Installed{}, typed(farmerr.PERMISSION_DENIED, "cannot create package identity directory", err)
	}
	temp, err := os.MkdirTemp(parent, ".install-")
	if err != nil {
		return Installed{}, typed(farmerr.PERMISSION_DENIED, "cannot create package staging directory", err)
	}
	defer os.RemoveAll(temp)
	if err := os.Chmod(temp, 0700); err != nil {
		return Installed{}, err
	}
	files, err := extractArchive(archivePath, temp)
	if err != nil {
		return Installed{}, err
	}
	executable := filepath.Join(temp, filepath.FromSlash(manifest.ExecutableRelativePath))
	if err := validateExecutable(ctx, executable, manifest); err != nil {
		return Installed{}, err
	}
	installed := Installed{Manifest: manifest, InstalledAt: time.Now().UTC(), FileSHA256: files}
	data, err := json.MarshalIndent(installed, "", "  ")
	if err != nil {
		return Installed{}, typed(farmerr.INTERNAL_ERROR, "cannot encode package metadata", err)
	}
	data = append(data, '\n')
	if err := writeSynced(filepath.Join(temp, metadataFile), data, 0600); err != nil {
		return Installed{}, typed(farmerr.PERMISSION_DENIED, "cannot persist package metadata", err)
	}
	if err := syncDir(temp); err != nil {
		return Installed{}, err
	}
	if err := os.Rename(temp, final); err != nil {
		if _, statErr := os.Stat(final); statErr == nil {
			installed, loadErr := s.load(manifest.PackageID, manifest.Version)
			if loadErr != nil {
				return Installed{}, loadErr
			}
			if !reflect.DeepEqual(installed.Manifest, manifest) {
				return Installed{}, typed(farmerr.CONFIG_CONFLICT, "package identity/version was concurrently published with different content", nil)
			}
			return installed, nil
		}
		return Installed{}, typed(farmerr.PERMISSION_DENIED, "cannot publish verified package", err)
	}
	if err := syncDir(parent); err != nil {
		return Installed{}, err
	}
	installed.ExecutablePath = filepath.Join(final, filepath.FromSlash(manifest.ExecutableRelativePath))
	return installed, nil
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
	for relative, expected := range installed.FileSHA256 {
		if !safeRelative(relative) || len(expected) != 64 {
			return Installed{}, typed(farmerr.PACKAGE_HASH_MISMATCH, "installed package file manifest is invalid", nil)
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

func extractArchive(archivePath, destination string) (map[string]string, error) {
	f, err := os.Open(archivePath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, typed(farmerr.PACKAGE_HASH_MISMATCH, "package archive is not valid gzip", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	files := make(map[string]string)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, typed(farmerr.PACKAGE_HASH_MISMATCH, "package tar is malformed", err)
		}
		if !safeRelative(header.Name) {
			return nil, typed(farmerr.PACKAGE_HASH_MISMATCH, "unsafe package archive path", nil)
		}
		path := filepath.Join(destination, filepath.FromSlash(header.Name))
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(path, 0755); err != nil {
				return nil, err
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
				return nil, err
			}
			mode := os.FileMode(header.Mode) & 0755
			if mode&0111 == 0 {
				mode = 0644
			}
			out, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
			if err != nil {
				return nil, typed(farmerr.PACKAGE_HASH_MISMATCH, "duplicate or invalid archive entry", err)
			}
			h := sha256.New()
			_, copyErr := io.Copy(io.MultiWriter(out, h), io.LimitReader(tr, header.Size))
			syncErr := out.Sync()
			closeErr := out.Close()
			if copyErr != nil || syncErr != nil || closeErr != nil {
				return nil, typed(farmerr.PACKAGE_HASH_MISMATCH, "cannot extract package file", errors.Join(copyErr, syncErr, closeErr))
			}
			files[filepath.ToSlash(header.Name)] = hex.EncodeToString(h.Sum(nil))
		default:
			return nil, typed(farmerr.PACKAGE_HASH_MISMATCH, "links and special files are not allowed in package archives", nil)
		}
	}
	return files, nil
}

func validateExecutable(ctx context.Context, path string, manifest Manifest) error {
	info, err := os.Stat(path)
	if err != nil {
		return typed(farmerr.PACKAGE_HASH_MISMATCH, "expected package executable is missing", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return typed(farmerr.PERMISSION_DENIED, "expected package executable is not executable", nil)
	}
	if len(manifest.VersionArgs) == 0 || manifest.ExpectedVersion == "" {
		return nil
	}
	checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	output := &boundedOutput{max: 64 * 1024}
	command := exec.CommandContext(checkCtx, path, manifest.VersionArgs...)
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

func writeSynced(path string, data []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	return errors.Join(err, f.Close())
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
