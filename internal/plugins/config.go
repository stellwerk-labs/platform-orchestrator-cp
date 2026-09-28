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

// LoadGateway reads operator-owned files. The private key is never part of
// discovery, manifests, logs or the plugin process environment.
func LoadGateway(registryPath, signingKeyPath, issuer string, development bool) (*Gateway, error) {
	file, err := openOperatorFile(registryPath)
	if err != nil {
		return nil, fmt.Errorf("open plugin installation configuration: %w", err)
	}
	defer func() { _ = file.Close() }()
	registry, err := LoadInstallations(file, development)
	if err != nil {
		return nil, err
	}
	keyFile, err := openOperatorFile(signingKeyPath)
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
	gateway := &Gateway{Registry: registry, SigningKey: key, Issuer: issuer}
	if err := gateway.Validate(); err != nil {
		return nil, err
	}
	return gateway, nil
}

func openOperatorFile(path string) (*os.File, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("plugin operator files require an absolute path")
	}
	return os.OpenInRoot(filepath.Dir(path), filepath.Base(path))
}
