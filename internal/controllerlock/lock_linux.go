//go:build linux

// Package controllerlock provides the Ubuntu process boundary that prevents a
// live Controller and an offline restore from owning the same configured data
// directory.
package controllerlock

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/le0xdon/le0xfarm/internal/farmerr"
	"golang.org/x/sys/unix"
)

// fileName is retained only as a reserved legacy diagnostic filename. Lease
// authority deliberately does not depend on any entry inside DataDir.
const fileName = "controller.lock"

type fileIdentity struct {
	device uint64
	inode  uint64
}

type Lease struct {
	mu          sync.Mutex
	directory   *os.File
	pathLease   *net.UnixListener
	objectLease *net.UnixListener
	dir         string
	pathName    string
	objectName  string
	directoryID fileIdentity
	closed      bool
}

func Acquire(dataDir string) (*Lease, error) {
	abs, err := canonicalDataDir(dataDir)
	if err != nil {
		return nil, err
	}

	// Linux abstract Unix sockets are kernel namespace objects. There is no
	// filesystem entry for a same-UID process to unlink or replace, and the bind
	// remains unique even if the configured DataDir path is renamed/recreated.
	// A hostile process may pre-bind and deny service; it cannot obtain dual
	// Controller authority. Closing the socket (including process death) releases
	// the lease automatically.
	pathName := abstractSocketName("path", abs)
	pathLease, err := net.ListenUnix("unix", &net.UnixAddr{Name: pathName, Net: "unix"})
	if err != nil {
		return nil, lockError(farmerr.SERVICE_NOT_READY, "Controller data directory is already in use", err)
	}
	directory, directoryID, err := openPrivateDirectory(abs)
	if err != nil {
		_ = pathLease.Close()
		return nil, lockError(farmerr.PERMISSION_DENIED, "unsafe Controller data directory", err)
	}
	objectName := abstractSocketName("object", fmt.Sprintf("%d:%d", directoryID.device, directoryID.inode))
	objectLease, err := net.ListenUnix("unix", &net.UnixAddr{Name: objectName, Net: "unix"})
	if err != nil {
		_ = directory.Close()
		_ = pathLease.Close()
		return nil, lockError(farmerr.SERVICE_NOT_READY, "Controller data-directory object is already in use", err)
	}
	lease := &Lease{directory: directory, pathLease: pathLease, objectLease: objectLease, dir: abs, pathName: pathName, objectName: objectName, directoryID: directoryID}
	if err := lease.validateLocked(abs); err != nil {
		_ = lease.closeLocked()
		return nil, err
	}
	return lease, nil
}

func canonicalDataDir(dataDir string) (string, error) {
	if dataDir == "" {
		return "", lockError(farmerr.CONFIG_CONFLICT, "Controller data directory is required", nil)
	}
	abs, err := filepath.Abs(dataDir)
	if err != nil {
		return "", lockError(farmerr.CONFIG_CONFLICT, "invalid Controller data directory", err)
	}
	return filepath.Clean(abs), nil
}

func abstractSocketName(kind, identity string) string {
	sum := sha256.Sum256([]byte(kind + "\x00" + identity))
	return "@le0xfarm-controller-" + kind + "-" + hex.EncodeToString(sum[:])
}

func (lease *Lease) Validate(dataDir string) error {
	if lease == nil {
		return lockError(farmerr.SERVICE_NOT_READY, "offline Controller lease is required", nil)
	}
	abs, err := canonicalDataDir(dataDir)
	if err != nil {
		return err
	}
	lease.mu.Lock()
	defer lease.mu.Unlock()
	return lease.validateLocked(abs)
}

