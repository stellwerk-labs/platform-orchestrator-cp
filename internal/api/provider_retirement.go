package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	dp "github.com/stellwerk-labs/platform-orchestrator-dp/shared/v3/genclient"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
)

// ReconcileProviderDeletions uses one complete DP proof per organisation. A
// failed scan or changed admission epoch leaves the provider pending.
func (s *Server) ReconcileProviderDeletions(ctx context.Context) error {
	if !s.ProviderRetirementEnabled {
		return nil
	}
	if s.DpClient == nil {
		return fmt.Errorf("provider deletion reconciliation requires the Data Plane")
	}
	candidates, err := model.ListPendingProviderDeletions(ctx, s.Database, 64)
	if err != nil {
		return err
	}
	groups := make(map[string][]model.PendingProviderDeletion)
	var organisations []string
	for _, candidate := range candidates {
		if _, found := groups[candidate.OrgID]; !found {
			organisations = append(organisations, candidate.OrgID)
		}
		groups[candidate.OrgID] = append(groups[candidate.OrgID], candidate)
	}
	var failures []error
	for _, org := range organisations {
		if ctx.Err() != nil {
			failures = append(failures, ctx.Err())
			break
		}
		if err := model.MarkProviderDeletionCheck(ctx, s.Database, org, groups[org]); err != nil {
			failures = append(failures, err)
			continue
		}
		if err := s.reconcileOrgProviderDeletions(ctx, org, groups[org]); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func (s *Server) reconcileOrgProviderDeletions(ctx context.Context, org string, candidates []model.PendingProviderDeletion) error {
	epoch, err := model.CaptureProviderDeletionEpoch(ctx, s.Database, org)
	if err != nil {
		return err
	}
	environments, err := model.RequiredProviderStateEnvironments(ctx, s.Database, org)
	if err != nil {
		return err
	}
	scanCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	response, err := s.DpClient.InternalCheckProviderUsageWithResponse(scanCtx, org, dp.InternalCheckProviderUsageJSONRequestBody{RequiredEnvironmentUuids: environments})
	if err != nil {
		return fmt.Errorf("data plane provider usage proof is unavailable")
	}
	if response.StatusCode() != http.StatusOK || response.JSON200 == nil {
		// Older DPs and incomplete scans cannot authorise physical deletion.
		return nil
	}
	proof := response.JSON200
	if proof.ProtocolVersion != 1 || proof.OrgId != org || !proof.Complete || proof.Busy || proof.Providers == nil {
		return nil
	}
	for _, environment := range environments {
		if !slices.Contains(proof.EnvironmentUuids, environment) {
			return nil
		}
	}
	for _, candidate := range candidates {
		if slices.Contains(proof.Providers, candidate.ProviderType+"."+candidate.ID) {
			continue
		}
		if _, err := model.FinalizeProviderDeletion(ctx, s.Database, candidate, epoch); err != nil {
			return err
		}
	}
	return nil
}
