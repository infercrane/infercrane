package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/config"
)

func TestMutualTLSRequiresQualifiedClientIdentity(t *testing.T) {
	directory := t.TempDir()
	caCert, caKey, caPEM := testCertificateAuthority(t)
	serverCert, serverKey := testSignedCertificate(t, caCert, caKey, true)
	clientCert, clientKey := testSignedCertificate(t, caCert, caKey, false)
	caPath := writeTLSFixture(t, directory, "ca.pem", caPEM)
	serverCertPath := writeTLSFixture(t, directory, "server.pem", serverCert)
	serverKeyPath := writeTLSFixture(t, directory, "server-key.pem", serverKey)
	clientCertPath := writeTLSFixture(t, directory, "client.pem", clientCert)
	clientKeyPath := writeTLSFixture(t, directory, "client-key.pem", clientKey)

	serverConfig, err := serverTLSConfig(config.Config{TLSCertFile: serverCertPath, TLSKeyFile: serverKeyPath, TLSClientCAFile: caPath})
	if err != nil {
		t.Fatal(err)
	}
	serverIdentity, err := tls.LoadX509KeyPair(serverCertPath, serverKeyPath)
	if err != nil {
		t.Fatal(err)
	}
	serverConfig.Certificates = []tls.Certificate{serverIdentity}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	server.TLS = serverConfig
	server.StartTLS()
	defer server.Close()

	withoutIdentity, err := controlHTTPClient(config.Config{ClientTLSCAFile: caPath}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	request, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL, nil)
	if _, err = withoutIdentity.Do(request); err == nil {
		t.Fatal("mTLS server accepted a client without an identity")
	}

	withIdentity, err := controlHTTPClient(config.Config{ClientTLSCAFile: caPath, ClientTLSCertFile: clientCertPath, ClientTLSKeyFile: clientKeyPath}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	response, err := withIdentity.Get(server.URL)
	if err != nil || response.StatusCode != http.StatusNoContent {
		t.Fatalf("qualified client response=%v err=%v", response, err)
	}
	response.Body.Close()
}

func testCertificateAuthority(t *testing.T) (*x509.Certificate, *ecdsa.PrivateKey, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "InferCrane test CA"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return certificate, key, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func testSignedCertificate(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey, server bool) ([]byte, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	usage := []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	dnsNames := []string(nil)
	if server {
		usage, dnsNames = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, []string{"127.0.0.1", "localhost"}
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: "infercrane-test"}, DNSNames: dnsNames, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: usage}
	if server {
		template.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
}

func writeTLSFixture(t *testing.T, directory, name string, contents []byte) string {
	t.Helper()
	path := filepath.Join(directory, name)
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
