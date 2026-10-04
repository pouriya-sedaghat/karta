package online

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"fmt"
)

// maxKeyFile bounds a private key file.
const maxKeyFile = 16 << 10

// LoadSigningKey reads a PKCS#8 PEM Ed25519 private key (karta-sign keygen,
// or `openssl genpkey -algorithm ed25519`), never through a symlink. Signing
// keys belong to producers (cmd/karta-sign, the bridge's signer), never to a
// Karta deployment.
func LoadSigningKey(path string) (ed25519.PrivateKey, error) {
	b, err := readRegular(path, maxKeyFile)
	if err != nil {
		return nil, err
	}
	blk, _ := pem.Decode(b)
	if blk == nil || blk.Type != "PRIVATE KEY" {
		return nil, fmt.Errorf("%s is not a PKCS#8 PEM private key", path)
	}
	k, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
	if err != nil {
		return nil, err
	}
	ek, ok := k.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("%s is not an Ed25519 key", path)
	}
	return ek, nil
}
