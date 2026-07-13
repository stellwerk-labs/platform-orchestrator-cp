package vault

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	vaultapi "github.com/hashicorp/vault/api"
	"github.com/stellwerk-labs/golib/hlogger"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	vaultToken  = "test-token"
	secretsPath = "path/to/secrets"
)

func NewTestVaultClient(t *testing.T, baseURL string, httpClient *http.Client) VaultClientInterface {
	client, err := vaultapi.NewClient(&vaultapi.Config{
		Address:    baseURL,
		HttpClient: httpClient,
	})
	require.NoError(t, err)
	client.SetToken(vaultToken)

	logger, _ := hlogger.NewTestLogger()
	return &vaultClient{
		path:   "secret",
		client: client,
		logger: logger.Logger,
	}
}

func TestUpsertSecret(t *testing.T) {
	var (
		secretData = map[string]interface{}{
			"key": "value",
		}
	)

	var tests = []struct {
		Name string

		VaultURL        string
		VaultStatusCode int
		VaultResponse   string

		ExpectedError error
	}{
		// Get: Success path
		//
		{
			Name:            "should save secret to the vault",
			VaultStatusCode: http.StatusOK,
			VaultResponse:   `{"data": {"version": 1}}`,
			ExpectedError:   nil,
		},

		// Get: Errors handling
		//
		{
			Name:          "should return error on bad vault URL",
			VaultURL:      "wrong.domain/vault/path",
			VaultResponse: `{"data": {}}`,
			ExpectedError: errors.New("failed to write Vault secret at path"),
		},
		{
			Name:            "should return ErrUnexpectedSecretStoreError when an internal error is received by Vault",
			VaultStatusCode: http.StatusInternalServerError,
			ExpectedError:   ErrUnexpectedSecretStoreError,
		},
	}
	for _, tt := range tests {
		assert := assert.New(t)
		var sentDataBytes []byte
		t.Run(tt.Name, func(t *testing.T) {
			fakeServer := httptest.NewServer(
				http.HandlerFunc(
					func(w http.ResponseWriter, r *http.Request) {
						switch r.URL.Path {
						case "/v1/secret/data/path/to/secrets":
							if r.Method != http.MethodPost && r.Method != http.MethodPut {
								w.WriteHeader(http.StatusMethodNotAllowed)
								return
							}
							if r.Header.Get("X-Vault-Token") != vaultToken {
								w.WriteHeader(http.StatusUnauthorized)
								return
							}

							sentDataBytes, _ = io.ReadAll(r.Body)

							w.Header().Set("Content-Type", "application/json")
							w.WriteHeader(tt.VaultStatusCode)
							_, _ = w.Write([]byte(tt.VaultResponse))
							return
						}

						w.WriteHeader(http.StatusExpectationFailed)
					},
				),
			)
			defer fakeServer.Close()

			if tt.VaultURL == "" {
				tt.VaultURL = fakeServer.URL
			}

			vlt := NewTestVaultClient(t, tt.VaultURL, fakeServer.Client())

			_, err := vlt.UpsertSecret(t.Context(), secretsPath, secretData)

			if tt.ExpectedError == nil {
				// On Success

				assert.NoError(err)

				// Confirm saved data
				//
				var sentData struct {
					Data map[string]interface{} `json:"data"`
				}
				assert.NoError(json.Unmarshal(sentDataBytes, &sentData))
				assert.Equal(sentData.Data, secretData)

			} else {
				// On Error

				assert.ErrorContains(err, tt.ExpectedError.Error())
			}
		})
	}
}

func TestDeleteSecret(t *testing.T) {
	var tests = []struct {
		Name string

		VaultURL        string
		VaultStatusCode int
		VaultResponse   string

		ExpectedError error
	}{
		// Get: Success path
		//
		{
			Name:            "should purge the secret from the vault",
			VaultStatusCode: http.StatusNoContent,
		},
		{
			Name:            "should handle the missing secret (not found)",
			VaultStatusCode: http.StatusNotFound,
			ExpectedError:   ErrSecretNotFound,
		},

		// Get: Errors handling
		//
		{
			Name:          "should return error on bad vault URL",
			VaultURL:      "wrong.domain/vault/path",
			ExpectedError: errors.New("failed to delete Vault secret"),
		},
		{
			Name:            "should return ErrUnexpectedSecretStoreError when an internal error is received by Vault",
			VaultStatusCode: http.StatusInternalServerError,
			ExpectedError:   ErrUnexpectedSecretStoreError,
		},
	}

	for _, tt := range tests {
		t.Run(tt.Name, func(t *testing.T) {
			assert := assert.New(t)
			fakeServer := httptest.NewServer(
				http.HandlerFunc(
					func(w http.ResponseWriter, r *http.Request) {
						switch r.URL.Path {
						case "/v1/secret/metadata/path/to/secrets":
							if r.Method != http.MethodDelete {
								w.WriteHeader(http.StatusMethodNotAllowed)
								return
							}
							if r.Header.Get("X-Vault-Token") != vaultToken {
								w.WriteHeader(http.StatusUnauthorized)
								return
							}

							w.Header().Set("Content-Type", "application/json")
							w.WriteHeader(tt.VaultStatusCode)
							_, _ = w.Write([]byte(tt.VaultResponse))
							return
						}

						w.WriteHeader(http.StatusExpectationFailed)
					},
				),
			)
			defer fakeServer.Close()

			if tt.VaultURL == "" {
				tt.VaultURL = fakeServer.URL
			}

			vlt := NewTestVaultClient(t, tt.VaultURL, fakeServer.Client())

			err := vlt.DeleteSecret(t.Context(), secretsPath)

			if tt.ExpectedError == nil {
				// On Success

				assert.NoError(err)

			} else {
				// On Error

				assert.ErrorContains(err, tt.ExpectedError.Error())
			}
		})
	}
}
