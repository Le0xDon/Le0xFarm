//go:build linux

package controllerbackup

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

const (
	restoreOldFile    = ".farm.db.restore-old"
	restoreMarkerFile = ".farm.db.restore-in-progress"
)

type fileObjectIdentity struct {
	device uint64
	inode  uint64
}

func privateDirectoryIdentity(file *os.File) (fileObjectIdentity, error) {
	info, err := file.Stat()
	if err != nil {
		return fileObjectIdentity{}, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || info.Mode().Perm() != 0700 || stat.Nlink == 0 {
		return fileObjectIdentity{}, errors.New("directory is not a private mode-0700 directory")
	}
	return fileObjectIdentity{device: uint64(stat.Dev), inode: stat.Ino}, nil
}

func privateRegularIdentity(file *os.File, links uint64) (fileObjectIdentity, error) {
	info, err := file.Stat()
	if err != nil {
		return fileObjectIdentity{}, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || uint64(stat.Nlink) != links {
		return fileObjectIdentity{}, errors.New("file is not an exclusive regular mode-0600 object")
	}
	return fileObjectIdentity{device: uint64(stat.Dev), inode: stat.Ino}, nil
}

func openPrivateDirectoryPath(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if _, err := privateDirectoryIdentity(file); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func openPrivateDirectoryAt(parent *os.File, name string) (*os.File, error) {
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), name)
	if _, err := privateDirectoryIdentity(file); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func openBackupRoot(dataDir, backupDir string) (*os.File, error) {
	data, err := openPrivateDirectoryPath(dataDir)
	if err != nil {
		return nil, err
	}
	defer data.Close()
	return openBackupRootAt(data, dataDir, backupDir)
}

func openBackupRootAt(data *os.File, dataDir, backupDir string) (*os.File, error) {
	name := filepath.Base(backupDir)
	if name == "." || name == string(filepath.Separator) || filepath.Join(dataDir, name) != backupDir {
		return nil, errors.New("backup root is not an immediate DataDir child")
	}
	if err := unix.Mkdirat(int(data.Fd()), name, 0700); err != nil && !errors.Is(err, unix.EEXIST) {
		return nil, err
	}
	return openPrivateDirectoryAt(data, name)
}

func validateBackupRootPath(dataDir, backupDir string, expected fileObjectIdentity) error {
	data, err := openPrivateDirectoryPath(dataDir)
	if err != nil {
		return err
	}
	defer data.Close()
	root, err := openPrivateDirectoryAt(data, filepath.Base(backupDir))
	if err != nil {
		return err
	}
	defer root.Close()
	actual, err := privateDirectoryIdentity(root)
	if err != nil {
		return err
	}
	if actual != expected {
		return errors.New("configured backup root was replaced")
	}
	return nil
}

func createPrivateDirectoryAt(parent *os.File, name string) (*os.File, error) {
	if err := unix.Mkdirat(int(parent.Fd()), name, 0700); err != nil {
		return nil, err
	}
	return openPrivateDirectoryAt(parent, name)
}

func openRegularAt(parent *os.File, name string) (*os.File, error) {
	return openRegularAtLinks(parent, name, 1)
}

func openPrivateRegularAt(parent *os.File, name string) (*os.File, error) {
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), name)
	info, err := file.Stat()
	stat, ok := infoSysStat(info)
	if err != nil || !ok || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || stat.Nlink == 0 {
		_ = file.Close()
		return nil, errors.New("file is not a private regular mode-0600 object")
	}
	return file, nil
}

func privateRegularDetails(file *os.File) (fileObjectIdentity, uint64, int64, error) {
	info, err := file.Stat()
	if err != nil {
		return fileObjectIdentity{}, 0, 0, err
	}
	stat, ok := infoSysStat(info)
	if !ok || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || stat.Nlink == 0 {
		return fileObjectIdentity{}, 0, 0, errors.New("file is not a private regular mode-0600 object")
	}
	return fileObjectIdentity{device: uint64(stat.Dev), inode: stat.Ino}, uint64(stat.Nlink), info.Size(), nil
}

