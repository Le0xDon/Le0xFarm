package controllertrust

import (
	"os"
	"testing"

	"github.com/le0xdon/le0xfarm/internal/identity"
)

func TestPairAndReload(t *testing.T) {
	d := t.TempDir()
	c, _ := identity.NewControllerID()
	f, _ := identity.NewFarmID()
	a, _ := identity.NewAgentID()
	h, _ := identity.NewHostID()
	s, err := Open(d, c, f)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Pair(a, h); err != nil {
		t.Fatal(err)
	}
	s, err = Open(d, c, f)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := s.Find(a); !ok || got.HostID != h {
		t.Fatal("pair not persisted")
	}
	if mode := mustMode(t, d+"/paired_agents.json"); mode != 0600 {
		t.Fatalf("mode %o", mode)
	}
}

func TestRejectsIdentityCollisions(t *testing.T) {
	d := t.TempDir()
	c, _ := identity.NewControllerID()
	f, _ := identity.NewFarmID()
	a1, _ := identity.NewAgentID()
	a2, _ := identity.NewAgentID()
	h1, _ := identity.NewHostID()
	h2, _ := identity.NewHostID()
	s, err := Open(d, c, f)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Pair(a1, h1); err != nil {
		t.Fatal(err)
	}
	if err := s.Pair(a1, h2); err == nil {
		t.Fatal("same AgentID with different HostID accepted")
	}
	if err := s.Pair(a2, h1); err == nil {
		t.Fatal("same HostID with different AgentID accepted")
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
