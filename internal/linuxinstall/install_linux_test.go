//go:build linux

package linuxinstall

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

type fakeHost struct {
	account         Account
	actions         []string
	fail            func(string) error
	states          map[string]ServiceState
	unloadOnDisable bool
}

func (host *fakeHost) EnsureServiceAccount(context.Context) (Account, error) {
	host.actions = append(host.actions, "account:"+ServiceUser)
	return host.account, nil
}

func (host *fakeHost) EnsureGPUAccess(context.Context, Account) error {
	host.actions = append(host.actions, "gpu-access-preflight")
	if host.fail != nil {
		return host.fail("gpu-access-preflight")
	}
	return nil
}

func (host *fakeHost) SetOwner(path string, _, _ int) error {
	host.actions = append(host.actions, "owner:"+path)
	if host.fail != nil {
		return host.fail(path)
	}
	return nil
}

func (host *fakeHost) ServiceState(_ context.Context, service string) (ServiceState, error) {
	action := "systemctl-state:" + service
	host.actions = append(host.actions, action)
	if host.fail != nil {
		if err := host.fail(action); err != nil {
			return ServiceState{}, err
		}
	}
	return host.states[service], nil
}

func (host *fakeHost) Systemctl(_ context.Context, args ...string) error {
	action := "systemctl:" + strings.Join(args, " ")
	host.actions = append(host.actions, action)
	if host.fail != nil {
		if err := host.fail(action); err != nil {
			return err
		}
	}
	if host.states == nil {
		host.states = make(map[string]ServiceState)
	}
	if len(args) == 1 && args[0] == "daemon-reload" {
		for service, state := range host.states {
			if !state.Active && !state.Enabled {
				state.Loaded = false
			}
			host.states[service] = state
		}
		return nil
	}
	var service string
	if len(args) >= 2 {
		service = args[len(args)-1]
	}
	state := host.states[service]
	switch args[0] {
	case "enable":
		state.Enabled = true
	case "disable":
		state.Enabled = false
		if len(args) == 3 && args[1] == "--now" {
			state.Active = false
		}
		if host.unloadOnDisable && !state.Active {
			state.Loaded = false
		}
	case "start":
		state.Active = true
	case "stop":
		state.Active = false
	case "reset-failed":
		state.Failed = false
	}
	host.states[service] = state
	return nil
}

func TestInstallRolesLayoutUnitsAndPermissions(t *testing.T) {
	for _, test := range []struct {
		name  string
		roles []Role
	}{{"controller", []Role{RoleController}}, {"agent", []Role{RoleAgent}}, {"both", []Role{RoleController, RoleAgent}}} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			controller := testBinary(t, "controller-binary")
			agent := testBinary(t, "agent-binary")
			host := &fakeHost{account: Account{UID: 123, GID: 456}}
			installer := testInstaller(t, Config{Root: root, Roles: test.roles, ControllerBinary: controller, AgentBinary: agent, Enable: true, Start: true, Host: host})
			if err := installer.Install(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(host.actions) == 0 || host.actions[0] != "account:"+ServiceUser {
				t.Fatalf("service account action missing: %v", host.actions)
			}
			if slices.Contains(test.roles, RoleAgent) && !contains(host.actions, "gpu-access-preflight") {
				t.Fatalf("Agent GPU access was not preflighted: %v", host.actions)
			}
			for _, role := range test.roles {
				binary := filepath.Join(root, "usr/local/bin/le0x-"+string(role))
				assertFile(t, binary, 0755)
				state := filepath.Join(root, "var/lib/le0xfarm", string(role))
				assertDirectory(t, state, 0700)
				config := filepath.Join(root, "etc/le0xfarm", string(role)+".env")
				assertFile(t, config, 0640)
				unitPath := filepath.Join(root, "etc/systemd/system", serviceName(role))
				assertFile(t, unitPath, 0644)
				unit, err := os.ReadFile(unitPath)
				if err != nil {
					t.Fatal(err)
				}
				assertUnit(t, string(unit), role)
				for _, want := range []string{"systemctl:enable " + serviceName(role), "systemctl:start " + serviceName(role)} {
					if !contains(host.actions, want) {
						t.Fatalf("missing %q in %v", want, host.actions)
					}
				}
			}
		})
	}
}

func TestGPUAccessPlanningIsBoundedAndIdempotent(t *testing.T) {
	device := func(path string, gid int, permission os.FileMode) gpuDevicePermission {
		return gpuDevicePermission{path: path, uid: 0, gid: gid, permission: permission}
	}
	for _, test := range []struct {
		name      string
		account   Account
		devices   []gpuDevicePermission
		groups    map[int]string
		want      []string
		wantError bool
	}{
		{name: "cpu only", account: Account{UID: 123, GID: 123}},
		{name: "render", account: Account{UID: 123, GID: 123}, devices: []gpuDevicePermission{device("/dev/dri/renderD128", 104, 0660)}, groups: map[int]string{104: "render"}, want: []string{"render"}},
		{name: "video", account: Account{UID: 123, GID: 123}, devices: []gpuDevicePermission{device("/dev/dri/card0", 44, 0660)}, groups: map[int]string{44: "video"}, want: []string{"video"}},
		{name: "both", account: Account{UID: 123, GID: 123}, devices: []gpuDevicePermission{device("/dev/dri/card0", 44, 0660), device("/dev/dri/renderD128", 104, 0660)}, groups: map[int]string{44: "video", 104: "render"}, want: []string{"render", "video"}},
		{name: "existing membership and unrelated preserved", account: Account{UID: 123, GID: 123, GIDs: []int{44, 777}}, devices: []gpuDevicePermission{device("/dev/dri/card0", 44, 0660)}, groups: map[int]string{44: "video"}},
		{name: "world accessible nvidia", account: Account{UID: 123, GID: 123}, devices: []gpuDevicePermission{device("/dev/nvidia0", 0, 0666)}},
		{name: "unsupported owning group", account: Account{UID: 123, GID: 123}, devices: []gpuDevicePermission{device("/dev/dri/renderD128", 999, 0660)}, groups: map[int]string{999: "unrelated"}, wantError: true},
		{name: "group lacks write", account: Account{UID: 123, GID: 123}, devices: []gpuDevicePermission{device("/dev/dri/renderD128", 104, 0640)}, groups: map[int]string{104: "render"}, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := planGPUAccess(test.account, test.devices, test.groups)
			if (err != nil) != test.wantError || !slices.Equal(got, test.want) {
				t.Fatalf("groups=%v err=%v want=%v wantError=%t", got, err, test.want, test.wantError)
			}
		})
	}
}

