package plugins

import (
	"bytes"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestServiceTransportVerifiesOperatorCA(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	defer server.Close()
	client, err := NewServiceHTTPClient(nil)
	require.NoError(t, err)
	untrusted, err := client.Get(server.URL)
	if untrusted != nil {
		require.NoError(t, untrusted.Body.Close())
	}
	require.Error(t, err, "an untrusted private issuer must fail closed")
	client, err = NewServiceHTTPClient(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}))
	require.NoError(t, err)
	response, err := client.Get(server.URL)
	require.NoError(t, err)
	defer func() { require.NoError(t, response.Body.Close()) }()
	require.Equal(t, http.StatusNoContent, response.StatusCode)
	_, err = NewServiceHTTPClient([]byte("not-a-certificate"))
	require.Error(t, err)
	_, err = NewServiceHTTPClient(bytes.Repeat([]byte{'x'}, (1<<20)+1))
	require.Error(t, err)
}
