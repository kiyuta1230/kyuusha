// Package mtls implements the east-west (service-to-service) transport
// security from docs/architecture.md "認可の粒度": every kyuusha service
// mutually authenticates its callers via TLS client certificates, so a
// backend never accepts a plaintext connection from anywhere on the
// network. This is the mTLS half of the two orthogonal authorization axes
// docs/architecture.md describes -- it establishes *that* the caller is a
// legitimate kyuusha service, not *which* RPCs that service may call (the
// latter -- "compute can call network's CreateNetworkInterface but not
// DeleteSubnet" -- is a separate, still-undone follow-up).
//
// One CA signs one shared leaf certificate for every service in a
// deployment (see hack/devcerts/README.md); the leaf's SAN list covers
// every service hostname, so it works as both a server certificate (any
// service can present it to any caller) and a client certificate (any
// service can present it when dialing any other). This is deliberately not
// per-service identity: today's authorization boundary is "is this any
// kyuusha service," not "is this specifically compute" -- see the
// docs/architecture.md note above about deferring finer internal
// least-privilege.
package mtls

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"

	"google.golang.org/grpc/credentials"
)

// ServerCredentials builds mutual-TLS transport credentials for a gRPC
// server: it presents certFile/keyFile to callers and requires every
// caller to present a client certificate signed by caFile, refusing the
// connection otherwise.
func ServerCredentials(certFile, keyFile, caFile string) (credentials.TransportCredentials, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("load server cert/key: %w", err)
	}
	pool, err := loadCAPool(caFile)
	if err != nil {
		return nil, err
	}
	return credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    pool,
	}), nil
}

// ClientCredentials builds mutual-TLS transport credentials for a gRPC
// client: it presents certFile/keyFile to the server it dials (satisfying
// that server's own ServerCredentials requirement) and verifies the
// server's certificate against caFile.
func ClientCredentials(certFile, keyFile, caFile string) (credentials.TransportCredentials, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("load client cert/key: %w", err)
	}
	pool, err := loadCAPool(caFile)
	if err != nil {
		return nil, err
	}
	return credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
	}), nil
}

func loadCAPool(caFile string) (*x509.CertPool, error) {
	pemBytes, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("read CA file %s: %w", caFile, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemBytes) {
		return nil, fmt.Errorf("no certificates found in %s", caFile)
	}
	return pool, nil
}