func TestConfiguredSystemUIDRangeUsesDistroConfiguration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "login.defs")
	if err := os.WriteFile(path, []byte("UID_MIN 1000\nSYS_UID_MIN 200\nSYS_UID_MAX 899\n"), 0600); err != nil {
		t.Fatal(err)
	}
	minimum, maximum, err := configuredSystemUIDRange(path)
	if err != nil || minimum != 200 || maximum != 899 {
		t.Fatalf("range=%d..%d err=%v", minimum, maximum, err)
	}
}

func TestReusedServiceAccountMustRemainDedicatedLockedAndNoLogin(t *testing.T) {
	validPasswd := []string{"le0x", "x", "450", "450", "", "/nonexistent", "/usr/sbin/nologin"}
	validShadow := []string{"le0x", "!", "", "", "", "", "", "", ""}
	if err := validateServiceAccountRecord("450", "450", "le0x", validPasswd, validShadow, 100, 999); err != nil {
		t.Fatalf("valid service account rejected: %v", err)
	}
	for _, test := range []struct {
		name    string
		uid     string
		group   string
		passwd  []string
		shadow  []string
		minimum int
		maximum int
	}{
		{name: "human primary group", uid: "450", group: "users", passwd: validPasswd, shadow: validShadow, minimum: 100, maximum: 999},
		{name: "interactive shell", uid: "450", group: "le0x", passwd: []string{"le0x", "x", "450", "450", "", "/nonexistent", "/bin/bash"}, shadow: validShadow, minimum: 100, maximum: 999},
		{name: "unlocked password", uid: "450", group: "le0x", passwd: validPasswd, shadow: []string{"le0x", "$6$password"}, minimum: 100, maximum: 999},
		{name: "human uid", uid: "1000", group: "le0x", passwd: []string{"le0x", "x", "1000", "450", "", "/nonexistent", "/usr/sbin/nologin"}, shadow: validShadow, minimum: 100, maximum: 999},
		{name: "passwd identity mismatch", uid: "450", group: "le0x", passwd: []string{"le0x", "x", "451", "450", "", "/nonexistent", "/usr/sbin/nologin"}, shadow: validShadow, minimum: 100, maximum: 999},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := validateServiceAccountRecord(test.uid, "450", test.group, test.passwd, test.shadow, test.minimum, test.maximum); err == nil {
				t.Fatal("unsafe reused account was accepted")
			}
		})
	}
}

