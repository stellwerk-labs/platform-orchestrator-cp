//go:build integration

package model

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/moduleversions"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/opt"
	"github.com/stretchr/testify/require"
)

func retirementProvider(t *testing.T, db Databaser, org string) *ModuleProvider {
	t.Helper()
	provider, err := db.CreateModuleProvider(t.Context(), nil, &ModuleProvider{
		OrgId: org, ProviderType: "random", Id: "retiring", Source: "hashicorp/random", VersionConstraint: "= 3.7.2", Config: map[string]any{}, CreatedAt: time.Now(),
	})
	require.NoError(t, err)
	return provider
}

func requestProviderRetirement(t *testing.T, db Databaser, p *ModuleProvider) {
	t.Helper()
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	require.NoError(t, db.DeleteModuleProvider(t.Context(), tx, p.OrgId, p.ProviderType, p.Id))
	require.NoError(t, tx.Commit())
}

func TestProviderRetirementPreservesMaterialAndIdentity(t *testing.T) {
	db, _, request := admissionFixture(t)
	p := retirementProvider(t, db, request.OrgID)
	requestProviderRetirement(t, db, p)
	first, err := db.GetModuleProvider(t.Context(), nil, p.OrgId, p.ProviderType, p.Id)
	require.NoError(t, err)
	require.True(t, first.DeletionRequestedAt.IsSet())
	require.Equal(t, p.Config, first.Config)
	require.Equal(t, p.Source, first.Source)
	requestProviderRetirement(t, db, p)
	second, err := db.GetModuleProvider(t.Context(), nil, p.OrgId, p.ProviderType, p.Id)
	require.NoError(t, err)
	require.Equal(t, first.DeletionRequestedAt, second.DeletionRequestedAt)
	_, err = db.CreateModuleProvider(t.Context(), nil, p)
	require.ErrorContains(t, err, "already exists")
	rows, err := ListPendingProviderDeletions(t.Context(), db, 1)
	require.NoError(t, err)
	require.Equal(t, []PendingProviderDeletion{{OrgID: p.OrgId, ProviderType: p.ProviderType, ID: p.Id}}, rows)
}

func TestProviderRetirementEpochClosesAdmissionAndCompletionRaces(t *testing.T) {
	db, _, request := admissionFixture(t)
	p := retirementProvider(t, db, request.OrgID)
	requestProviderRetirement(t, db, p)
	candidate := PendingProviderDeletion{OrgID: p.OrgId, ProviderType: p.ProviderType, ID: p.Id}
	epoch, err := CaptureProviderDeletionEpoch(t.Context(), db, p.OrgId)
	require.NoError(t, err)

	receipt, err := AuthorizeDeploymentAdmission(t.Context(), db, request)
	require.NoError(t, err)
	deleted, err := FinalizeProviderDeletion(t.Context(), db, candidate, epoch)
	require.NoError(t, err)
	require.False(t, deleted, "admission started during the scan")
	epoch, err = CaptureProviderDeletionEpoch(t.Context(), db, p.OrgId)
	require.NoError(t, err)
	deleted, err = FinalizeProviderDeletion(t.Context(), db, candidate, epoch)
	require.NoError(t, err)
	require.False(t, deleted, "unresolved admission must retain Provider")
	_, err = SettleDeploymentAdmission(t.Context(), db, request.OrgID, request.AdmissionID, request.ProjectUUID, request.EnvironmentUUID, request.RequestDigest, "succeeded")
	require.NoError(t, err)
	deleted, err = FinalizeProviderDeletion(t.Context(), db, candidate, epoch)
	require.NoError(t, err)
	require.False(t, deleted, "a completion during the scan invalidates it")
	require.Positive(t, receipt.Epoch)
	epoch, err = CaptureProviderDeletionEpoch(t.Context(), db, p.OrgId)
	require.NoError(t, err)
	deleted, err = FinalizeProviderDeletion(t.Context(), db, candidate, epoch)
	require.NoError(t, err)
	require.True(t, deleted)
	_, err = db.GetModuleProvider(t.Context(), nil, p.OrgId, p.ProviderType, p.Id)
	_, absent := IsErrNotFound(err)
	require.True(t, absent)
}

func TestProviderRetirementRejectsFreshDesiredUse(t *testing.T) {
	db, _, request := admissionFixture(t)
	p := retirementProvider(t, db, request.OrgID)
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	module, err := db.CreateEmptyModule(t.Context(), tx, request.OrgID, "retirement-module", "Retirement fixture", "", "generic", map[string]string{})
	require.NoError(t, err)
	version, err := db.PublishCoreModuleVersion(t.Context(), tx, &ModuleDefinitionVersion{
		OrgId: request.OrgID, DefinitionId: module.Slug, SemanticVersion: "1.0.0",
		ModuleSource: "inline", ModuleSourceCode: opt.Of(`output "name" { value = "fixture" }`),
		OutputSchema: map[string]any{"type": "object", "properties": map[string]any{"name": map[string]any{"type": "string"}}},
		ModuleInputs: map[string]any{}, ModuleParams: map[string]ModuleParam{}, ProviderMapping: map[string]string{"random": "random.retiring"},
		Dependencies: map[string]ModuleDefinitionDependency{}, CoProvisioned: []ModuleDefinitionCoProvision{},
	}, request.ActorID)
	require.NoError(t, err)
	version, err = db.TransitionCoreModuleVersion(t.Context(), tx, request.OrgID, module.Slug, version.UUID.String(), moduleversions.LifecycleDefault, version.ResourceVersion, request.ActorID, "Prepare fixture", nil)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())

	request.AdmissionID, request.RequestDigest = uuid.New(), strings.Repeat("c", 64)
	request.ModuleVersions = map[string]uuid.UUID{module.UUID.String(): version.UUID}
	request.CarryForwardModuleVersions = nil
	requestProviderRetirement(t, db, p)
	_, err = AuthorizeDeploymentAdmission(t.Context(), db, request)
	conflict, ok := IsErrConflict(err)
	require.True(t, ok, "%v", err)
	require.Equal(t, "provider_deletion_pending", conflict.Code)

	tx, err = db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	userErr, systemErr := validateProviders(t.Context(), tx, request.OrgID, map[string]string{"random": "random.retiring"})
	require.NoError(t, systemErr)
	require.ErrorContains(t, userErr, "no active provider")
	require.NoError(t, tx.Rollback())
}