func openRegularAtLinks(parent *os.File, name string, links uint64) (*os.File, error) {
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), name)
	if _, err := privateRegularIdentity(file, links); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func createRegularAt(parent *os.File, name string) (*os.File, error) {
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_RDWR|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), name)
	if _, err := privateRegularIdentity(file, 1); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func secureExistingRegularAt(parent *os.File, name string) (*os.File, error) {
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), name)
	info, err := file.Stat()
	stat, ok := infoSysStat(info)
	if err != nil || !ok || !info.Mode().IsRegular() || stat.Nlink != 1 {
		_ = file.Close()
		return nil, errors.New("created database is not an exclusive regular file")
	}
	if err := file.Chmod(0600); err != nil {
		_ = file.Close()
		return nil, err
	}
	if _, err := privateRegularIdentity(file, 1); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func infoSysStat(info os.FileInfo) (*syscall.Stat_t, bool) {
	if info == nil {
		return nil, false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return stat, ok
}

func createAnonymousRegular(parent *os.File) (*os.File, error) {
	fd, err := unix.Openat(int(parent.Fd()), ".", unix.O_RDWR|unix.O_CLOEXEC|unix.O_TMPFILE, 0600)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), "anonymous restore database")
	if _, err := privateRegularIdentity(file, 0); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func procDescriptorPath(file *os.File) string {
	return fmt.Sprintf("/proc/self/fd/%d", file.Fd())
}

func duplicateDirectory(file *os.File) (*os.File, error) {
	// openat(".") creates an independent directory stream. dup would share the
	// directory offset and make repeated validation observe a false empty set.
	fd, err := unix.Openat(int(file.Fd()), ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), file.Name()), nil
}

func directoryNames(file *os.File) ([]string, error) {
	copy, err := duplicateDirectory(file)
	if err != nil {
		return nil, err
	}
	defer copy.Close()
	entries, err := copy.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names, nil
}

func directoryEntryIdentity(parent *os.File, name string, directory bool) (fileObjectIdentity, error) {
	var stat unix.Stat_t
	if err := unix.Fstatat(int(parent.Fd()), name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return fileObjectIdentity{}, err
	}
	wantType := uint32(unix.S_IFREG)
	wantMode := uint32(0600)
	if directory {
		wantType = unix.S_IFDIR
		wantMode = 0700
	}
	if stat.Mode&unix.S_IFMT != wantType || stat.Mode&0777 != wantMode || stat.Nlink == 0 {
		return fileObjectIdentity{}, errors.New("directory entry has unsafe type, mode, or links")
	}
	if !directory && stat.Nlink != 1 {
		return fileObjectIdentity{}, errors.New("regular directory entry has additional hard links")
	}
	return fileObjectIdentity{device: uint64(stat.Dev), inode: stat.Ino}, nil
}

func entryMatches(parent *os.File, name string, expected fileObjectIdentity, directory bool) error {
	actual, err := directoryEntryIdentity(parent, name, directory)
	if err != nil {
		return err
	}
	if actual != expected {
		return errors.New("directory entry no longer names the bound object")
	}
	return nil
}

func regularEntryMatchesLinks(parent *os.File, name string, expected fileObjectIdentity, links uint64) error {
	var stat unix.Stat_t
	if err := unix.Fstatat(int(parent.Fd()), name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	actual := fileObjectIdentity{device: uint64(stat.Dev), inode: stat.Ino}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Mode&0777 != 0600 || uint64(stat.Nlink) != links || actual != expected {
		return errors.New("regular entry no longer names the expected bound object")
	}
	return nil
}

func anyEntryExists(parent *os.File, name string) (bool, error) {
	var stat unix.Stat_t
	err := unix.Fstatat(int(parent.Fd()), name, &stat, unix.AT_SYMLINK_NOFOLLOW)
	if errors.Is(err, unix.ENOENT) {
		return false, nil
	}
	return err == nil, err
}

func regularEntryExists(parent *os.File, name string) (bool, error) {
	_, err := directoryEntryIdentity(parent, name, false)
	if errors.Is(err, unix.ENOENT) {
		return false, nil
	}
	return err == nil, err
}

func removeRegularEntryAt(parent *os.File, name string) error {
	file, err := openRegularAt(parent, name)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return err
	}
	identity, identityErr := privateRegularIdentity(file, 1)
	closeErr := file.Close()
	if err := errors.Join(identityErr, closeErr); err != nil {
		return err
	}
	if err := entryMatches(parent, name, identity, false); err != nil {
		return err
	}
	return unix.Unlinkat(int(parent.Fd()), name, 0)
}