func TestReinstallPreservesPersistentStateIdentityAndConfiguration(t *testing.T) {
	root := t.TempDir()
	oldBinary := testBinary(t, "old")
	host := &fakeHost{account: Account{UID: 123, GID: 456}}
	installer := testInstaller(t, Config{Root: root, Roles: []Role{RoleController}, ControllerBinary: oldBinary, Host: host})
	if err := installer.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(root, "var/lib/le0xfarm/controller")
	for name, value := range map[string]string{"farm.db": "persistent-db", "identity.json": "persistent-identity"} {
		if err := os.WriteFile(filepath.Join(state, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	config := filepath.Join(root, "etc/le0xfarm/controller.env")
	if err := os.WriteFile(config, []byte("LE0X_CONTROLLER_LISTEN=127.0.0.2:50051\n"), 0640); err != nil {
		t.Fatal(err)
	}
	newBinary := testBinary(t, "new")
	installer = testInstaller(t, Config{Root: root, Roles: []Role{RoleController}, ControllerBinary: newBinary, Host: host})
	if err := installer.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{"farm.db": "persistent-db", "identity.json": "persistent-identity"} {
		data, err := os.ReadFile(filepath.Join(state, name))
		if err != nil || string(data) != value {
			t.Fatalf("%s changed: %q %v", name, data, err)
		}
	}
	data, err := os.ReadFile(config)
	if err != nil || string(data) != "LE0X_CONTROLLER_LISTEN=127.0.0.2:50051\n" {
		t.Fatalf("configuration changed: %q %v", data, err)
	}
	data, err = os.ReadFile(filepath.Join(root, "usr/local/bin/le0x-controller"))
	if err != nil || string(data) != "new" {
		t.Fatalf("binary was not atomically updated: %q %v", data, err)
	}
}

func TestInstallPreflightsEverySelectedBinaryBeforeHostMutation(t *testing.T) {
	root := t.TempDir()
	host := &fakeHost{account: Account{UID: 123, GID: 456}}
	installer := testInstaller(t, Config{
		Root: root, Roles: []Role{RoleController, RoleAgent},
		ControllerBinary: testBinary(t, "controller"), AgentBinary: filepath.Join(root, "missing-agent"), Host: host,
	})
	if err := installer.Install(context.Background()); err == nil {
		t.Fatal("install with missing Agent binary succeeded")
	}
	if len(host.actions) != 0 {
		t.Fatalf("host was mutated before preflight completed: %v", host.actions)
	}
	if _, err := os.Lstat(filepath.Join(root, "usr")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("filesystem was mutated before preflight completed: %v", err)
	}
}

func TestInstallFailsClosedOnSymlinksAndFailedUpdatePreservesBinary(t *testing.T) {
	t.Run("source symlink", func(t *testing.T) {
		root := t.TempDir()
		realBinary := testBinary(t, "binary")
		symlink := filepath.Join(t.TempDir(), "binary-link")
		if err := os.Symlink(realBinary, symlink); err != nil {
			t.Fatal(err)
		}
		installer := testInstaller(t, Config{Root: root, Roles: []Role{RoleController}, ControllerBinary: symlink, Host: &fakeHost{account: Account{UID: 1, GID: 1}}})
		if err := installer.Install(context.Background()); err == nil {
			t.Fatal("source binary symlink accepted")
		}
	})

	t.Run("state directory symlink", func(t *testing.T) {
		root := t.TempDir()
		if err := os.MkdirAll(filepath.Join(root, "var/lib/le0xfarm"), 0755); err != nil {
			t.Fatal(err)
		}
		outside := t.TempDir()
		if err := os.Symlink(outside, filepath.Join(root, "var/lib/le0xfarm/controller")); err != nil {
			t.Fatal(err)
		}
		installer := testInstaller(t, Config{Root: root, Roles: []Role{RoleController}, ControllerBinary: testBinary(t, "binary"), Host: &fakeHost{account: Account{UID: 1, GID: 1}}})
		if err := installer.Install(context.Background()); err == nil {
			t.Fatal("state-directory symlink accepted")
		}
		if entries, err := os.ReadDir(outside); err != nil || len(entries) != 0 {
			t.Fatalf("symlink target changed: %v %v", entries, err)
		}
	})

	t.Run("destination symlink", func(t *testing.T) {
		root := t.TempDir()
		host := &fakeHost{account: Account{UID: 1, GID: 1}}
		installer := testInstaller(t, Config{Root: root, Roles: []Role{RoleController}, ControllerBinary: testBinary(t, "old"), Host: host})
		if err := installer.Install(context.Background()); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(root, "usr/local/bin/le0x-controller")
		outside := filepath.Join(t.TempDir(), "outside")
		if err := os.WriteFile(outside, []byte("untouched"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(target); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, target); err != nil {
			t.Fatal(err)
		}
		installer = testInstaller(t, Config{Root: root, Roles: []Role{RoleController}, ControllerBinary: testBinary(t, "new"), Host: host})
		if err := installer.Install(context.Background()); err == nil {
			t.Fatal("binary destination symlink accepted")
		}
		data, _ := os.ReadFile(outside)
		if string(data) != "untouched" {
			t.Fatal("symlink target changed")
		}
	})

	t.Run("destination hard link", func(t *testing.T) {
		root := t.TempDir()
		host := &fakeHost{account: Account{UID: 1, GID: 1}}
		installer := testInstaller(t, Config{Root: root, Roles: []Role{RoleController}, ControllerBinary: testBinary(t, "old"), Host: host})
		if err := installer.Install(context.Background()); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(root, "usr/local/bin/le0x-controller")
		otherLink := filepath.Join(t.TempDir(), "installed-hard-link")
		if err := os.Link(target, otherLink); err != nil {
			t.Fatal(err)
		}
		installer = testInstaller(t, Config{Root: root, Roles: []Role{RoleController}, ControllerBinary: testBinary(t, "new"), Host: host})
		if err := installer.Install(context.Background()); err == nil {
			t.Fatal("multiply-linked binary destination accepted")
		}
		data, err := os.ReadFile(otherLink)
		if err != nil || string(data) != "old" {
			t.Fatalf("hard-linked working binary changed: %q %v", data, err)
		}
	})

	t.Run("copy failure", func(t *testing.T) {
		root := t.TempDir()
		host := &fakeHost{account: Account{UID: 1, GID: 1}}
		old := testBinary(t, "working")
		installer := testInstaller(t, Config{Root: root, Roles: []Role{RoleController}, ControllerBinary: old, Host: host})
		if err := installer.Install(context.Background()); err != nil {
			t.Fatal(err)
		}
		host.fail = func(value string) error {
			if strings.Contains(value, ".le0x-install-") {
				return errors.New("injected staged update failure")
			}
			return nil
		}
		installer = testInstaller(t, Config{Root: root, Roles: []Role{RoleController}, ControllerBinary: testBinary(t, "broken-update"), Host: host})
		if err := installer.Install(context.Background()); err == nil {
			t.Fatal("injected update failure succeeded")
		}
		data, err := os.ReadFile(filepath.Join(root, "usr/local/bin/le0x-controller"))
		if err != nil || string(data) != "working" {
			t.Fatalf("working binary changed: %q %v", data, err)
		}
	})
}

func TestFailedUpdateRollsBackCompleteArtifactSet(t *testing.T) {
	for _, test := range []struct {
		name         string
		failSuffix   string
		failReload   bool
		failAction   string
		removeConfig bool
	}{
		{name: "after binary", failSuffix: "/usr/local/bin/le0x-controller"},
		{name: "after unit", failSuffix: "/etc/systemd/system/le0x-controller.service"},
		{name: "after generated config", failSuffix: "/etc/le0xfarm/controller.env", removeConfig: true},
		{name: "daemon reload", failReload: true},
		{name: "enable action", failAction: "systemctl:enable le0x-controller.service"},
		{name: "start action", failAction: "systemctl:start le0x-controller.service"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			host := &fakeHost{account: Account{UID: 123, GID: 456}}
			oldInstaller := testInstaller(t, Config{Root: root, Roles: []Role{RoleController}, ControllerBinary: testBinary(t, "old-binary"), Host: host})
			if err := oldInstaller.Install(context.Background()); err != nil {
				t.Fatal(err)
			}
			paths := roleArtifactPaths(root, RoleController)
			if test.removeConfig {
				if err := os.Remove(paths[2]); err != nil {
					t.Fatal(err)
				}
			}
			before := snapshotPaths(t, paths)
			state := filepath.Join(root, "var/lib/le0xfarm/controller")
			persistent := []string{
				filepath.Join(state, "farm.db"),
				filepath.Join(state, "identity.json"),
				filepath.Join(state, "backups", "manual", "manifest.json"),
				filepath.Join(state, "packages", "package", "miner"),
			}
			for _, path := range persistent {
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("persistent"), 0600); err != nil {
					t.Fatal(err)
				}
			}

			upgrade := testInstaller(t, Config{Root: root, Roles: []Role{RoleController}, ControllerBinary: testBinary(t, "new-binary"), Enable: test.failAction != "", Start: test.failAction == "systemctl:start le0x-controller.service", Host: host})
			if test.failSuffix != "" {
				upgrade.afterPublish = func(path string) error {
					if strings.HasSuffix(path, test.failSuffix) {
						return errors.New("injected publication failure")
					}
					return nil
				}
			}
			reloadFailures := 0
			if test.failReload || test.failAction != "" {
				host.fail = func(value string) error {
					if test.failReload && value == "systemctl:daemon-reload" && reloadFailures == 0 {
						reloadFailures++
						return errors.New("injected daemon-reload failure")
					}
					if value == test.failAction {
						return errors.New("injected daemon-reload failure")
					}
					return nil
				}
			}
			if err := upgrade.Install(context.Background()); err == nil {
				t.Fatal("injected failed update succeeded")
			}
			assertPathSnapshot(t, paths, before)
			for _, path := range persistent {
				if data, err := os.ReadFile(path); err != nil || string(data) != "persistent" {
					t.Fatalf("persistent data changed: %s %q %v", path, data, err)
				}
			}
			assertNoRollbackFiles(t, root)

			host.fail = nil
			upgrade.afterPublish = nil
			if err := upgrade.Install(context.Background()); err != nil {
				t.Fatalf("upgrade after rollback failed: %v", err)
			}
			if data, err := os.ReadFile(roleArtifactPaths(root, RoleController)[0]); err != nil || string(data) != "new-binary" {
				t.Fatalf("successful binary update=%q err=%v", data, err)
			}
		})
	}
}

func TestArtifactRollbackFailureRetainsRecoveryEvidence(t *testing.T) {
	for _, test := range []struct {
		name          string
		failSuffix    string
		triggerSuffix string
		removeConfig  bool
	}{
		{name: "binary restore", failSuffix: "/usr/local/bin/le0x-controller", triggerSuffix: "/etc/systemd/system/le0x-controller.service"},
		{name: "unit restore", failSuffix: "/etc/systemd/system/le0x-controller.service", triggerSuffix: "/etc/systemd/system/le0x-controller.service"},
		{name: "generated config removal", failSuffix: "/etc/le0xfarm/controller.env", triggerSuffix: "/etc/le0xfarm/controller.env", removeConfig: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			host := &fakeHost{account: Account{UID: 123, GID: 456}}
			oldInstaller := testInstaller(t, Config{Root: root, Roles: []Role{RoleController}, ControllerBinary: testBinary(t, "old-binary"), Host: host})
			if err := oldInstaller.Install(context.Background()); err != nil {
				t.Fatal(err)
			}
			if test.removeConfig {
				if err := os.Remove(roleArtifactPaths(root, RoleController)[2]); err != nil {
					t.Fatal(err)
				}
			}
			persistent := filepath.Join(root, "var/lib/le0xfarm/controller/farm.db")
			if err := os.WriteFile(persistent, []byte("persistent"), 0600); err != nil {
				t.Fatal(err)
			}

			upgrade := testInstaller(t, Config{Root: root, Roles: []Role{RoleController}, ControllerBinary: testBinary(t, "new-binary"), Host: host})
			upgrade.afterPublish = func(path string) error {
				if strings.HasSuffix(path, test.triggerSuffix) {
					return errors.New("primary installation failure")
				}
				return nil
			}
			upgrade.beforeArtifactRestore = func(target, _ string) error {
				if strings.HasSuffix(target, test.failSuffix) {
					return errors.New("injected artifact rollback failure")
				}
				return nil
			}
			err := upgrade.Install(context.Background())
			if err == nil || !strings.Contains(err.Error(), "primary installation failure") || !strings.Contains(err.Error(), "injected artifact rollback failure") || !strings.Contains(err.Error(), "manual recovery") {
				t.Fatalf("combined rollback error=%v", err)
			}
			rollbacks := rollbackFiles(t, root)
			if test.removeConfig {
				// A generated config had no prior artifact of its own. The old binary
				// and unit recovery copies still identify and preserve the transaction.
				if len(rollbacks) < 2 {
					t.Fatalf("transaction recovery evidence not retained: %v", rollbacks)
				}
			} else {
				want := ".le0x-rollback-" + filepath.Base(strings.TrimPrefix(test.failSuffix, "/")) + "-"
				found := false
				for _, path := range rollbacks {
					if strings.Contains(filepath.Base(path), want) {
						found = true
						if strings.Contains(test.failSuffix, "/usr/local/bin/") {
							if data, readErr := os.ReadFile(path); readErr != nil || string(data) != "old-binary" {
								t.Fatalf("retained binary rollback copy=%q err=%v", data, readErr)
							}
						}
					}
				}
				if !found {
					t.Fatalf("failed artifact recovery copy not retained: %v", rollbacks)
				}
			}
			if data, readErr := os.ReadFile(persistent); readErr != nil || string(data) != "persistent" {
				t.Fatalf("persistent state changed: %q %v", data, readErr)
			}
		})
	}
}

func TestSystemdStateRollsBackWithArtifacts(t *testing.T) {
	for _, test := range []struct {
		name   string
		before ServiceState
	}{
		{name: "disabled inactive", before: ServiceState{}},
		{name: "enabled inactive", before: ServiceState{Enabled: true}},
		{name: "enabled active", before: ServiceState{Enabled: true, Active: true}},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			host := &fakeHost{account: Account{UID: 123, GID: 456}}
			oldInstaller := testInstaller(t, Config{Root: root, Roles: []Role{RoleController}, ControllerBinary: testBinary(t, "old"), Host: host})
			if err := oldInstaller.Install(context.Background()); err != nil {
				t.Fatal(err)
			}
			paths := roleArtifactPaths(root, RoleController)
			beforeArtifacts := snapshotPaths(t, paths)
			host.states = map[string]ServiceState{ControllerService: test.before}
			upgrade := testInstaller(t, Config{Root: root, Roles: []Role{RoleController}, ControllerBinary: testBinary(t, "new"), Enable: true, Start: true, Host: host})
			upgrade.afterServiceAction = func(action, service string) error {
				if action == "start" && service == ControllerService {
					return errors.New("failure after start")
				}
				return nil
			}
			upgrade.beforeArtifactRestore = func(_, _ string) error {
				if got := host.states[ControllerService].Active; got != test.before.Active {
					t.Fatalf("service active=%t before artifact rollback, want prior state %t; actions=%v", got, test.before.Active, host.actions)
				}
				return nil
			}
			if err := upgrade.Install(context.Background()); err == nil {
				t.Fatal("injected post-start failure succeeded")
			}
			assertPathSnapshot(t, paths, beforeArtifacts)
			if got := host.states[ControllerService]; got != test.before {
				t.Fatalf("service state=%+v want=%+v actions=%v", got, test.before, host.actions)
			}
			assertNoRollbackFiles(t, root)
		})
	}
}

