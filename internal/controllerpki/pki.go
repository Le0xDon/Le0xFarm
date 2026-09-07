// Package controllerpki owns the persistent Farm CA and Controller TLS identity.
package controllerpki

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/le0xdon/le0xfarm/internal/farmerr"
	"github.com/le0xdon/le0xfarm/internal/identity"
)

const DirName = "pki"

type PKI struct {
	CA            *x509.Certificate
	caKey         ed25519.PrivateKey
	Controller    *x509.Certificate
	ControllerTLS tls.Certificate
	ControllerID  identity.ControllerID
	FarmID        identity.FarmID
}

func Initialize(dataDir string, controllerID identity.ControllerID, farmID identity.FarmID) (*PKI, error) {
	pkiDir := filepath.Join(dataDir, DirName)
	if _, err := os.Lstat(pkiDir); err == nil {
		return nil, conflict("Controller PKI is already initialized", nil)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, conflict("cannot inspect Controller PKI", err)
	}
	if err := os.Mkdir(pkiDir, 0700); err != nil {
		return nil, conflict("cannot create Controller PKI directory", err)
	}
	now := time.Now().UTC()
	caPub, caKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	caTemplate := &x509.Certificate{SerialNumber: serial(), Subject: pkix.Name{CommonName: "Le0xFarm CA " + farmID.String()}, NotBefore: now.Add(-5 * time.Minute), NotAfter: now.AddDate(10, 0, 0), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature, URIs: []*url.URL{farmURI(farmID)}}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, caPub, caKey)
	if err != nil {
		return nil, err
	}
	controllerPub, controllerKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	controllerTemplate := &x509.Certificate{SerialNumber: serial(), Subject: pkix.Name{CommonName: "Le0xController"}, NotBefore: now.Add(-5 * time.Minute), NotAfter: now.AddDate(1, 0, 0), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, DNSNames: []string{ServerName(controllerID)}, URIs: []*url.URL{controllerURI(farmID, controllerID)}}
	controllerDER, err := x509.CreateCertificate(rand.Reader, controllerTemplate, caTemplate, controllerPub, caKey)
	if err != nil {
		return nil, err
	}
	caKeyDER, err := x509.MarshalPKCS8PrivateKey(caKey)
	if err != nil {
		return nil, err
	}
	controllerKeyDER, err := x509.MarshalPKCS8PrivateKey(controllerKey)
	if err != nil {
		return nil, err
	}
	files := []struct {
		name string
		data []byte
		mode os.FileMode
	}{{"ca.crt", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), 0644}, {"ca.key", pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: caKeyDER}), 0600}, {"controller.crt", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: controllerDER}), 0644}, {"controller.key", pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: controllerKeyDER}), 0600}}
	for _, f := range files {
		if err := writeAtomic(pkiDir, f.name, f.data, f.mode); err != nil {
			return nil, err
		}
	}
	if err := syncDir(pkiDir); err != nil {
		return nil, err
	}
	return Load(dataDir, controllerID, farmID)
}

func Load(dataDir string, controllerID identity.ControllerID, farmID identity.FarmID) (*PKI, error) {
	dir := filepath.Join(dataDir, DirName)
	info, err := os.Stat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, farmerr.Error{Code: farmerr.TLS_CREDENTIALS_REQUIRED, HumanMessage: "Controller PKI is not initialized", SuggestedFix: "Run le0x-controller --init-pki once for this existing Controller identity."}
	}
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		return nil, conflict("invalid Controller PKI directory", err)
	}
	caPEM, err := readMode(dir, "ca.crt", 0644)
	if err != nil {
		return nil, err
	}
	caKeyPEM, err := readMode(dir, "ca.key", 0600)
	if err != nil {
		return nil, err
	}
	certPEM, err := readMode(dir, "controller.crt", 0644)
	if err != nil {
		return nil, err
	}
	keyPEM, err := readMode(dir, "controller.key", 0600)
	if err != nil {
		return nil, err
	}
	ca, err := parseCertPEM(caPEM)
	if err != nil {
		return nil, conflict("invalid Farm CA certificate", err)
	}
	keyAny, err := parseKeyPEM(caKeyPEM)
	if err != nil {
		return nil, conflict("invalid Farm CA private key", err)
	}
	caKey, ok := keyAny.(ed25519.PrivateKey)
	if !ok {
		return nil, conflict("Farm CA key is not Ed25519", nil)
	}
	tlsCert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, conflict("invalid Controller certificate/key", err)
	}
	controller, err := x509.ParseCertificate(tlsCert.Certificate[0])
	if err != nil {
		return nil, conflict("invalid Controller certificate", err)
	}
	if !ca.IsCA || !caKey.Public().(ed25519.PublicKey).Equal(ca.PublicKey) {
		return nil, conflict("Farm CA certificate and key do not match", nil)
	}
	if err := verifyController(controller, ca, controllerID, farmID, time.Now()); err != nil {
		return nil, err
	}
	if !hasFarmURI(ca, farmID) {
		return nil, conflict("Farm CA identity does not match FarmID", nil)
	}
	return &PKI{CA: ca, caKey: caKey, Controller: controller, ControllerTLS: tlsCert, ControllerID: controllerID, FarmID: farmID}, nil
}

