package controllerpki_test

import (
	"crypto/tls"
	"crypto/x509"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/le0xdon/le0xfarm/internal/agentpki"
	"github.com/le0xdon/le0xfarm/internal/controllerpki"
	"github.com/le0xdon/le0xfarm/internal/farmerr"
	"github.com/le0xdon/le0xfarm/internal/identity"
)

func TestInitializeLoadAndIssue(t *testing.T) {
	d := t.TempDir()
	cid, _ := identity.NewControllerID()
	fid, _ := identity.NewFarmID()
	p, err := controllerpki.Initialize(d, cid, fid)
	if err != nil {
		t.Fatal(err)
	}
	first := append([]byte(nil), p.CA.Raw...)
	loaded, err := controllerpki.Load(d, cid, fid)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(loaded.CA.Raw) {
		t.Fatal("CA changed")
	}
	if _, err := controllerpki.Initialize(d, cid, fid); err == nil {
		t.Fatal("repeated init accepted")
	}
	for name, want := range map[string]os.FileMode{"ca.key": 0600, "controller.key": 0600, "ca.crt": 0644, "controller.crt": 0644} {
		i, err := os.Stat(filepath.Join(d, "pki", name))
		if err != nil || i.Mode().Perm() != want {
			t.Fatalf("%s mode=%v err=%v", name, i.Mode().Perm(), err)
		}
	}
	if p.CA.NotAfter.Sub(p.CA.NotBefore) < 9*365*24*time.Hour {
		t.Fatal("CA validity too short")
	}
	if p.Controller.NotAfter.Sub(p.Controller.NotBefore) < 350*24*time.Hour {
		t.Fatal("Controller validity too short")
	}
	a, _ := identity.NewAgentID()
	h, _ := identity.NewHostID()
	enrollment, err := agentpki.NewEnrollment(a, h, fid)
	if err != nil {
		t.Fatal(err)
	}
	der, err := p.IssueAgent(enrollment.CSRDER, a, h, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	if err := controllerpki.VerifyAgentCertificate(cert, p.CA, fid, a, h, time.Now()); err != nil {
		t.Fatal(err)
	}
	if cert.NotAfter.Sub(cert.NotBefore) < 175*24*time.Hour {
		t.Fatal("Agent validity too short")
	}
	if _, err := os.Stat(filepath.Join(d, "pki", "agent.key")); !os.IsNotExist(err) {
		t.Fatal("Agent private key stored Controller-side")
	}
	otherAgent, _ := identity.NewAgentID()
	if code(controllerpki.VerifyAgentCertificate(cert, p.CA, fid, otherAgent, h, time.Now())) != farmerr.TLS_IDENTITY_MISMATCH {
		t.Fatal("wrong AgentID accepted")
	}
	otherHost, _ := identity.NewHostID()
	if code(controllerpki.VerifyAgentCertificate(cert, p.CA, fid, a, otherHost, time.Now())) != farmerr.TLS_IDENTITY_MISMATCH {
		t.Fatal("wrong HostID accepted")
	}
	otherFarm, _ := identity.NewFarmID()
	if code(controllerpki.VerifyAgentCertificate(cert, p.CA, otherFarm, a, h, time.Now())) != farmerr.TLS_IDENTITY_MISMATCH {
		t.Fatal("wrong FarmID accepted")
	}
	if code(controllerpki.VerifyAgentCertificate(cert, p.CA, fid, a, h, cert.NotAfter.Add(time.Second))) != farmerr.CERTIFICATE_EXPIRED {
		t.Fatal("expired certificate accepted")
	}
	if _, err := p.IssueAgent([]byte("bad"), a, h, time.Now()); err == nil {
		t.Fatal("malformed CSR accepted")
	}
	if p.TLSConfig(false).MinVersion != tls.VersionTLS13 || p.TLSConfig(false).ClientAuth != tls.RequireAndVerifyClientCert {
		t.Fatal("normal TLS policy is not TLS 1.3 mutual TLS")
	}
}

func TestMissingCorruptAndIdentityMismatch(t *testing.T) {
	d := t.TempDir()
	cid, _ := identity.NewControllerID()
	fid, _ := identity.NewFarmID()
	if _, err := controllerpki.Load(d, cid, fid); code(err) != farmerr.TLS_CREDENTIALS_REQUIRED {
		t.Fatalf("missing: %v", err)
	}
	if _, err := controllerpki.Initialize(d, cid, fid); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "pki", "ca.crt"), []byte("bad"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := controllerpki.Load(d, cid, fid); err == nil {
		t.Fatal("corrupt CA accepted")
	}
}
func code(err error) farmerr.Code { c, _ := farmerr.CodeOf(err); return c }