func TestMultiRoleServiceActionsRollBackAsOneTransaction(t *testing.T) {
	for _, failAction := range []string{
		"systemctl:enable " + AgentService,
		"systemctl:start " + AgentService,
	} {
		t.Run(failAction, func(t *testing.T) {
			root := t.TempDir()
			host := &fakeHost{account: Account{UID: 123, GID: 456}}
			oldInstaller := testInstaller(t, Config{Root: root, Roles: []Role{RoleController, RoleAgent}, ControllerBinary: testBinary(t, "old-controller"), AgentBinary: testBinary(t, "old-agent"), Host: host})
			if err := oldInstaller.Install(context.Background()); err != nil {
				t.Fatal(err)
			}
			paths := append(roleArtifactPaths(root, RoleController), roleArtifactPaths(root, RoleAgent)...)
			beforeArtifacts := snapshotPaths(t, paths)
			beforeStates := map[string]ServiceState{
				ControllerService: {},
				AgentService:      {Enabled: true, Active: true},
			}
			host.states = map[string]ServiceState{
				ControllerService: beforeStates[ControllerService],
				AgentService:      beforeStates[AgentService],
			}
			failed := false
			host.fail = func(action string) error {
				if action == failAction && !failed {
					failed = true
					return errors.New("injected later-role action failure")
				}
				return nil
			}
			upgrade := testInstaller(t, Config{Root: root, Roles: []Role{RoleController, RoleAgent}, ControllerBinary: testBinary(t, "new-controller"), AgentBinary: testBinary(t, "new-agent"), Enable: true, Start: true, Host: host})
			if err := upgrade.Install(context.Background()); err == nil {
				t.Fatal("injected multi-role failure succeeded")
			}
			assertPathSnapshot(t, paths, beforeArtifacts)
			for service, want := range beforeStates {
				if got := host.states[service]; got != want {
					t.Fatalf("%s state=%+v want=%+v actions=%v", service, got, want, host.actions)
				}
			}
			assertNoRollbackFiles(t, root)
		})
	}
}

