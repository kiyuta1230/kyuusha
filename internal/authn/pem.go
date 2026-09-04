package authn

import (
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
)

// LoadECDSAPublicKeyPEM reads a PEM-encoded SubjectPublicKeyInfo (as
// produced by `openssl ec -in key.pem -pubout`) from path.
func LoadECDSAPublicKeyPEM(path string) (*ecdsa.PublicKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("%s: no PEM block found", path)
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%s: parse public key: %w", path, err)
	}
	ecdsaPub, ok := pub.(*ecdsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("%s: not an ECDSA public key", path)
	}
	return ecdsaPub, nil
}

// LoadECDSAPrivateKeyPEM reads a PEM-encoded EC private key (as produced by
// `openssl ecparam -genkey`), for dev/test token minting only. Production
// token issuance is Dex/Hydra's job (see docs/architecture.md); nothing in
// kyuusha's own services holds a signing key.
func LoadECDSAPrivateKeyPEM(path string) (*ecdsa.PrivateKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("%s: no PEM block found", path)
	}
	key, err := x509.ParseECPrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%s: parse private key: %w", path, err)
	}
	return key, nil
}
