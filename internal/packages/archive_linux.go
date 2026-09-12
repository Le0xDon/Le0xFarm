//go:build linux

package packages

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/le0xdon/le0xfarm/internal/farmerr"
	"golang.org/x/sys/unix"
)

func openPackageArchive(path string) (*os.File, int64, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, 0, typed(farmerr.PERMISSION_DENIED, "cannot safely open package archive", err)
	}
	file := os.NewFile(uintptr(fd), "package-archive")
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, 0, typed(farmerr.PERMISSION_DENIED, "cannot inspect package archive", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || stat.Nlink != 1 || info.Size() <= 0 || info.Size() > maxArchiveBytes {
		file.Close()
		return nil, 0, typed(farmerr.PACKAGE_HASH_MISMATCH, "package archive must be a bounded single-link regular file", nil)
	}
	return file, info.Size(), nil
}

func validateOpenPackageArchive(file *os.File, expectedSize int64) error {
	info, err := file.Stat()
	if err != nil {
		return typed(farmerr.PERMISSION_DENIED, "cannot revalidate open package archive", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || stat.Nlink != 1 || info.Size() != expectedSize {
		return typed(farmerr.PACKAGE_HASH_MISMATCH, "package archive changed while it was staged", nil)
	}
	return nil
}

func createAnonymousArchive(directory *os.File) (*os.File, error) {
	fd, err := unix.Openat(int(directory.Fd()), ".", unix.O_RDWR|unix.O_CLOEXEC|unix.O_TMPFILE, 0600)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), "verified-package-archive"), nil
}

func openPackageDirectory(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), "package-directory"), nil
}

func cleanupPackageStaging(parent *os.File, name string, staging *os.File) error {
	if err := removePackageDirectoryContents(int(staging.Fd())); err != nil {
		return err
	}
	if err := unix.Unlinkat(int(parent.Fd()), name, unix.AT_REMOVEDIR); err != nil && err != unix.ENOENT {
		return err
	}
	return nil
}

