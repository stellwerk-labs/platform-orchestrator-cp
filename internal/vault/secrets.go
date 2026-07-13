package vault

import (
	"context"

	"net/http"

	"github.com/hashicorp/vault/api"
	"github.com/pkg/errors"
	"github.com/stellwerk-labs/golib/hlogger"
	"go.uber.org/zap"
)

// UpsertSecret creates or updates the path in a KV store in vault
func (vlt *vaultClient) UpsertSecret(ctx context.Context, path string, data map[string]interface{}) (int, error) {
	logger := hlogger.TraceScopedLoggerFromCtx(vlt.logger, ctx).Sugar()

	if s, err := vlt.client.KVv2(vlt.path).Put(ctx, path, data); err != nil {
		var vErr *api.ResponseError
		if errors.As(err, &vErr) && vErr.StatusCode == http.StatusInternalServerError {
			logger.Errorw("can't write secret to Vault secret engine", "secretKey", path, "err", err)
			return 0, ErrUnexpectedSecretStoreError
		}
		return 0, errors.Wrapf(err, "failed to write Vault secret at path `%s`", path)
	} else {
		if s.VersionMetadata == nil {
			panic("unsupported Vault version used, v2 is required")
		}
		return s.VersionMetadata.Version, nil
	}
}

// DeleteSecret deletes all values at the path in a KV store in Vault
func (vlt *vaultClient) DeleteSecret(ctx context.Context, path string) error {
	logger := hlogger.TraceScopedLoggerFromCtx(vlt.logger, ctx).Sugar()

	if err := vlt.client.KVv2(vlt.path).DeleteMetadata(ctx, path); err != nil {
		var vErr *api.ResponseError
		if errors.As(err, &vErr) {
			if vErr.StatusCode == http.StatusNotFound {
				return ErrSecretNotFound
			}
			if vErr.StatusCode == http.StatusInternalServerError {
				logger.Errorw("can't delete secret from Vault secret engine", zap.String("secretKey", path), zap.Error(err))
				return ErrUnexpectedSecretStoreError
			}
		}
		return errors.Wrapf(err, "failed to delete Vault secret `%s`", path)
	}
	return nil
}
