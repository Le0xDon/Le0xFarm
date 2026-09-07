package agentpki_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/le0xdon/le0xfarm/internal/agentpki"
	"github.com/le0xdon/le0xfarm/internal/agenttrust"
	"github.com/le0xdon/le0xfarm/internal/controllerpki"
	"github.com/le0xdon/le0xfarm/internal/farmerr"
	"github.com/le0xdon/le0xfarm/internal/identity"
)

func provision(t *testing.T) (string, agenttrust.Binding, identity.AgentID, identity.HostID) {
	t.Helper()
	root := t.TempDir()
	controllerDir := filepath.Join(root, "controller")
	agentDir := filepath.Join(root, "agent")
	if err := os.Mkdir(controllerDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(agentDir, 0700); err != nil {
		t.Fatal(err)
	}
	cid, _ := identity.NewControllerID()
	fid, _ := identity.NewFarmID()
	p, err := controllerpki.Initialize(controllerDir, cid, fid)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := identity.NewAgentID()
	h, _ := identity.NewHostID()
	e, err := agentpki.NewEnrollment(a, h, fid)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := p.IssueAgent(e.CSRDER, a, h, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	binding := agenttrust.Binding{ControllerID: cid, FarmID: fid}
	if err := agentpki.Save(agentDir, e, cert, p.CACertificateDER(), binding, a, h); err != nil {
		t.Fatal(err)
	}
	return agentDir, binding, a, h
}
func TestStoragePermissionsLoadAndNoOverwrite(t *testing.T) {
	d, b, a, h := provision(t)
	if _, err := agentpki.Load(d, b, a, h); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]os.FileMode{"agent.key": 0600, "agent.crt": 0644, "ca.crt": 0644} {
		i, err := os.Stat(filepath.Join(d, "pki", name))
		if err != nil || i.Mode().Perm() != want {
			t.Fatalf("%s mode=%v err=%v", name, i.Mode().Perm(), err)
		}
	}
	before, err := os.ReadFile(filepath.Join(d, "pki", "agent.key"))
	if err != nil {
		t.Fatal(err)
	}
	if err := agentpki.Save(d, agentpki.Enrollment{}, nil, nil, b, a, h); err == nil {
		t.Fatal("overwrite accepted")
	}
	after, _ := os.ReadFile(filepath.Join(d, "pki", "agent.key"))
	if string(before) != string(after) {
		t.Fatal("private key overwritten")
	}
}
func TestCorruptAndTrustMismatchRejected(t *testing.T) {
	d, b, a, h := provision(t)
	other, _ := identity.NewFarmID()
	bad := b
	bad.FarmID = other
	if _, err := agentpki.Load(d, bad, a, h); code(err) != farmerr.TLS_IDENTITY_MISMATCH {
		t.Fatalf("mismatch=%v", err)
	}
	if err := os.WriteFile(filepath.Join(d, "pki", "agent.crt"), []byte("bad"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := agentpki.Load(d, b, a, h); err == nil {
		t.Fatal("corrupt certificate accepted")
	}
}
func code(err error) farmerr.Code { c, _ := farmerr.CodeOf(err); return c }
