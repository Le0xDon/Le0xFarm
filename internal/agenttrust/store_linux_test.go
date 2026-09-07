package agenttrust

import (
	"errors"
	"github.com/le0xdon/le0xfarm/internal/identity"
	"os"
	"testing"
)

func TestSaveLoadAndCorruption(t *testing.T) {
	d := t.TempDir()
	c, _ := identity.NewControllerID()
	f, _ := identity.NewFarmID()
	if err := Save(d, Binding{c, f}); err != nil {
		t.Fatal(err)
	}
	got, err := Load(d)
	if err != nil || got.ControllerID != c || got.FarmID != f {
		t.Fatalf("load %v", err)
	}
	if mode := mustMode(t, d+"/controller.json"); mode != 0600 {
		t.Fatalf("mode %o", mode)
	}
	if err := os.WriteFile(d+"/controller.json", []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(d); err == nil || errors.Is(err, os.ErrNotExist) {
		t.Fatal("corruption accepted")
	}
}
func mustMode(t *testing.T, p string) os.FileMode {
	t.Helper()
	i, e := os.Stat(p)
	if e != nil {
		t.Fatal(e)
	}
	return i.Mode().Perm()
}