func removePackageDirectoryContents(directoryFD int) error {
	duplicate, err := unix.Dup(directoryFD)
	if err != nil {
		return err
	}
	unix.CloseOnExec(duplicate)
	directory := os.NewFile(uintptr(duplicate), "package-staging-cleanup")
	entries, readErr := directory.ReadDir(-1)
	closeErr := directory.Close()
	if readErr != nil || closeErr != nil {
		return errors.Join(readErr, closeErr)
	}
	for _, entry := range entries {
		name := entry.Name()
		fd, openErr := unix.Openat(directoryFD, name, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if openErr != nil {
			return openErr
		}
		var stat unix.Stat_t
		statErr := unix.Fstat(fd, &stat)
		if statErr != nil {
			unix.Close(fd)
			return statErr
		}
		if stat.Mode&unix.S_IFMT == unix.S_IFDIR {
			if err := removePackageDirectoryContents(fd); err != nil {
				unix.Close(fd)
				return err
			}
			unix.Close(fd)
			if err := unix.Unlinkat(directoryFD, name, unix.AT_REMOVEDIR); err != nil {
				return err
			}
			continue
		}
		unix.Close(fd)
		if err := unix.Unlinkat(directoryFD, name, 0); err != nil {
			return err
		}
	}
	return nil
}

func extractArchive(archive io.Reader, destination *os.File) (map[string]string, error) {
	rootFD := int(destination.Fd())
	gz, err := gzip.NewReader(archive)
	if err != nil {
		return nil, typed(farmerr.PACKAGE_HASH_MISMATCH, "package archive is not valid gzip", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	files := make(map[string]string)
	var totalBytes int64
	entries := 0
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
		entries++
		if entries > maxArchiveEntries || header.Size < 0 || header.Size > maxExtractedBytes-totalBytes {
			return nil, typed(farmerr.PACKAGE_HASH_MISMATCH, "package archive exceeds extraction limits", nil)
		}
		totalBytes += header.Size
		relative := strings.TrimSuffix(filepath.ToSlash(header.Name), "/")
		switch header.Typeflag {
		case tar.TypeDir:
			dir, openErr := openPackageDirectoryAt(rootFD, relative, true)
			if openErr != nil {
				return nil, typed(farmerr.PACKAGE_HASH_MISMATCH, "cannot create package archive directory", openErr)
			}
			unix.Close(dir)
		case tar.TypeReg, tar.TypeRegA:
			parent, base, openErr := openPackageParentAt(rootFD, relative)
			if openErr != nil {
				return nil, typed(farmerr.PACKAGE_HASH_MISMATCH, "cannot create package archive parent", openErr)
			}
			mode := uint32(header.Mode) & 0755
			if mode&0111 == 0 {
				mode = 0644
			}
			fd, openErr := unix.Openat(parent, base, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, mode)
			unix.Close(parent)
			if openErr != nil {
				return nil, typed(farmerr.PACKAGE_HASH_MISMATCH, "duplicate or invalid archive entry", openErr)
			}
			out := os.NewFile(uintptr(fd), "package-archive-entry")
			hash := sha256.New()
			written, copyErr := io.Copy(io.MultiWriter(out, hash), io.LimitReader(tr, header.Size))
			syncErr := out.Sync()
			closeErr := out.Close()
			if copyErr != nil || written != header.Size || syncErr != nil || closeErr != nil {
				return nil, typed(farmerr.PACKAGE_HASH_MISMATCH, "cannot extract package file", errors.Join(copyErr, syncErr, closeErr))
			}
			files[relative] = hex.EncodeToString(hash.Sum(nil))
		default:
			return nil, typed(farmerr.PACKAGE_HASH_MISMATCH, "links and special files are not allowed in package archives", nil)
		}
	}
	return files, nil
}

func openPackageParentAt(rootFD int, relative string) (int, string, error) {
	directory, base := filepath.Split(relative)
	directory = strings.TrimSuffix(filepath.ToSlash(directory), "/")
	if directory == "" {
		fd, err := unix.Dup(rootFD)
		if err == nil {
			unix.CloseOnExec(fd)
		}
		return fd, base, err
	}
	fd, err := openPackageDirectoryAt(rootFD, directory, true)
	return fd, base, err
}

func openPackageDirectoryAt(rootFD int, relative string, create bool) (int, error) {
	current, err := unix.Dup(rootFD)
	if err != nil {
		return -1, err
	}
	unix.CloseOnExec(current)
	for _, component := range strings.Split(relative, "/") {
		if component == "" {
			continue
		}
		if create {
			if err := unix.Mkdirat(current, component, 0755); err != nil && err != unix.EEXIST {
				unix.Close(current)
				return -1, err
			}
		}
		next, err := unix.Openat(current, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		unix.Close(current)
		if err != nil {
			return -1, err
		}
		current = next
	}
	return current, nil
}

func openVerifiedExecutable(root *os.File, relativePath, expectedHash string) (*os.File, error) {
	if len(expectedHash) != sha256.Size*2 {
		return nil, typed(farmerr.PACKAGE_HASH_MISMATCH, "expected package executable has no verified archive hash", nil)
	}
	parentFD, base, err := openPackageParentAt(int(root.Fd()), filepath.ToSlash(relativePath))
	if err != nil {
		return nil, typed(farmerr.PACKAGE_HASH_MISMATCH, "cannot safely open expected package executable parent", err)
	}
	sourceFD, err := unix.Openat(parentFD, base, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	unix.Close(parentFD)
	if err != nil {
		return nil, typed(farmerr.PACKAGE_HASH_MISMATCH, "cannot safely open expected package executable", err)
	}
	source := os.NewFile(uintptr(sourceFD), "extracted-package-executable")
	defer source.Close()
	info, err := source.Stat()
	if err != nil {
		return nil, typed(farmerr.PACKAGE_HASH_MISMATCH, "cannot inspect expected package executable", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || stat.Nlink != 1 || info.Size() < 1 || info.Size() > maxArchiveBytes {
		return nil, typed(farmerr.PACKAGE_HASH_MISMATCH, "expected package executable is not a bounded single-link executable", nil)
	}
	if info.Mode().Perm()&0111 == 0 {
		return nil, typed(farmerr.PERMISSION_DENIED, "expected package executable is not executable", nil)
	}
	memFD, err := unix.MemfdCreate("le0xfarm-package-verify", unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING)
	if err != nil {
		return nil, typed(farmerr.PERMISSION_DENIED, "cannot create private executable validation object", err)
	}
	verified := os.NewFile(uintptr(memFD), "verified-package-executable")
	failed := true
	defer func() {
		if failed {
			_ = verified.Close()
		}
	}()
	hash := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(verified, hash), io.LimitReader(source, maxArchiveBytes+1))
	if copyErr != nil || written != info.Size() || written > maxArchiveBytes {
		return nil, typed(farmerr.PACKAGE_HASH_MISMATCH, "expected package executable changed during validation", copyErr)
	}
	after, statErr := source.Stat()
	if statErr != nil {
		return nil, typed(farmerr.PACKAGE_HASH_MISMATCH, "expected package executable changed during validation", statErr)
	}
	afterStat, afterOK := after.Sys().(*syscall.Stat_t)
	if !afterOK || !after.Mode().IsRegular() || afterStat.Nlink != 1 || after.Size() != info.Size() {
		return nil, typed(farmerr.PACKAGE_HASH_MISMATCH, "expected package executable changed during validation", nil)
	}
	if got := hex.EncodeToString(hash.Sum(nil)); got != expectedHash {
		return nil, typed(farmerr.PACKAGE_HASH_MISMATCH, "expected package executable does not match verified archive content", nil)
	}
	if err := verified.Sync(); err != nil {
		return nil, typed(farmerr.PERMISSION_DENIED, "cannot sync executable validation object", err)
	}
	if err := verified.Chmod(0500); err != nil {
		return nil, typed(farmerr.PERMISSION_DENIED, "cannot secure executable validation object", err)
	}
	if _, err := unix.FcntlInt(verified.Fd(), unix.F_ADD_SEALS, unix.F_SEAL_WRITE|unix.F_SEAL_GROW|unix.F_SEAL_SHRINK|unix.F_SEAL_SEAL); err != nil {
		return nil, typed(farmerr.PERMISSION_DENIED, "cannot seal executable validation object", err)
	}
	if _, err := verified.Seek(0, io.SeekStart); err != nil {
		return nil, typed(farmerr.PACKAGE_HASH_MISMATCH, "cannot rewind executable validation object", err)
	}
	failed = false
	return verified, nil
}

func writeSyncedAt(directory *os.File, name string, data []byte, mode os.FileMode) error {
	if !safeSegment(name) {
		return unix.EINVAL
	}
	fd, err := unix.Openat(int(directory.Fd()), name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, uint32(mode.Perm()))
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), "package-metadata")
	if _, err = file.Write(data); err == nil {
		err = file.Sync()
	}
	return errors.Join(err, file.Close())
}
