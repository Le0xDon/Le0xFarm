package controlleridentity

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/le0xdon/le0xfarm/internal/farmerr"
)

func privateDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestCreateReloadAndSeparateDirectories(t *testing.T) {
	dir := filepath.Join(privateDir(t), "controller")
	first, err := Initialize(dir)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	if len(fields) != 3 || string(fields["schema_version"]) != "1" {
		t.Fatalf("invalid persisted identity: %s", data)
	}
	second, err := Load(dir)
	if err != nil || first != second {
		t.Fatalf("identity changed: %v", err)
	}
	other, err := Initialize(filepath.Join(privateDir(t), "controller"))
	if err != nil {
		t.Fatal(err)
	}
	if first == other {
		t.Fatal("different data directories share identity")
	}
	for path, mode := range map[string]os.FileMode{dir: 0700, filepath.Join(dir, FileName): 0600} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != mode {
			t.Fatalf("permissions for %s: %v", path, err)
		}
	}
}

func TestMissingIdentityRequiresExplicitInitialization(t *testing.T) {
	dir := filepath.Join(privateDir(t), "controller")
	if _, err := Load(dir); err == nil {
		t.Fatal("missing identity loaded")
	} else {
		var typed farmerr.Error
		if !errors.As(err, &typed) || typed.Code != farmerr.CONFIG_CONFLICT {
			t.Fatalf("wrong error: %v", err)
		}
	}
	id, err := Initialize(dir)
	if err != nil {
		t.Fatal(err)
	}
	if id.ControllerID.Validate() != nil || id.FarmID.Validate() != nil {
		t.Fatal("invalid initialized IDs")
	}
}

func TestExistingIdentityRejectsInitAndPhysicalLossNeedsExplicitInit(t *testing.T) {
	parent := privateDir(t)
	dir := filepath.Join(parent, "controller")
	first, err := Initialize(dir)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, FileName)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Initialize(dir); err == nil {
		t.Fatal("--init regenerated existing identity")
	} else {
		var typed farmerr.Error
		if !errors.As(err, &typed) || typed.Code != farmerr.CONFIG_CONFLICT {
			t.Fatalf("wrong existing identity error: %v", err)
		}
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("existing identity changed")
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil {
		t.Fatal("missing identity accepted after physical loss")
	}
	second, err := Initialize(dir)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("explicit reinitialization reused deleted identity unexpectedly")
	}
}

func TestIdentityIndependentOfHostnameAndListenAddress(t *testing.T) {
	dir := filepath.Join(privateDir(t), "controller")
	first, err := Initialize(dir)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatal("hostname/listen changes must not affect persisted identity")
	}
}

func TestSchemaAndCorruptionRejectedWithoutReplacement(t *testing.T) {
	cases := []string{`{"controller_id":"controller_0123456789abcdef0123456789abcdef","farm_id":"farm_0123456789abcdef0123456789abcdef"}`, `{"schema_version":999,"controller_id":"controller_0123456789abcdef0123456789abcdef","farm_id":"farm_0123456789abcdef0123456789abcdef"}`, `{"schema_version":1,"controller_id":"bad","farm_id":"farm_0123456789abcdef0123456789abcdef"}`, `{"schema_version":1,"controller_id":"controller_0123456789abcdef0123456789abcdef","farm_id":"bad"}`, `{`, `null`}
	for i, content := range cases {
		t.Run(string(rune('A'+i)), func(t *testing.T) {
			dir := privateDir(t)
			path := filepath.Join(dir, FileName)
			if err := os.WriteFile(path, []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			_, err = Load(dir)
			var typed farmerr.Error
			if !errors.As(err, &typed) || typed.Code != farmerr.CONFIG_CONFLICT {
				t.Fatalf("expected CONFIG_CONFLICT, got %v", err)
			}
			after, readErr := os.ReadFile(path)
			if readErr != nil || !bytes.Equal(before, after) {
				t.Fatal("identity file changed after error")
			}
		})
	}
}

func TestConcurrentFirstInitialization(t *testing.T) {
	dir := filepath.Join(privateDir(t), "controller")
	ids := make([]Identity, 16)
	errs := make([]error, 16)
	var wg sync.WaitGroup
	for i := range ids {
		wg.Add(1)
		go func(i int) { defer wg.Done(); ids[i], errs[i] = Initialize(dir) }(i)
	}
	wg.Wait()
	var winner Identity
	for i := range ids {
		if errs[i] == nil {
			winner = ids[i]
			break
		}
	}
	if winner == (Identity{}) {
		t.Fatalf("no concurrent initializer succeeded: %v", errs)
	}
	loaded, err := Load(dir)
	if err != nil || loaded != winner {
		t.Fatalf("concurrent identity mismatch: %v", err)
	}
	for i := range ids {
		if errs[i] == nil && ids[i] != winner {
			t.Fatalf("two different identities initialized: %v", errs[i])
		}
	}
}

func TestDataDirOverrideAndXDG(t *testing.T) {
	home := privateDir(t)
	t.Setenv("HOME", home)
	t.Setenv("LE0X_CONTROLLER_DATA_DIR", "")
	t.Setenv("XDG_DATA_HOME", "")
	got, err := DataDir()
	if err != nil || got != filepath.Join(home, ".local/share/le0xfarm/controller") {
		t.Fatalf("default path: %s %v", got, err)
	}
	xdg := filepath.Join(home, "xdg")
	t.Setenv("XDG_DATA_HOME", xdg)
	got, err = DataDir()
	if err != nil || got != filepath.Join(xdg, "le0xfarm/controller") {
		t.Fatalf("XDG path: %s %v", got, err)
	}
	override := filepath.Join(home, "override")
	t.Setenv("LE0X_CONTROLLER_DATA_DIR", override)
	got, err = DataDir()
	if err != nil || got != override {
		t.Fatalf("override path: %s %v", got, err)
	}
}
