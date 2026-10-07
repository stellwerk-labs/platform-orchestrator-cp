package plugins

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// LoadSigningKey reads the operator-owned plugin context signing key. The
// private key is never part of discovery, manifests, logs or the plugin
// process environment.
func LoadSigningKey(path string) (ed25519.PrivateKey, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("the plugin signing key requires an absolute path")
	}
	keyFile, err := os.OpenInRoot(filepath.Dir(path), filepath.Base(path))
	if err != nil {
		return nil, fmt.Errorf("read plugin signing key file: %w", err)
	}
	defer func() { _ = keyFile.Close() }()
	encoded, err := io.ReadAll(io.LimitReader(keyFile, 16385))
	if err != nil || len(encoded) > 16384 {
		return nil, fmt.Errorf("cannot read bounded plugin signing key")
	}
	block, rest := pem.Decode(encoded)
	if block == nil || block.Type != "PRIVATE KEY" || len(rest) != 0 {
		return nil, fmt.Errorf("plugin signing key must be a single PKCS8 PEM private key")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("invalid plugin signing key")
	}
	key, ok := parsed.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("plugin signing key must use Ed25519")
	}
	return key, nil
}