func TestProviderRetirementSerializesWithAdmission(t *testing.T) {
	db, _, request := admissionFixture(t)
	p := retirementProvider(t, db, request.OrgID)
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	require.NoError(t, db.DeleteModuleProvider(t.Context(), tx, p.OrgId, p.ProviderType, p.Id))
	blocked, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	_, err = AuthorizeDeploymentAdmission(blocked, db, request)
	require.Error(t, err)
	require.Error(t, blocked.Err())
	require.NoError(t, tx.Commit())
	_, err = AuthorizeDeploymentAdmission(t.Context(), db, request)
	require.NoError(t, err, "unrelated Provider retirement does not block other work")
}

func TestProviderRetirementChecksRotatePastBlockedCandidates(t *testing.T) {
	db, _, request := admissionFixture(t)
	first := retirementProvider(t, db, request.OrgID)
	second := *first
	second.Id = "another"
	_, err := db.CreateModuleProvider(t.Context(), nil, &second)
	require.NoError(t, err)
	requestProviderRetirement(t, db, first)
	requestProviderRetirement(t, db, &second)
	initial, err := ListPendingProviderDeletions(t.Context(), db, 1)
	require.NoError(t, err)
	require.Len(t, initial, 1)
	require.NoError(t, MarkProviderDeletionCheck(t.Context(), db, request.OrgID, initial))
	next, err := ListPendingProviderDeletions(t.Context(), db, 1)
	require.NoError(t, err)
	require.Len(t, next, 1)
	require.NotEqual(t, initial[0].ID, next[0].ID, "an attempted blocked row must not starve an unchecked row")
}

func TestProviderRetirementTracksRequiredEnvironmentState(t *testing.T) {
	for _, outcome := range []string{"absent", "succeeded", "failed"} {
		t.Run(outcome, func(t *testing.T) {
			db, _, request := admissionFixture(t)
			_, err := AuthorizeDeploymentAdmission(t.Context(), db, request)
			require.NoError(t, err)
			required, err := RequiredProviderStateEnvironments(t.Context(), db, request.OrgID)
			require.NoError(t, err)
			require.Empty(t, required, "unresolved guards block purge without inventing state")
			_, err = SettleDeploymentAdmission(t.Context(), db, request.OrgID, request.AdmissionID, request.ProjectUUID, request.EnvironmentUUID, request.RequestDigest, outcome)
			require.NoError(t, err)
			required, err = RequiredProviderStateEnvironments(t.Context(), db, request.OrgID)
			require.NoError(t, err)
			if outcome == "absent" {
				require.Empty(t, required, "authoritative absence must not strand retirement")
			} else {
				require.Equal(t, []uuid.UUID{request.EnvironmentUUID}, required)
			}
		})
	}
}

func TestProviderRetirementExternalNoEffectRequiresNoState(t *testing.T) {
	db := externalOperationDatabase(t)
	command := externalGrantFixture(t, db)
	grant, err := ApplyExternalGrantCommand(t.Context(), db, command)
	require.NoError(t, err)
	control := command
	control.Intent = nil
	control.SourceOperation = "controlWorkflow"
	control.Action, control.RequestID, control.ExpectedRevision = "activate", "start", grant.Revision
	grant, err = ApplyExternalGrantCommand(t.Context(), db, control)
	require.NoError(t, err)
	env := grant.Intent.Environments[0]
	dispatch := ExternalDispatch{OrgID: grant.OrgID, PluginID: grant.PluginID, OperationID: grant.OperationID, ActorID: grant.ActorID, Fence: grant.Revision, Action: "external-deployment.admit", DispatchID: "never-created", ProjectUUID: env.ProjectID, EnvironmentUUID: env.EnvironmentID, RequestDigest: grant.Intent.Fingerprint, TargetVersions: grant.Intent.TargetVersions}
	require.NoError(t, RecordExternalDispatch(t.Context(), db, dispatch))
	require.NoError(t, WithdrawExternalDispatch(t.Context(), db, grant.OrgID, grant.PluginID, grant.OperationID, dispatch.DispatchID))
	require.NoError(t, CompleteExternalDispatch(t.Context(), db, grant.OrgID, grant.PluginID, grant.OperationID, dispatch.DispatchID, ExternalDispatchCompletion{EnvironmentUUID: env.EnvironmentID, ProjectUUID: env.ProjectID, RequestDigest: dispatch.RequestDigest, NoEffect: true}))
	required, err := RequiredProviderStateEnvironments(t.Context(), db, grant.OrgID)
	require.NoError(t, err)
	require.Empty(t, required, "an unexecuted plugin operation must not invent state")
}
