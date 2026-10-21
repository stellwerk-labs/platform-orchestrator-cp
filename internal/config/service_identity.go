package config

import (
	"errors"

	"github.com/google/uuid"
	"github.com/stellwerk-labs/platform-orchestrator-iam/shared/userid"
)

// ServiceIdentity is the UUID of an IAM service identity, such as a platform
// principal. The zero value means none is configured.
type ServiceIdentity uuid.UUID

// EnvDecode accepts only the canonical form of a service identity's UUID, so a
// file path, a JSON document or a human user's UUID fails at start-up instead
// of at the first call.
func (id *ServiceIdentity) EnvDecode(value string) error {
	parsed, err := uuid.Parse(value)
	if err != nil || parsed.String() != value || !userid.IsServiceUser(parsed) {
		return errors.New("must be the canonical UUID of an IAM service identity")
	}
	*id = ServiceIdentity(parsed)
	return nil
}