func (p *PKI) TLSConfig(pairing bool) *tls.Config {
	pool := x509.NewCertPool()
	pool.AddCert(p.CA)
	auth := tls.RequireAndVerifyClientCert
	if pairing {
		auth = tls.VerifyClientCertIfGiven
	}
	return &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{p.ControllerTLS}, ClientCAs: pool, ClientAuth: auth}
}
func (p *PKI) CACertificateDER() []byte { return append([]byte(nil), p.CA.Raw...) }
func (p *PKI) ServerFingerprint() string {
	sum := sha256.Sum256(p.Controller.Raw)
	return "SHA256:" + strings.ToUpper(hex.EncodeToString(sum[:]))
}

func (p *PKI) IssueAgent(csrDER []byte, agentID identity.AgentID, hostID identity.HostID, now time.Time) ([]byte, error) {
	csr, err := x509.ParseCertificateRequest(csrDER)
	if err != nil {
		return nil, conflict("invalid Agent CSR", err)
	}
	if err = csr.CheckSignature(); err != nil {
		return nil, conflict("invalid Agent CSR signature", err)
	}
	if !hasAgentURI(csr.URIs, p.FarmID, agentID, hostID) {
		return nil, farmerr.Error{Code: farmerr.TLS_IDENTITY_MISMATCH, HumanMessage: "Agent CSR identity does not match AgentHello"}
	}
	template := &x509.Certificate{SerialNumber: serial(), Subject: pkix.Name{CommonName: "Le0xAgent"}, NotBefore: now.Add(-5 * time.Minute), NotAfter: now.AddDate(0, 6, 0), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, URIs: []*url.URL{agentURI(p.FarmID, agentID, hostID)}}
	return x509.CreateCertificate(rand.Reader, template, p.CA, csr.PublicKey, p.caKey)
}
func VerifyAgentCertificate(cert *x509.Certificate, ca *x509.Certificate, farmID identity.FarmID, agentID identity.AgentID, hostID identity.HostID, now time.Time) error {
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	if _, err := cert.Verify(x509.VerifyOptions{Roots: pool, CurrentTime: now, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		if now.Before(cert.NotBefore) || now.After(cert.NotAfter) {
			return farmerr.Error{Code: farmerr.CERTIFICATE_EXPIRED, HumanMessage: "Agent certificate is not currently valid"}
		}
		return farmerr.Error{Code: farmerr.TLS_IDENTITY_MISMATCH, HumanMessage: "Agent certificate does not chain to Farm CA"}
	}
	if !hasAgentURI(cert.URIs, farmID, agentID, hostID) {
		return farmerr.Error{Code: farmerr.TLS_IDENTITY_MISMATCH, HumanMessage: "Agent certificate identity does not match AgentHello"}
	}
	return nil
}

func ServerName(id identity.ControllerID) string { return id.String() + ".controller.le0xfarm" }
func ParseControllerIdentity(cert *x509.Certificate) (identity.ControllerID, identity.FarmID, error) {
	for _, u := range cert.URIs {
		parts := strings.Split(strings.Trim(u.Path, "/"), "/")
		if u.Scheme == "le0xfarm" && u.Host == "identity" && len(parts) == 4 && parts[0] == "farm" && parts[2] == "controller" {
			f, e := identity.ParseFarmID(parts[1])
			if e != nil {
				return identity.ControllerID{}, identity.FarmID{}, e
			}
			c, e := identity.ParseControllerID(parts[3])
			return c, f, e
		}
	}
	return identity.ControllerID{}, identity.FarmID{}, errors.New("Controller identity URI SAN missing")
}
func farmURI(f identity.FarmID) *url.URL {
	return &url.URL{Scheme: "le0xfarm", Host: "identity", Path: "/farm/" + f.String()}
}
func controllerURI(f identity.FarmID, c identity.ControllerID) *url.URL {
	return &url.URL{Scheme: "le0xfarm", Host: "identity", Path: "/farm/" + f.String() + "/controller/" + c.String()}
}
func AgentURI(f identity.FarmID, a identity.AgentID, h identity.HostID) *url.URL {
	return agentURI(f, a, h)
}
func agentURI(f identity.FarmID, a identity.AgentID, h identity.HostID) *url.URL {
	return &url.URL{Scheme: "le0xfarm", Host: "identity", Path: "/farm/" + f.String() + "/agent/" + a.String() + "/host/" + h.String()}
}
func hasFarmURI(c *x509.Certificate, f identity.FarmID) bool {
	for _, u := range c.URIs {
		if u.String() == farmURI(f).String() {
			return true
		}
	}
	return false
}
func hasAgentURI(uris []*url.URL, f identity.FarmID, a identity.AgentID, h identity.HostID) bool {
	want := agentURI(f, a, h).String()
	for _, u := range uris {
		if u.String() == want {
			return true
		}
	}
	return false
}
func verifyController(c, ca *x509.Certificate, cid identity.ControllerID, fid identity.FarmID, now time.Time) error {
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	if _, err := c.Verify(x509.VerifyOptions{Roots: pool, DNSName: ServerName(cid), CurrentTime: now, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
		return conflict("Controller certificate verification failed", err)
	}
	want := controllerURI(fid, cid).String()
	for _, u := range c.URIs {
		if u.String() == want {
			return nil
		}
	}
	return conflict("Controller certificate identity does not match Controller/Farm", nil)
}
func serial() *big.Int {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	n, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return big.NewInt(time.Now().UnixNano())
	}
	return n
}
func parseCertPEM(data []byte) (*x509.Certificate, error) {
	b, _ := pem.Decode(data)
	if b == nil || b.Type != "CERTIFICATE" {
		return nil, errors.New("missing certificate PEM")
	}
	return x509.ParseCertificate(b.Bytes)
}
func parseKeyPEM(data []byte) (any, error) {
	b, _ := pem.Decode(data)
	if b == nil {
		return nil, errors.New("missing key PEM")
	}
	return x509.ParsePKCS8PrivateKey(b.Bytes)
}
func readMode(dir, name string, mode os.FileMode) ([]byte, error) {
	p := filepath.Join(dir, name)
	i, err := os.Stat(p)
	if err != nil {
		return nil, conflict("cannot read PKI file", err)
	}
	if !i.Mode().IsRegular() || i.Mode().Perm() != mode {
		return nil, conflict("invalid PKI file permissions", fmt.Errorf("%s must be %04o", name, mode))
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, conflict("cannot read PKI file", err)
	}
	return b, nil
}
func writeAtomic(dir, name string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(dir, "."+name+"-")
	if err != nil {
		return conflict("cannot create PKI file", err)
	}
	n := tmp.Name()
	defer os.Remove(n)
	if err = tmp.Chmod(mode); err == nil {
		_, err = tmp.Write(data)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if e := tmp.Close(); err == nil {
		err = e
	}
	if err != nil {
		return conflict("cannot persist PKI file", err)
	}
	if _, err = os.Stat(filepath.Join(dir, name)); err == nil {
		return conflict("PKI file already exists", nil)
	}
	if err = os.Rename(n, filepath.Join(dir, name)); err != nil {
		return conflict("cannot publish PKI file", err)
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
func conflict(msg string, err error) error {
	d := map[string]string{}
	if err != nil {
		d["reason"] = err.Error()
	}
	return farmerr.Error{Code: farmerr.CONFIG_CONFLICT, HumanMessage: msg, Details: d}
}
