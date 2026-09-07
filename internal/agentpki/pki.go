// Package agentpki owns an Agent's private key and issued mTLS credentials.
package agentpki

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/le0xdon/le0xfarm/internal/agenttrust"
	"github.com/le0xdon/le0xfarm/internal/controllerpki"
	"github.com/le0xdon/le0xfarm/internal/farmerr"
	"github.com/le0xdon/le0xfarm/internal/identity"
)

const DirName = "pki"

type Enrollment struct {
	PrivateKey ed25519.PrivateKey
	CSRDER     []byte
}
type Credentials struct {
	Certificate  tls.Certificate
	CA           *x509.Certificate
	ControllerID identity.ControllerID
	FarmID       identity.FarmID
}

func NewEnrollment(agentID identity.AgentID, hostID identity.HostID, farmID identity.FarmID) (Enrollment, error) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return Enrollment{}, err
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: "Le0xAgent"}, URIs: []*url.URL{controllerpki.AgentURI(farmID, agentID, hostID)}}, key)
	if err != nil {
		return Enrollment{}, err
	}
	return Enrollment{PrivateKey: key, CSRDER: csr}, nil
}

func Load(dataDir string, binding agenttrust.Binding, agentID identity.AgentID, hostID identity.HostID) (Credentials, error) {
	dir := filepath.Join(dataDir, DirName)
	info, err := os.Stat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return Credentials{}, farmerr.Error{Code: farmerr.TLS_CREDENTIALS_REQUIRED, HumanMessage: "Agent TLS credentials are not provisioned", SuggestedFix: "Open Controller pairing and use --pair-stdin with --tls-fingerprint."}
	}
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		return Credentials{}, invalid("invalid Agent PKI directory", err)
	}
	caPEM, err := readMode(dir, "ca.crt", 0644)
	if err != nil {
		return Credentials{}, err
	}
	certPEM, err := readMode(dir, "agent.crt", 0644)
	if err != nil {
		return Credentials{}, err
	}
	keyPEM, err := readMode(dir, "agent.key", 0600)
	if err != nil {
		return Credentials{}, err
	}
	ca, err := parseCert(caPEM)
	if err != nil {
		return Credentials{}, invalid("invalid Farm CA", err)
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return Credentials{}, invalid("invalid Agent certificate/key", err)
	}
	cert, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return Credentials{}, invalid("invalid Agent certificate", err)
	}
	if err := controllerpki.VerifyAgentCertificate(cert, ca, binding.FarmID, agentID, hostID, time.Now()); err != nil {
		return Credentials{}, err
	}
	return Credentials{Certificate: pair, CA: ca, ControllerID: binding.ControllerID, FarmID: binding.FarmID}, nil
}

func Save(dataDir string, enrollment Enrollment, certDER, caDER []byte, binding agenttrust.Binding, agentID identity.AgentID, hostID identity.HostID) error {
	dir := filepath.Join(dataDir, DirName)
	if _, err := os.Lstat(dir); err == nil {
		return invalid("Agent PKI already exists", nil)
	} else if !errors.Is(err, os.ErrNotExist) {
		return invalid("cannot inspect Agent PKI", err)
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		return invalid("invalid returned Farm CA", err)
	}
	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		return invalid("invalid returned Agent certificate", err)
	}
	if err := controllerpki.VerifyAgentCertificate(cert, ca, binding.FarmID, agentID, hostID, time.Now()); err != nil {
		return err
	}
	if !enrollment.PrivateKey.Public().(ed25519.PublicKey).Equal(cert.PublicKey) {
		return farmerr.Error{Code: farmerr.TLS_IDENTITY_MISMATCH, HumanMessage: "Issued Agent certificate does not match local private key"}
	}
	if err := os.Mkdir(dir, 0700); err != nil {
		return invalid("cannot create Agent PKI directory", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(enrollment.PrivateKey)
	if err != nil {
		return err
	}
	files := []struct {
		name string
		data []byte
		mode os.FileMode
	}{{"ca.crt", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), 0644}, {"agent.crt", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER}), 0644}, {"agent.key", pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0600}}
	for _, f := range files {
		if err := writeAtomic(dir, f.name, f.data, f.mode); err != nil {
			return err
		}
	}
	return syncDir(dir)
}

func TLSConfig(c Credentials) *tls.Config {
	pool := x509.NewCertPool()
	pool.AddCert(c.CA)
	return &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: pool, Certificates: []tls.Certificate{c.Certificate}, ServerName: controllerpki.ServerName(c.ControllerID), VerifyConnection: func(state tls.ConnectionState) error {
		if len(state.PeerCertificates) == 0 {
			return farmerr.Error{Code: farmerr.TLS_IDENTITY_MISMATCH, HumanMessage: "Controller did not present a certificate"}
		}
		cert := state.PeerCertificates[0]
		now := time.Now()
		if now.Before(cert.NotBefore) || now.After(cert.NotAfter) {
			return farmerr.Error{Code: farmerr.CERTIFICATE_EXPIRED, HumanMessage: "Controller certificate is not currently valid"}
		}
		if _, err := cert.Verify(x509.VerifyOptions{Roots: pool, DNSName: controllerpki.ServerName(c.ControllerID), CurrentTime: now, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
			return farmerr.Error{Code: farmerr.TLS_IDENTITY_MISMATCH, HumanMessage: "Controller certificate is not trusted"}
		}
		controllerID, farmID, err := controllerpki.ParseControllerIdentity(cert)
		if err != nil || controllerID != c.ControllerID || farmID != c.FarmID {
			return farmerr.Error{Code: farmerr.CONTROLLER_IDENTITY_MISMATCH, HumanMessage: "Controller certificate identity differs from trusted binding"}
		}
		return nil
	}}
}
func parseCert(data []byte) (*x509.Certificate, error) {
	b, _ := pem.Decode(data)
	if b == nil {
		return nil, errors.New("missing certificate PEM")
	}
	return x509.ParseCertificate(b.Bytes)
}
func readMode(dir, name string, mode os.FileMode) ([]byte, error) {
	p := filepath.Join(dir, name)
	i, err := os.Stat(p)
	if err != nil {
		return nil, invalid("cannot read Agent PKI", err)
	}
	if !i.Mode().IsRegular() || i.Mode().Perm() != mode {
		return nil, invalid("invalid Agent PKI permissions", fmt.Errorf("%s must be %04o", name, mode))
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, invalid("cannot read Agent PKI", err)
	}
	return b, nil
}
func writeAtomic(dir, name string, data []byte, mode os.FileMode) error {
	f, err := os.CreateTemp(dir, "."+name+"-")
	if err != nil {
		return invalid("cannot create Agent PKI file", err)
	}
	n := f.Name()
	defer os.Remove(n)
	if err = f.Chmod(mode); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	if e := f.Close(); err == nil {
		err = e
	}
	if err != nil {
		return invalid("cannot persist Agent PKI", err)
	}
	if _, err = os.Stat(filepath.Join(dir, name)); err == nil {
		return invalid("Agent PKI file already exists", nil)
	}
	if err = os.Rename(n, filepath.Join(dir, name)); err != nil {
		return invalid("cannot publish Agent PKI", err)
	}
	return nil
}
func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
func invalid(msg string, err error) error {
	d := map[string]string{}
	if err != nil {
		d["reason"] = err.Error()
	}
	return farmerr.Error{Code: farmerr.CONFIG_CONFLICT, HumanMessage: msg, Details: d}
}
