//go:build linux

package controlleradmin

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
)

func SocketPath(dataDir string) string { return filepath.Join(dataDir, SocketName) }

func Listen(dataDir string) (net.Listener, error) {
	path := SocketPath(dataDir)
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("operator socket path is not a socket")
		}
		if err = os.Remove(path); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err = os.Chmod(path, 0600); err != nil {
		listener.Close()
		return nil, err
	}
	return listener, nil
}
