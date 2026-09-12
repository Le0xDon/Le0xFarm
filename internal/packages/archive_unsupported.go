//go:build !linux

package packages

import (
	"errors"
	"io"
	"os"

	"github.com/le0xdon/le0xfarm/internal/farmerr"
)

func openPackageArchive(string) (*os.File, int64, error) {
	return nil, 0, typed(farmerr.PERMISSION_DENIED, "verified package import requires the Linux package-store backend", errors.ErrUnsupported)
}

func validateOpenPackageArchive(*os.File, int64) error { return errors.ErrUnsupported }

func createAnonymousArchive(*os.File) (*os.File, error) { return nil, errors.ErrUnsupported }

func openPackageDirectory(string) (*os.File, error) { return nil, errors.ErrUnsupported }

func cleanupPackageStaging(*os.File, string, *os.File) error { return errors.ErrUnsupported }

func extractArchive(io.Reader, *os.File) (map[string]string, error) {
	return nil, errors.ErrUnsupported
}

func openVerifiedExecutable(*os.File, string, string) (*os.File, error) {
	return nil, errors.ErrUnsupported
}

func writeSyncedAt(*os.File, string, []byte, os.FileMode) error { return errors.ErrUnsupported }