func removeBoundRegularEntryAt(parent *os.File, name string, expected fileObjectIdentity) error {
	var stat unix.Stat_t
	if err := unix.Fstatat(int(parent.Fd()), name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	actual := fileObjectIdentity{device: uint64(stat.Dev), inode: stat.Ino}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Mode&0777 != 0600 || stat.Nlink == 0 || actual != expected {
		return errors.New("regular entry no longer names the bound object")
	}
	return unix.Unlinkat(int(parent.Fd()), name, 0)
}

func bindAnonymousRegular(parent, anonymous *os.File, name string) (*os.File, fileObjectIdentity, error) {
	if _, err := privateRegularIdentity(anonymous, 0); err != nil {
		return nil, fileObjectIdentity{}, err
	}
	if err := unix.Linkat(int(anonymous.Fd()), "", int(parent.Fd()), name, unix.AT_EMPTY_PATH); err != nil {
		return nil, fileObjectIdentity{}, err
	}
	bound, err := openRegularAt(parent, name)
	if err != nil {
		return nil, fileObjectIdentity{}, err
	}
	identity, err := privateRegularIdentity(bound, 1)
	if err != nil {
		bound.Close()
		return nil, fileObjectIdentity{}, err
	}
	anonymousIdentity, err := privateRegularIdentity(anonymous, 1)
	if err != nil || anonymousIdentity != identity || entryMatches(parent, name, identity, false) != nil {
		bound.Close()
		return nil, fileObjectIdentity{}, errors.New("anonymous file binding did not preserve exact object identity")
	}
	return bound, identity, nil
}

func syncDirectoryFile(file *os.File) error {
	return file.Sync()
}

func syncArtifactDirectory(file *os.File) error {
	for _, name := range []string{DatabaseFile, ManifestFile} {
		child, err := openRegularAt(file, name)
		if err != nil {
			return err
		}
		syncErr := child.Sync()
		closeErr := child.Close()
		if err := errors.Join(syncErr, closeErr); err != nil {
			return err
		}
	}
	return file.Sync()
}

func copyExact(dst, src *os.File) (int64, string, error) {
	if _, err := src.Seek(0, io.SeekStart); err != nil {
		return 0, "", err
	}
	if _, err := dst.Seek(0, io.SeekStart); err != nil {
		return 0, "", err
	}
	if err := dst.Truncate(0); err != nil {
		return 0, "", err
	}
	hash := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(dst, hash), src)
	return written, hex.EncodeToString(hash.Sum(nil)), copyErr
}

func removeExpectedArtifactAt(root, artifact *os.File, name string, expected fileObjectIdentity) error {
	if err := entryMatches(root, name, expected, true); err != nil {
		return err
	}
	names, err := directoryNames(artifact)
	if err != nil || len(names) != 2 || !containsExact(names, DatabaseFile) || !containsExact(names, ManifestFile) {
		return errors.New("managed backup directory changed before pruning")
	}
	for _, childName := range []string{DatabaseFile, ManifestFile} {
		child, err := openRegularAt(artifact, childName)
		if err != nil {
			return err
		}
		childID, identityErr := privateRegularIdentity(child, 1)
		closeErr := child.Close()
		if err := errors.Join(identityErr, closeErr); err != nil {
			return err
		}
		if err := entryMatches(artifact, childName, childID, false); err != nil {
			return err
		}
		if err := unix.Unlinkat(int(artifact.Fd()), childName, 0); err != nil {
			return err
		}
	}
	if err := artifact.Sync(); err != nil {
		return err
	}
	// Linux cannot unlink an arbitrary directory by descriptor. A final
	// name-based rmdir could delete a replacement empty directory after any
	// finite identity check. Leave the exact validated directory as an empty
	// tombstone instead; without its fixed files it is not a valid/listed backup.
	// This trades bounded empty directories for a provable non-redirection rule.
	return nil
}

func cleanupStagingDirectory(root, staging *os.File, name string) {
	expected, err := privateDirectoryIdentity(staging)
	if err != nil || entryMatches(root, name, expected, true) != nil {
		return
	}
	for _, child := range []string{DatabaseFile, ManifestFile} {
		file, err := openRegularAt(staging, child)
		if err != nil {
			continue
		}
		identity, identityErr := privateRegularIdentity(file, 1)
		_ = file.Close()
		if identityErr == nil && entryMatches(staging, child, identity, false) == nil {
			_ = unix.Unlinkat(int(staging.Fd()), child, 0)
		}
	}
	// As with pruned artifacts, do not perform a final pathname-selected rmdir
	// under a hostile namespace. An empty .incomplete-* tombstone is harmless
	// and is never considered a completed backup.
}

func containsExact(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}
