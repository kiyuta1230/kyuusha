package mtls

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc/credentials"
)

func genCA(t *testing.T) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create CA cert: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse CA cert: %v", err)
	}
	return cert, key
}

func genLeaf(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey, cn string) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate leaf key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create leaf cert: %v", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal leaf key: %v", err)
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM
}

func writeFile(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// handshakePair runs one TLS handshake over a real loopback TCP connection
// (not net.Pipe: crypto/tls's record buffering doesn't play well with
// net.Pipe's strict synchronous rendezvous for a mutual/client-cert
// handshake, so a real socket is both simpler and closer to production
// usage anyway) and returns each side's handshake error.
func handshakePair(t *testing.T, serverCreds, clientCreds credentials.TransportCredentials) (serverErr, clientErr error) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	serverErrCh := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			serverErrCh <- err
			return
		}
		defer conn.Close()
		_, _, err = serverCreds.ServerHandshake(conn)
		serverErrCh <- err
	}()

	rawConn, err := net.DialTimeout("tcp", ln.Addr().String(), 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer rawConn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, clientErr = clientCreds.ClientHandshake(ctx, "localhost", rawConn)

	select {
	case serverErr = <-serverErrCh:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the server side of the handshake")
	}
	return serverErr, clientErr
}

// TestServerAndClientCredentials_Handshake exercises the real happy path:
// a client presenting a cert signed by the CA the server trusts, dialing a
// server presenting a cert signed by the same CA the client trusts (the
// "one shared leaf cert for everyone" scheme mtls.go's doc comment
// describes) -- the mutual handshake must complete on both sides.
func TestServerAndClientCredentials_Handshake(t *testing.T) {
	dir := t.TempDir()
	ca, caKey := genCA(t)
	caFile := writeFile(t, dir, "ca.crt", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.Raw}))

	leafCertPEM, leafKeyPEM := genLeaf(t, ca, caKey, "kyuusha-internal")
	certFile := writeFile(t, dir, "server.crt", leafCertPEM)
	keyFile := writeFile(t, dir, "server.key", leafKeyPEM)

	serverCreds, err := ServerCredentials(certFile, keyFile, caFile)
	if err != nil {
		t.Fatalf("ServerCredentials: %v", err)
	}
	clientCreds, err := ClientCredentials(certFile, keyFile, caFile)
	if err != nil {
		t.Fatalf("ClientCredentials: %v", err)
	}

	serverErr, clientErr := handshakePair(t, serverCreds, clientCreds)
	if clientErr != nil {
		t.Fatalf("ClientHandshake: %v", clientErr)
	}
	if serverErr != nil {
		t.Fatalf("ServerHandshake: %v", serverErr)
	}
}

// TestServerCredentials_RejectsClientCertFromUntrustedCA confirms the
// mutual half of mTLS actually enforces something: a caller presenting a
// structurally valid certificate -- just signed by a different CA than the
// one the server's ClientCAs pool trusts -- must be rejected, even though
// that same caller correctly trusts the server's own certificate (so this
// isolates "did the server's client-cert check reject them" from "did the
// client fail to verify the server").
func TestServerCredentials_RejectsClientCertFromUntrustedCA(t *testing.T) {
	dir := t.TempDir()
	ca, caKey := genCA(t)
	caFile := writeFile(t, dir, "ca.crt", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.Raw}))
	serverCertPEM, serverKeyPEM := genLeaf(t, ca, caKey, "kyuusha-internal")
	serverCertFile := writeFile(t, dir, "server.crt", serverCertPEM)
	serverKeyFile := writeFile(t, dir, "server.key", serverKeyPEM)

	serverCreds, err := ServerCredentials(serverCertFile, serverKeyFile, caFile)
	if err != nil {
		t.Fatalf("ServerCredentials: %v", err)
	}

	// The attacker's cert is signed by a different CA than the one
	// serverCreds trusts, but the attacker itself trusts the real CA (so
	// it will correctly verify the server's certificate) -- the failure
	// under test must come from the server's own client-cert check, not
	// from the attacker failing to verify the server.
	otherCA, otherCAKey := genCA(t)
	attackerCertPEM, attackerKeyPEM := genLeaf(t, otherCA, otherCAKey, "attacker")
	attackerCertFile := writeFile(t, dir, "attacker.crt", attackerCertPEM)
	attackerKeyFile := writeFile(t, dir, "attacker.key", attackerKeyPEM)

	attackerCreds, err := ClientCredentials(attackerCertFile, attackerKeyFile, caFile)
	if err != nil {
		t.Fatalf("ClientCredentials: %v", err)
	}

	serverErr, _ := handshakePair(t, serverCreds, attackerCreds)
	if serverErr == nil {
		t.Fatal("ServerHandshake succeeded with a client certificate signed by an untrusted CA")
	}
}