func TestSystemdRollbackFailureRetainsArtifactRecoveryCopies(t *testing.T) {
	root := t.TempDir()
	host := &fakeHost{account: Account{UID: 123, GID: 456}}
	oldInstaller := testInstaller(t, Config{Root: root, Roles: []Role{RoleController}, ControllerBinary: testBinary(t, "old"), Host: host})
	if err := oldInstaller.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	host.states = map[string]ServiceState{ControllerService: {}}
	upgrade := testInstaller(t, Config{Root: root, Roles: []Role{RoleController}, ControllerBinary: testBinary(t, "new"), Enable: true, Start: true, Host: host})
	upgrade.afterServiceAction = func(action, service string) error {
		if action == "start" && service == ControllerService {
			host.fail = func(value string) error {
				if value == "systemctl:stop "+ControllerService {
					return errors.New("injected systemd rollback failure")
				}
				return nil
			}
			return errors.New("primary installation failure")
		}
		return nil
	}
	err := upgrade.Install(context.Background())
	if err == nil || !strings.Contains(err.Error(), "primary installation failure") || !strings.Contains(err.Error(), "injected systemd rollback failure") || !strings.Contains(err.Error(), "rollback copies retained") {
		t.Fatalf("combined rollback error=%v", err)
	}
	if rollbacks := rollbackFiles(t, root); len(rollbacks) != 3 {
		t.Fatalf("rollback copies not retained after service rollback failure: %v", rollbacks)
	}
	if data, readErr := os.ReadFile(roleArtifactPaths(root, RoleController)[0]); readErr != nil || string(data) != "new" {
		t.Fatalf("new coherent artifacts should remain when transaction-started service cannot be stopped: %q %v", data, readErr)
	}
}

