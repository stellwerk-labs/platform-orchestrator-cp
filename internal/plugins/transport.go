package plugins

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"time"
)

// NewServiceHTTPClient adds an operator CA bundle without bypassing hostname
// verification or replacing the platform trust roots. Redirects never carry
// delegated assertions to another destination.
func NewServiceHTTPClient(caPEM []byte) (*http.Client, error) {
	if len(caPEM) > 1<<20 {
		return nil, fmt.Errorf("plugin CA bundle exceeds 1 MiB")
	}
	roots, err := x509.SystemCertPool()
	if err != nil {
		return nil, fmt.Errorf("load platform trust roots: %w", err)
	}
	if len(caPEM) > 0 && !roots.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("plugin CA bundle contains no valid certificate")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}
	return &http.Client{Transport: transport, Timeout: 30 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}, nil
}
