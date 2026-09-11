package agentidentity

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/le0xdon/le0xfarm/internal/farmerr"
)

func TestCreateReloadAndSeparateDirectories(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "agent")
	first, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	if first.HostID.Validate() != nil || first.AgentID.Validate() != nil {
		t.Fatal("invalid generated IDs")
	}
	before, err := os.ReadFile(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatal(err)
	}
	var persisted map[string]json.RawMessage
	if err := json.Unmarshal(before, &persisted); err != nil {
		t.Fatal(err)
	}
	if len(persisted) != 3 || string(persisted["schema_version"]) != "1" {
		t.Fatalf("unexpected persisted schema: %s", before)
	}
	var stored Identity
	if err := json.Unmarshal(before, &stored); err != nil || stored != first {
		t.Fatalf("persisted IDs changed: %v", err)
	}
	second, err := LoadOrCreate(dir)
	if err != nil || first != second {
		t.Fatalf("identity changed: %v", err)
	}
	after, err := os.ReadFile(filepath.Join(dir, FileName))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("existing identity rewritten")
	}
	other, err := LoadOrCreate(filepath.Join(t.TempDir(), "agent"))
	if err != nil || first.HostID == other.HostID || first.AgentID == other.AgentID {
		t.Fatalf("different data dirs share identity: %v", err)
	}
	for path, mode := range map[string]os.FileMode{dir: 0700, filepath.Join(dir, FileName): 0600} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != mode {
			t.Fatalf("wrong permissions for %s: %v", path, err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || entries[0].Name() != FileName {
		t.Fatal("unexpected temporary files")
	}
}

func TestCorruptedIdentityNeverReplaced(t *testing.T) {
	valid := `{"schema_version":1,"host_id":"host_0123456789abcdef0123456789abcdef","agent_id":"agent_0123456789abcdef0123456789abcdef"}`
	cases := []string{"", "{", "null", "{}", valid + "{}", strings.Replace(valid, `"schema_version":1`, `"schema_version":2`, 1), strings.Replace(valid, `"host_id":"host_`, `"host_id":"agent_`, 1), strings.Replace(valid, `"agent_id":"agent_0123456789abcdef0123456789abcdef"`, `"agent_id":null`, 1), strings.Replace(valid, `"schema_version":1`, `"schema_version":1,"schema_version":1`, 1), strings.Replace(valid, `"schema_version":1`, `"schema_version":1,"extra":true`, 1), strings.Repeat(" ", maxFileBytes+1)}
	for i, content := range cases {
		t.Run(string(rune('A'+i)), func(t *testing.T) {
			dir := privateDir(t)
			path := filepath.Join(dir, FileName)
			if err := os.WriteFile(path, []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
			_, err := LoadOrCreate(dir)
			var typed farmerr.Error
			if !errors.As(err, &typed) || typed.Code != farmerr.CONFIG_CONFLICT {
				t.Fatalf("expected typed corruption error: %v", err)
			}
			after, err := os.ReadFile(path)
			if err != nil || string(after) != content {
				t.Fatal("corrupt identity replaced")
			}
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 1 {
				t.Fatal("failure created extra identity files")
			}
		})
	}
}

func TestUnsafeExistingPathsNotReplaced(t *testing.T) {
	for _, kind := range []string{"directory", "symlink", "permissions", "unreadable"} {
		t.Run(kind, func(t *testing.T) {
			dir := privateDir(t)
			path := filepath.Join(dir, FileName)
			var err error
			switch kind {
			case "directory":
				err = os.Mkdir(path, 0700)
			case "symlink":
				err = os.Symlink("missing.json", path)
			default:
				_, err = LoadOrCreate(dir)
				if err == nil {
					mode := os.FileMode(0644)
					if kind == "unreadable" {
						mode = 0000
					}
					err = os.Chmod(path, mode)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			before, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			_, err = LoadOrCreate(dir)
			var typed farmerr.Error
			if !errors.As(err, &typed) {
				t.Fatalf("expected typed error: %v", err)
			}
			after, err := os.Lstat(path)
			if err != nil || !os.SameFile(before, after) || before.Mode() != after.Mode() {
				t.Fatal("existing identity path modified")
			}
		})
	}
	dir := privateDir(t)
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreate(dir); err == nil {
		t.Fatal("insecure directory accepted")
	}
	if _, err := os.Stat(filepath.Join(dir, FileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("identity created in insecure directory")
	}
}

func TestConcurrentFirstRun(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "agent")
	const count = 16
	ids := make([]Identity, count)
	errs := make([]error, count)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			ids[i], errs[i] = LoadOrCreate(dir)
		}()
	}
	close(start)
	wg.Wait()
	for i := range ids {
		if errs[i] != nil || ids[i] != ids[0] {
			t.Fatalf("concurrent identity mismatch: %v", errs[i])
		}
	}
	persisted, err := LoadOrCreate(dir)
	if err != nil || persisted != ids[0] {
		t.Fatalf("persisted identity mismatch: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatal("concurrent creation left temporary files")
	}
}

func TestConcurrentPublishBetweenReadAndExistenceCheck(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "agent")
	afterMissing := make(chan struct{})
	continueLoad := make(chan struct{})
	type result struct {
		id  Identity
		err error
	}
	loser := make(chan result, 1)
	go func() {
		id, err := loadOrCreate(dir, func() {
			close(afterMissing)
			<-continueLoad
		})
		loser <- result{id: id, err: err}
	}()

	<-afterMissing
	winner, err := LoadOrCreate(dir)
	close(continueLoad)
	if err != nil {
		t.Fatalf("publish winning identity: %v", err)
	}
	got := <-loser
	if got.err != nil {
		t.Fatalf("load concurrently published identity: %v", got.err)
	}
	if got.id != winner {
		t.Fatalf("identities did not converge: loser=%+v winner=%+v", got.id, winner)
	}
	persisted, err := LoadOrCreate(dir)
	if err != nil || persisted != winner {
		t.Fatalf("persisted identity mismatch: %v", err)
	}
}

func TestDataDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("LE0X_DATA_DIR", "")
	for _, xdg := range []string{"", "relative/path"} {
		t.Setenv("XDG_DATA_HOME", xdg)
		got, err := DataDir()
		if err != nil || got != filepath.Join(home, ".local/share/le0xfarm/agent") {
			t.Fatalf("default: %s %v", got, err)
		}
	}
	xdg := filepath.Join(home, "data")
	t.Setenv("XDG_DATA_HOME", xdg)
	if got, err := DataDir(); err != nil || got != filepath.Join(xdg, "le0xfarm/agent") {
		t.Fatalf("XDG: %s %v", got, err)
	}
	override := filepath.Join(home, "override")
	t.Setenv("LE0X_DATA_DIR", override)
	if got, err := DataDir(); err != nil || got != override {
		t.Fatalf("override: %s %v", got, err)
	}
	t.Setenv("LE0X_DATA_DIR", "relative-override")
	expected, err := filepath.Abs("relative-override")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := DataDir(); err != nil || got != expected {
		t.Fatalf("relative override: %s %v", got, err)
	}
	t.Setenv("LE0X_DATA_DIR", "")
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("HOME", "")
	if _, err := DataDir(); err == nil {
		t.Fatal("missing home accepted")
	}
}

func privateDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestIdentitySchemaVersion(t *testing.T) {
	const host = "host_0123456789abcdef0123456789abcdef"
	const agent = "agent_0123456789abcdef0123456789abcdef"
	for _, tc := range []struct {
		name   string
		prefix string
		valid  bool
	}{
		{"version_1", `"schema_version":1,`, true},
		{"missing_version", "", false},
		{"future_version_999", `"schema_version":999,`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := privateDir(t)
			path := filepath.Join(dir, FileName)
			content := []byte(`{` + tc.prefix + `"host_id":"` + host + `","agent_id":"` + agent + `"}`)
			if err := os.WriteFile(path, content, 0600); err != nil {
				t.Fatal(err)
			}
			before, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			id, err := LoadOrCreate(dir)
			if tc.valid {
				if err != nil || id.HostID.String() != host || id.AgentID.String() != agent {
					t.Fatalf("version 1 not loaded: %v", err)
				}
			} else {
				var typed farmerr.Error
				if !errors.As(err, &typed) || typed.Code != farmerr.CONFIG_CONFLICT {
					t.Fatalf("expected typed version error, got %v", err)
				}
				if id != (Identity{}) {
					t.Fatal("identity returned despite unsupported schema")
				}
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(content, after) {
				t.Fatal("original identity content changed")
			}
			info, err := os.Stat(path)
			if err != nil || !os.SameFile(before, info) {
				t.Fatal("original identity file replaced")
			}
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 1 {
				t.Fatal("unexpected identity files created")
			}
		})
	}
}