func TestPostArtifactSystemdRollbackFailureRetainsRecoveryCopies(t *testing.T) {
	root := t.TempDir()
	host := &fakeHost{account: Account{UID: 123, GID: 456}}
	oldInstaller := testInstaller(t, Config{Root: root, Roles: []Role{RoleController}, ControllerBinary: testBinary(t, "old"), Host: host})
	if err := oldInstaller.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	before := snapshotPaths(t, roleArtifactPaths(root, RoleController))
	host.states = map[string]ServiceState{ControllerService: {}}
	upgrade := testInstaller(t, Config{Root: root, Roles: []Role{RoleController}, ControllerBinary: testBinary(t, "new"), Enable: true, Host: host})
	upgrade.afterServiceAction = func(action, service string) error {
		if action == "enable" && service == ControllerService {
			host.fail = func(value string) error {
				if value == "systemctl:disable "+ControllerService {
					return errors.New("injected enablement rollback failure")
				}
				return nil
			}
			return errors.New("primary installation failure")
		}
		return nil
	}
	err := upgrade.Install(context.Background())
	if err == nil || !strings.Contains(err.Error(), "primary installation failure") || !strings.Contains(err.Error(), "injected enablement rollback failure") || !strings.Contains(err.Error(), "manual recovery") {
		t.Fatalf("combined rollback error=%v", err)
	}
	assertPathSnapshot(t, roleArtifactPaths(root, RoleController), before)
	if rollbacks := rollbackFiles(t, root); len(rollbacks) != 3 {
		t.Fatalf("artifact recovery copies not retained after systemd rollback failure: %v", rollbacks)
	}
}

func rollbackFiles(t *testing.T, root string) []string {
	t.Helper()
	var result []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if strings.HasPrefix(entry.Name(), ".le0x-rollback-") {
			result = append(result, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(result)
	return result
}

func roleArtifactPaths(root string, role Role) []string {
	return []string{
		filepath.Join(root, "usr/local/bin/le0x-"+string(role)),
		filepath.Join(root, "etc/systemd/system", serviceName(role)),
		filepath.Join(root, "etc/le0xfarm", string(role)+".env"),
	}
}

type pathSnapshot struct {
	exists bool
	data   []byte
	mode   os.FileMode
}

func snapshotPaths(t *testing.T, paths []string) []pathSnapshot {
	t.Helper()
	result := make([]pathSnapshot, len(paths))
	for index, path := range paths {
		info, err := os.Stat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		result[index] = pathSnapshot{exists: true, data: data, mode: info.Mode().Perm()}
	}
	return result
}

func assertPathSnapshot(t *testing.T, paths []string, snapshots []pathSnapshot) {
	t.Helper()
	for index, path := range paths {
		snapshot := snapshots[index]
		data, err := os.ReadFile(path)
		if !snapshot.exists {
			if !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("new artifact was not rolled back: %s err=%v", path, err)
			}
			continue
		}
		if err != nil || !slices.Equal(data, snapshot.data) {
			t.Fatalf("artifact differs after rollback: %s data=%q err=%v", path, data, err)
		}
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != snapshot.mode {
			t.Fatalf("artifact mode differs after rollback: %s mode=%v err=%v", path, info.Mode(), err)
		}
	}
}

func assertNoRollbackFiles(t *testing.T, root string) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if strings.HasPrefix(entry.Name(), ".le0x-rollback-") {
			t.Errorf("rollback staging remains: %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestUninstallRemovesInfrastructureAndPreservesData(t *testing.T) {
	root := t.TempDir()
	host := &fakeHost{account: Account{UID: 1, GID: 1}}
	installer := testInstaller(t, Config{Root: root, Roles: []Role{RoleController, RoleAgent}, ControllerBinary: testBinary(t, "controller"), AgentBinary: testBinary(t, "agent"), Host: host})
	if err := installer.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	preserved := []string{
		"var/lib/le0xfarm/controller/identity.json",
		"var/lib/le0xfarm/controller/farm.db",
		"var/lib/le0xfarm/controller/backups/manual/manifest.json",
		"var/lib/le0xfarm/agent/identity.json",
		"var/lib/le0xfarm/agent/trust/controller.json",
		"var/lib/le0xfarm/agent/packages/package/miner",
	}
	for _, relative := range preserved {
		path := filepath.Join(root, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("keep"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	reinstall := testInstaller(t, Config{Root: root, Roles: []Role{RoleController, RoleAgent}, ControllerBinary: testBinary(t, "controller-v2"), AgentBinary: testBinary(t, "agent-v2"), Host: host})
	if err := reinstall.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := installer.Uninstall(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, role := range []Role{RoleController, RoleAgent} {
		for _, removed := range []string{"usr/local/bin/le0x-" + string(role), "etc/systemd/system/" + serviceName(role)} {
			if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(removed))); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("infrastructure remains: %s", removed)
			}
		}
		for _, kept := range []string{"etc/le0xfarm/" + string(role) + ".env"} {
			if data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(kept))); err != nil || string(data) == "" {
				t.Fatalf("persistent file lost: %s %q %v", kept, data, err)
			}
		}
		if state := host.states[serviceName(role)]; state.Active || state.Enabled || state.Loaded || state.Failed {
			t.Fatalf("service state remains after uninstall: %+v", state)
		}
	}
	for _, kept := range preserved {
		if data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(kept))); err != nil || string(data) != "keep" {
			t.Fatalf("persistent file lost: %s %q %v", kept, data, err)
		}
	}
}

