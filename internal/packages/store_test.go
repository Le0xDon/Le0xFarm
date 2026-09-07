package packages

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/le0xdon/le0xfarm/internal/farmerr"
	"github.com/le0xdon/le0xfarm/internal/identity"
)

type tarEntry struct {
	name string
	body []byte
	mode int64
	kind byte
	link string
}

func archive(t *testing.T, entries ...tarEntry) (string, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fixture.tar.gz")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for _, entry := range entries {
		kind := entry.kind
		if kind == 0 {
			kind = tar.TypeReg
		}
		header := &tar.Header{Name: entry.name, Mode: entry.mode, Size: int64(len(entry.body)), Typeflag: kind, Linkname: entry.link}
		if kind != tar.TypeReg {
			header.Size = 0
		}
		if err := tw.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if kind == tar.TypeReg {
			if _, err := tw.Write(entry.body); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	sum := sha256.Sum256(data)
	return path, hex.EncodeToString(sum[:])
}

func manifest(t *testing.T, hash string) Manifest {
	t.Helper()
	id, _ := identity.NewPackageID()
	return Manifest{PackageID: id, Name: "fixture", Version: "1.0", OS: "linux", Architecture: "amd64", ArchiveSHA256: hash, ExecutableRelativePath: "fixture/bin", SourceRepository: "https://example.invalid/source", SourceURL: "https://example.invalid/archive"}
}

func codeOf(err error) farmerr.Code { code, _ := farmerr.CodeOf(err); return code }

func TestInstallLookupIdempotencyAndIntegrity(t *testing.T) {
	path, hash := archive(t, tarEntry{name: "fixture/", mode: 0755, kind: tar.TypeDir}, tarEntry{name: "fixture/bin", body: []byte("verified"), mode: 0755})
	m := manifest(t, hash)
	store := New(t.TempDir())
	first, err := store.Install(context.Background(), path, m)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Install(context.Background(), path, m)
	if err != nil {
		t.Fatal(err)
	}
	if first.ExecutablePath != second.ExecutablePath || !filepath.IsAbs(first.ExecutablePath) {
		t.Fatal("lookup is not deterministic")
	}
	info, _ := os.Stat(filepath.Dir(filepath.Dir(filepath.Dir(first.ExecutablePath))))
	if info.Mode().Perm()&0002 != 0 {
		t.Fatal("package store is world-writable")
	}
	lookup, err := store.Lookup(m.PackageID, m.Version)
	if err != nil || lookup.ExecutablePath != first.ExecutablePath {
		t.Fatalf("lookup=%+v err=%v", lookup, err)
	}
	if err := os.WriteFile(first.ExecutablePath, []byte("tampered"), 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Lookup(m.PackageID, m.Version); codeOf(err) != farmerr.PACKAGE_HASH_MISMATCH {
		t.Fatalf("tamper error=%v", err)
	}
}

func TestLookupRejectsSymlinkReplacement(t *testing.T) {
	path, hash := archive(t, tarEntry{name: "bin", body: []byte("verified"), mode: 0755})
	m := manifest(t, hash)
	m.ExecutableRelativePath = "bin"
	store := New(t.TempDir())
	installed, err := store.Install(context.Background(), path, m)
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("verified"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(installed.ExecutablePath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, installed.ExecutablePath); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Lookup(m.PackageID, m.Version); codeOf(err) != farmerr.PACKAGE_HASH_MISMATCH {
		t.Fatalf("symlink replacement error=%v", err)
	}
}

func TestInstallRejectsWrongHashAndConflictingContent(t *testing.T) {
	path, hash := archive(t, tarEntry{name: "bin", body: []byte("one"), mode: 0755})
	m := manifest(t, hash)
	m.ExecutableRelativePath = "bin"
	store := New(t.TempDir())
	wrong := m
	wrong.ArchiveSHA256 = fmt.Sprintf("%064d", 0)
	if _, err := store.Install(context.Background(), path, wrong); codeOf(err) != farmerr.PACKAGE_HASH_MISMATCH {
		t.Fatalf("hash error=%v", err)
	}
	if _, err := store.Install(context.Background(), path, m); err != nil {
		t.Fatal(err)
	}
	otherPath, otherHash := archive(t, tarEntry{name: "bin", body: []byte("two"), mode: 0755})
	other := m
	other.ArchiveSHA256 = otherHash
	if _, err := store.Install(context.Background(), otherPath, other); codeOf(err) != farmerr.CONFIG_CONFLICT {
		t.Fatalf("conflict error=%v", err)
	}
}

func TestArchiveSafety(t *testing.T) {
	cases := []tarEntry{
		{name: "/absolute", body: []byte("x"), mode: 0644},
		{name: "../escape", body: []byte("x"), mode: 0644},
		{name: "link", mode: 0777, kind: tar.TypeSymlink, link: "../escape"},
		{name: "hard", mode: 0777, kind: tar.TypeLink, link: "../escape"},
		{name: "fifo", mode: 0644, kind: tar.TypeFifo},
	}
	for _, entry := range cases {
		t.Run(entry.name, func(t *testing.T) {
			path, hash := archive(t, entry)
			m := manifest(t, hash)
			m.ExecutableRelativePath = "missing"
			if _, err := New(t.TempDir()).Install(context.Background(), path, m); codeOf(err) != farmerr.PACKAGE_HASH_MISMATCH {
				t.Fatalf("unsafe archive error=%v", err)
			}
		})
	}
}

func TestExpectedExecutableValidation(t *testing.T) {
	missingPath, missingHash := archive(t, tarEntry{name: "other", body: []byte("x"), mode: 0755})
	m := manifest(t, missingHash)
	m.ExecutableRelativePath = "bin"
	if _, err := New(t.TempDir()).Install(context.Background(), missingPath, m); codeOf(err) != farmerr.PACKAGE_HASH_MISMATCH {
		t.Fatalf("missing error=%v", err)
	}
	nonPath, nonHash := archive(t, tarEntry{name: "bin", body: []byte("x"), mode: 0644})
	m.ArchiveSHA256 = nonHash
	if _, err := New(t.TempDir()).Install(context.Background(), nonPath, m); codeOf(err) != farmerr.PERMISSION_DENIED {
		t.Fatalf("mode error=%v", err)
	}
	executable, _ := os.Executable()
	body, _ := os.ReadFile(executable)
	versionPath, versionHash := archive(t, tarEntry{name: "bin", body: body, mode: 0755})
	m.ArchiveSHA256 = versionHash
	m.VersionArgs = []string{"-test.run=TestPackageVersionHelper", "--", "6.26.0"}
	m.ExpectedVersion = "9.99.9"
	if _, err := New(t.TempDir()).Install(context.Background(), versionPath, m); codeOf(err) != farmerr.CONFIG_CONFLICT {
		t.Fatalf("version error=%v", err)
	}
	m.ExpectedVersion = "6.26.0"
	if _, err := New(t.TempDir()).Install(context.Background(), versionPath, m); err != nil {
		t.Fatalf("matching version rejected: %v", err)
	}
}

func TestPackageVersionHelper(t *testing.T) {
	for i, arg := range os.Args {
		if arg == "--" && i+1 < len(os.Args) {
			fmt.Print(os.Args[i+1])
			return
		}
	}
}

func TestConcurrentInstallPublishesOneCompletePackage(t *testing.T) {
	path, hash := archive(t, tarEntry{name: "bin", body: []byte("complete"), mode: 0755})
	m := manifest(t, hash)
	m.ExecutableRelativePath = "bin"
	store := New(t.TempDir())
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := store.Install(context.Background(), path, m); errs <- err }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	installed, err := store.Lookup(m.PackageID, m.Version)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(installed.ExecutablePath)
	if string(data) != "complete" {
		t.Fatal("partial install")
	}
}