func (lease *Lease) validateLocked(abs string) error {
	if lease.closed || lease.directory == nil || lease.pathLease == nil || lease.objectLease == nil || lease.dir != abs {
		return lockError(farmerr.SERVICE_NOT_READY, "offline Controller lease is not authoritative", nil)
	}
	if err := validateListener(lease.pathLease, lease.pathName); err != nil {
		return lockError(farmerr.SERVICE_NOT_READY, "Controller path lease changed", err)
	}
	if err := validateListener(lease.objectLease, lease.objectName); err != nil {
		return lockError(farmerr.SERVICE_NOT_READY, "Controller directory-object lease changed", err)
	}
	heldDirectoryID, err := directoryIdentity(lease.directory)
	if err != nil || heldDirectoryID != lease.directoryID {
		return lockError(farmerr.SERVICE_NOT_READY, "Controller data-directory lease changed", err)
	}
	currentDirectory, currentDirectoryID, err := openPrivateDirectory(abs)
	if err != nil {
		return lockError(farmerr.SERVICE_NOT_READY, "Controller data-directory path continuity is lost", err)
	}
	_ = currentDirectory.Close()
	if currentDirectoryID != lease.directoryID {
		return lockError(farmerr.SERVICE_NOT_READY, "Controller data-directory path was replaced", nil)
	}
	return nil
}

func validateListener(listener *net.UnixListener, expected string) error {
	raw, err := listener.SyscallConn()
	if err != nil {
		return err
	}
	var socketErr error
	if err := raw.Control(func(fd uintptr) {
		address, err := unix.Getsockname(int(fd))
		if err != nil {
			socketErr = err
			return
		}
		unixAddress, ok := address.(*unix.SockaddrUnix)
		if !ok {
			socketErr = errors.New("Controller lease is not a Unix-domain socket")
			return
		}
		name := unixAddress.Name
		if len(name) > 0 && name[0] == 0 {
			name = "@" + name[1:]
		}
		if name != expected {
			socketErr = errors.New("Controller lease socket identity changed")
		}
	}); err != nil {
		return err
	}
	return socketErr
}

// OpenDirectory returns a close-on-exec duplicate of the exact directory
// object held by the lease. Callers can therefore use openat/linkat/unlinkat
// without resolving DataDir again. The configured pathname must still name
// that object at the time the duplicate is granted.
func (lease *Lease) OpenDirectory(dataDir string) (*os.File, error) {
	if lease == nil {
		return nil, lockError(farmerr.SERVICE_NOT_READY, "offline Controller lease is required", nil)
	}
	abs, err := canonicalDataDir(dataDir)
	if err != nil {
		return nil, err
	}
	lease.mu.Lock()
	defer lease.mu.Unlock()
	if err := lease.validateLocked(abs); err != nil {
		return nil, err
	}
	fd, err := unix.FcntlInt(lease.directory.Fd(), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		return nil, lockError(farmerr.SERVICE_NOT_READY, "cannot bind Controller data-directory descriptor", err)
	}
	return os.NewFile(uintptr(fd), abs), nil
}

func (lease *Lease) Close() error {
	if lease == nil {
		return nil
	}
	lease.mu.Lock()
	defer lease.mu.Unlock()
	return lease.closeLocked()
}

func (lease *Lease) closeLocked() error {
	if lease.closed {
		return nil
	}
	lease.closed = true
	var pathErr, objectErr, directoryErr error
	if lease.pathLease != nil {
		pathErr = lease.pathLease.Close()
	}
	if lease.objectLease != nil {
		objectErr = lease.objectLease.Close()
	}
	if lease.directory != nil {
		directoryErr = lease.directory.Close()
	}
	return errors.Join(pathErr, objectErr, directoryErr)
}

func openPrivateDirectory(path string) (*os.File, fileIdentity, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fileIdentity{}, err
	}
	file := os.NewFile(uintptr(fd), path)
	identity, err := directoryIdentity(file)
	if err != nil {
		_ = file.Close()
		return nil, fileIdentity{}, err
	}
	return file, identity, nil
}

func directoryIdentity(file *os.File) (fileIdentity, error) {
	info, err := file.Stat()
	if err != nil {
		return fileIdentity{}, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || info.Mode().Perm() != 0700 || !ok || stat.Nlink == 0 {
		return fileIdentity{}, errors.New("Controller data directory must be a real mode-0700 directory")
	}
	return fileIdentity{device: uint64(stat.Dev), inode: stat.Ino}, nil
}

func lockError(code farmerr.Code, message string, cause error) error {
	details := map[string]string{}
	if cause != nil {
		details["reason"] = "local lock operation failed"
	}
	return farmerr.Error{Code: code, HumanMessage: message, Details: details}
}