func TestUninstallIsIdempotentWhenUnitsAreAlreadyAbsent(t *testing.T) {
	root := t.TempDir()
	host := &fakeHost{account: Account{UID: 1, GID: 1}}
	installer := testInstaller(t, Config{Root: root, Roles: []Role{RoleController, RoleAgent}, ControllerBinary: testBinary(t, "controller"), AgentBinary: testBinary(t, "agent"), Host: host})
	if err := installer.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := installer.Uninstall(context.Background()); err != nil {
		t.Fatal(err)
	}
	host.actions = nil
	if err := installer.Uninstall(context.Background()); err != nil {
		t.Fatalf("second uninstall: %v", err)
	}
	for _, action := range host.actions {
		if strings.HasPrefix(action, "systemctl:disable") || strings.HasPrefix(action, "systemctl:reset-failed") {
			t.Fatalf("absent unit caused state mutation: %v", host.actions)
		}
	}
}

func TestUninstallConvergesLoadedStateWhenUnitFileIsMissing(t *testing.T) {
	root := t.TempDir()
	host := &fakeHost{account: Account{UID: 1, GID: 1}, states: map[string]ServiceState{AgentService: {Enabled: true, Active: true, Loaded: true}}}
	installer := testInstaller(t, Config{Root: root, Roles: []Role{RoleAgent}, AgentBinary: testBinary(t, "agent"), Host: host})
	if err := os.MkdirAll(filepath.Join(root, "usr/local/bin"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "usr/local/bin/le0x-agent"), []byte("binary"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := installer.Uninstall(context.Background()); err != nil {
		t.Fatal(err)
	}
	if state := host.states[AgentService]; state.Active || state.Enabled {
		t.Fatalf("loaded state remains: %+v", state)
	}
	for _, want := range []string{"systemctl:stop " + AgentService, "systemctl:disable " + AgentService} {
		if !contains(host.actions, want) {
			t.Fatalf("missing %q in %v", want, host.actions)
		}
	}
}

func TestUninstallHandlesAllExistingServiceStates(t *testing.T) {
	for _, initial := range []ServiceState{
		{Enabled: true, Active: true, Loaded: true},
		{Enabled: true, Active: false, Loaded: true},
		{Enabled: false, Active: true, Loaded: true},
		{Enabled: false, Active: false, Loaded: true},
		{Enabled: false, Active: false, Loaded: true, Failed: true},
		{Enabled: true, Active: false, Loaded: true, Failed: true},
		{Enabled: false, Active: false, Loaded: false},
	} {
		initial := initial
		t.Run(fmt.Sprintf("enabled_%t_active_%t_loaded_%t_failed_%t", initial.Enabled, initial.Active, initial.Loaded, initial.Failed), func(t *testing.T) {
			root := t.TempDir()
			host := &fakeHost{account: Account{UID: 1, GID: 1}, states: map[string]ServiceState{AgentService: initial}}
			installer := testInstaller(t, Config{Root: root, Roles: []Role{RoleAgent}, AgentBinary: testBinary(t, "agent"), Host: host})
			if err := installer.Install(context.Background()); err != nil {
				t.Fatal(err)
			}
			host.states[AgentService] = initial
			if err := installer.Uninstall(context.Background()); err != nil {
				t.Fatal(err)
			}
			if final := host.states[AgentService]; final.Enabled || final.Active || final.Loaded || final.Failed {
				t.Fatalf("final service state=%+v", final)
			}
		})
	}
}

func TestUninstallSurfacesRealServiceManagerFailure(t *testing.T) {
	for _, action := range []string{"systemctl:stop " + ControllerService, "systemctl:disable " + ControllerService} {
		action := action
		t.Run(action, func(t *testing.T) {
			root := t.TempDir()
			host := &fakeHost{account: Account{UID: 1, GID: 1}, states: map[string]ServiceState{ControllerService: {Enabled: true, Active: true, Loaded: true}}}
			installer := testInstaller(t, Config{Root: root, Roles: []Role{RoleController}, ControllerBinary: testBinary(t, "controller"), Host: host})
			if err := installer.Install(context.Background()); err != nil {
				t.Fatal(err)
			}
			host.states[ControllerService] = ServiceState{Enabled: true, Active: true, Loaded: true}
			want := errors.New("service manager unavailable")
			host.fail = func(got string) error {
				if got == action {
					return want
				}
				return nil
			}
			if err := installer.Uninstall(context.Background()); !errors.Is(err, want) {
				t.Fatalf("error=%v, want %v", err, want)
			}
			if _, err := os.Lstat(filepath.Join(root, "etc/systemd/system", ControllerService)); err != nil {
				t.Fatalf("unit removed after real systemctl failure: %v", err)
			}
		})
	}
}

func TestUninstallDoesNotRemoveUnitWhenResetFailedFails(t *testing.T) {
	root := t.TempDir()
	host := &fakeHost{account: Account{UID: 1, GID: 1}, states: map[string]ServiceState{ControllerService: {Loaded: true, Failed: true}}}
	installer := testInstaller(t, Config{Root: root, Roles: []Role{RoleController}, ControllerBinary: testBinary(t, "controller"), Host: host})
	if err := installer.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	host.states[ControllerService] = ServiceState{Loaded: true, Failed: true}
	want := errors.New("cannot reset failure state")
	host.fail = func(action string) error {
		if action == "systemctl:reset-failed "+ControllerService {
			return want
		}
		return nil
	}
	if err := installer.Uninstall(context.Background()); !errors.Is(err, want) {
		t.Fatalf("error=%v, want %v", err, want)
	}
	if _, err := os.Lstat(filepath.Join(root, "etc/systemd/system", ControllerService)); err != nil {
		t.Fatalf("unit removed after reset-failed failure: %v", err)
	}
}

func TestUninstallRealAcceptanceUnloadAfterDisableDoesNotResetAbsentUnit(t *testing.T) {
	root := t.TempDir()
	host := &fakeHost{
		account:         Account{UID: 1, GID: 1},
		states:          map[string]ServiceState{AgentService: {Enabled: true, Loaded: true}},
		unloadOnDisable: true,
	}
	installer := testInstaller(t, Config{Root: root, Roles: []Role{RoleAgent}, AgentBinary: testBinary(t, "agent"), Host: host})
	if err := installer.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	host.states[AgentService] = ServiceState{Enabled: true, Loaded: true}
	host.fail = func(action string) error {
		if action == "systemctl:reset-failed "+AgentService && !host.states[AgentService].Loaded {
			return errors.New("Unit le0x-agent.service not loaded")
		}
		return nil
	}
	if err := installer.Uninstall(context.Background()); err != nil {
		t.Fatalf("uninstall after disable unloaded the unit: %v actions=%v", err, host.actions)
	}
	if contains(host.actions, "systemctl:reset-failed "+AgentService) {
		t.Fatalf("normal unloaded unit was needlessly reset: %v", host.actions)
	}
	for _, relative := range []string{"usr/local/bin/le0x-agent", "etc/systemd/system/" + AgentService} {
		if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(relative))); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("infrastructure remains after acceptance-shaped uninstall: %s", relative)
		}
	}
}

