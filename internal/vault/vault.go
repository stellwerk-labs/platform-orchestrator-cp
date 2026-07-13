//go:generate go tool mockgen -destination=mocks/vaulter.go -package mock_vault github.com/stellwerk-labs/platform-orchestrator-cp/internal/vault VaultClientInterface

package vault

import (
	"context"
	"errors"

	vaultapi "github.com/hashicorp/vault/api"
	"go.uber.org/zap"
)

const SecretPrefix = "secret"

var ErrSecretNotFound = errors.New("not found")
var ErrSecretMarkedForDeletion = errors.New("secret scheduled for deletion")
var ErrBadParameter = errors.New("bad request parameter")
var ErrUnexpectedSecretStoreError = errors.New("unexpected error in the secret store")

type VaultClientInterface interface {
	DeleteSecret(ctx context.Context, path string) error
	UpsertSecret(ctx context.Context, path string, data map[string]interface{}) (int, error)
}

type vaultClient struct {
	path   string
	client *vaultapi.Client
	logger *zap.Logger
}

// NewVaultClient creates a new instance of a Client for internal Vault
func NewVaultClient(vaultApiClient *vaultapi.Client, logger *zap.Logger) VaultClientInterface {
	return &vaultClient{
		client: vaultApiClient,
		path:   SecretPrefix,
		logger: logger,
	}
}
