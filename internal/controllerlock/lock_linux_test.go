//go:build linux

package controllerlock

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/le0xdon/le0xfarm/internal/farmerr"
)

func TestLeaseExcludesConcurrentControllerAndReleasesOnClose(t *testing.T) {
	dir := privateTempDir(t)
	first, err := Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Acquire(dir); code(err) != farmerr.SERVICE_NOT_READY {
		t.Fatalf("second lease=%v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Validate(dir); err != nil {
		t.Fatal(err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestLeaseIgnoresReplaceableLegacyMarkerButStillExcludes(t *testing.T) {
	for _, test := range []struct {
		name    string
		replace func(string) error
	}{
		{name: "new inode", replace: func(path string) error { return os.WriteFile(path, []byte("replacement"), 0600) }},
		{name: "symlink", replace: func(path string) error { return os.Symlink(filepath.Join(filepath.Dir(path), "target"), path) }},
		{name: "hard link", replace: func(path string) error {
			target := filepath.Join(filepath.Dir(path), "replacement-target")
			if err := os.WriteFile(target, []byte("replacement"), 0600); err != nil {
				return err
			}
			return os.Link(target, path)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := privateTempDir(t)
			first, err := Acquire(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer first.Close()
			if err := test.replace(filepath.Join(dir, fileName)); err != nil {
				t.Fatal(err)
			}
			if err := first.Validate(dir); err != nil {
				t.Fatalf("legacy marker affected kernel lease: %v", err)
			}
			if second, err := Acquire(dir); code(err) != farmerr.SERVICE_NOT_READY || second != nil {
				if second != nil {
					_ = second.Close()
				}
				t.Fatalf("second lease after %s marker=%v lease=%v", test.name, err, second)
			}
		})
	}
}

func TestLeaseExcludesAfterDataDirectoryRenameAndReplacement(t *testing.T) {
	parent := privateTempDir(t)
	dir := filepath.Join(parent, "controller")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	lease, err := Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(parent, "controller-old")
	if err := os.Rename(dir, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := lease.Validate(dir); code(err) != farmerr.SERVICE_NOT_READY {
		t.Fatalf("replaced data-directory path remained authoritative: %v", err)
	}
	if second, err := Acquire(dir); code(err) != farmerr.SERVICE_NOT_READY || second != nil {
		if second != nil {
			_ = second.Close()
		}
		t.Fatalf("replacement path obtained dual authority: err=%v lease=%v", err, second)
	}
	if second, err := Acquire(old); code(err) != farmerr.SERVICE_NOT_READY || second != nil {
		if second != nil {
			_ = second.Close()
		}
		t.Fatalf("renamed held directory obtained dual object authority: err=%v lease=%v", err, second)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	legitimate, err := Acquire(dir)
	if err != nil {
		t.Fatalf("lease was not released: %v", err)
	}
	_ = legitimate.Close()
}

func TestLeaseRejectsSymlinkDataDirectory(t *testing.T) {
	parent := privateTempDir(t)
	target := filepath.Join(parent, "target")
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(parent, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Acquire(link); code(err) != farmerr.PERMISSION_DENIED {
		t.Fatalf("symlink data directory accepted: %v", err)
	}
}

func TestLeaseObjectIdentityExcludesAlternateParentPath(t *testing.T) {
	parent := privateTempDir(t)
	realParent := filepath.Join(parent, "real")
	if err := os.Mkdir(realParent, 0700); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(realParent, "controller")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	aliasParent := filepath.Join(parent, "alias")
	if err := os.Symlink(realParent, aliasParent); err != nil {
		t.Fatal(err)
	}
	lease, err := Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	if second, err := Acquire(filepath.Join(aliasParent, "controller")); code(err) != farmerr.SERVICE_NOT_READY || second != nil {
		if second != nil {
			_ = second.Close()
		}
		t.Fatalf("alternate path obtained directory-object authority: err=%v lease=%v", err, second)
	}
}

func TestStaleLegacyMarkerDoesNotSurviveKernelLease(t *testing.T) {
	dir := privateTempDir(t)
	first, err := Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, fileName), []byte("stale"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := Acquire(dir)
	if err != nil {
		t.Fatalf("stale filesystem marker blocked released kernel lease: %v", err)
	}
	_ = second.Close()
}

func TestOpenDirectoryBindsHeldObject(t *testing.T) {
	dir := privateTempDir(t)
	lease, err := Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	bound, err := lease.OpenDirectory(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer bound.Close()
	if info, err := bound.Stat(); err != nil || !info.IsDir() {
		t.Fatalf("bound directory info=%v err=%v", info, err)
	}
}

func privateTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func code(err error) farmerr.Code {
	value, _ := farmerr.CodeOf(err)
	return value
}