func TestUninstallDaemonReloadFailureIsFatal(t *testing.T) {
	root := t.TempDir()
	host := &fakeHost{account: Account{UID: 1, GID: 1}}
	installer := testInstaller(t, Config{Root: root, Roles: []Role{RoleController}, ControllerBinary: testBinary(t, "controller"), Host: host})
	if err := installer.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := errors.New("daemon reload failed")
	host.fail = func(action string) error {
		if action == "systemctl:daemon-reload" {
			return want
		}
		return nil
	}
	if err := installer.Uninstall(context.Background()); !errors.Is(err, want) {
		t.Fatalf("error=%v, want %v", err, want)
	}
}

func TestParseServiceState(t *testing.T) {
	for _, test := range []struct {
		name string
		text string
		want ServiceState
	}{
		{"enabled-active", "LoadState=loaded\nActiveState=active\nUnitFileState=enabled\n", ServiceState{Enabled: true, Active: true, Loaded: true}},
		{"failed-loaded", "ActiveState=failed\nUnitFileState=disabled\nLoadState=loaded\n", ServiceState{Loaded: true, Failed: true}},
		{"unloaded", "UnitFileState=\nLoadState=not-found\nActiveState=inactive\n", ServiceState{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseServiceState(AgentService, []byte(test.text))
			if err != nil || got != test.want {
				t.Fatalf("state=%+v err=%v want=%+v", got, err, test.want)
			}
		})
	}
	for _, invalid := range []string{
		"LoadState=loaded\nActiveState=activating\nUnitFileState=enabled\n",
		"LoadState=loaded\nActiveState=inactive\n",
		"LoadState=loaded\nLoadState=loaded\nActiveState=inactive\nUnitFileState=disabled\n",
	} {
		if _, err := parseServiceState(AgentService, []byte(invalid)); err == nil {
			t.Fatalf("invalid state accepted: %q", invalid)
		}
	}
}

func TestSystemctlAllowlist(t *testing.T) {
	for _, args := range [][]string{{"daemon-reload"}, {"enable", ControllerService}, {"disable", AgentService}, {"start", AgentService}, {"stop", ControllerService}, {"disable", "--now", AgentService}, {"reset-failed", ControllerService}} {
		if !validSystemctl(args) {
			t.Fatalf("valid operation rejected: %v", args)
		}
	}
	for _, args := range [][]string{{"restart", AgentService}, {"enable", "attacker.service"}, {"stop", "attacker.service"}, {"daemon-reload", AgentService}} {
		if validSystemctl(args) {
			t.Fatalf("unsafe operation accepted: %v", args)
		}
	}
}

func assertUnit(t *testing.T, unit string, role Role) {
	t.Helper()
	wantedKillMode := "KillMode=mixed"
	if role == RoleAgent {
		wantedKillMode = "KillMode=process"
	}
	for _, want := range []string{"User=le0x", "Group=le0x", "Restart=on-failure", "RestartSec=10s", wantedKillMode, "NoNewPrivileges=true", "ProtectSystem=strict", "UMask=0077", "ExecStart=/usr/local/bin/le0x-" + string(role)} {
		if !strings.Contains(unit, want) {
			t.Fatalf("unit missing %q:\n%s", want, unit)
		}
	}
	if role == RoleAgent {
		for _, broad := range []string{"KillMode=mixed", "KillMode=control-group"} {
			if strings.Contains(unit, broad) {
				t.Fatalf("Agent unit permits broad cgroup termination through %s", broad)
			}
		}
	}
	for _, forbidden := range []string{"User=root", "/bin/sh", "bash", "PASSWORD=", "TOKEN=", "ExecStartPre=", "ExecStop="} {
		if strings.Contains(unit, forbidden) {
			t.Fatalf("unit contains forbidden %q", forbidden)
		}
	}
}

func testInstaller(t *testing.T, config Config) *Installer {
	t.Helper()
	installer, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	return installer
}

func testBinary(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "binary")
	if err := os.WriteFile(path, []byte(contents), 0755); err != nil {
		t.Fatal(err)
	}
	return path
}

func assertFile(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("file %s: %v", path, err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != mode {
		t.Fatalf("file %s mode=%v", path, info.Mode())
	}
}

func assertDirectory(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("directory %s: %v", path, err)
	}
	if !info.IsDir() || info.Mode().Perm() != mode {
		t.Fatalf("directory %s mode=%v", path, info.Mode())
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
