package plugins

import (
	"context"
	"fmt"
	"github.com/google/uuid"
	"github.com/stellwerk-labs/platform-orchestrator-iam/shared/adminapi"
)

// IAMOrganizationAdminAuthorizer uses the optional canonical IAM predicate.
// Older IAMs and incomplete responses refuse authorization without a role-name
// or assignable-permission fallback.
type IAMOrganizationAdminAuthorizer struct{ Client *adminapi.ClientWithResponses }

func (a *IAMOrganizationAdminAuthorizer) IsOrganizationAdmin(ctx context.Context, org, actor string) (bool, error) {
	id, err := uuid.Parse(actor)
	if err != nil {
		return false, err
	}
	if a == nil || a.Client == nil {
		return false, fmt.Errorf("canonical Admin API unavailable")
	}
	response, err := a.Client.InternalIsOrganizationAdminWithResponse(ctx, org, id)
	if err != nil {
		return false, err
	}
	if response == nil || response.StatusCode() != 200 || response.JSON200 == nil {
		return false, fmt.Errorf("canonical Admin API unavailable")
	}
	return response.JSON200.Allowed, nil
}
