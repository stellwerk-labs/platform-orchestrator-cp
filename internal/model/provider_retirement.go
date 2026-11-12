package model

import (
	"context"
	"database/sql"
	"errors"

	"github.com/google/uuid"
	"github.com/lib/pq"
)

// The organisation epoch makes a remote DP usage scan conditional on all
// execution admissions that could change that state. No remote calls hold it.
func lockProviderLifecycle(ctx context.Context, tx Tx, org string) error {
	if tx == nil {
		return errors.New("provider lifecycle requires a transaction")
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO provider_lifecycle_epochs(org_id) VALUES($1) ON CONFLICT DO NOTHING`, org); err != nil {
		return err
	}
	var epoch int64
	return tx.QueryRowContext(ctx, `SELECT epoch FROM provider_lifecycle_epochs WHERE org_id=$1 FOR UPDATE`, org).Scan(&epoch)
}

func ReadProviderLifecycleEpoch(ctx context.Context, tx Tx, org string) (int64, error) {
	var epoch int64
	err := tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT epoch FROM provider_lifecycle_epochs WHERE org_id=$1),0)`, org).Scan(&epoch)
	return epoch, err
}

func advanceProviderLifecycleEpoch(ctx context.Context, tx Tx, org string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO provider_lifecycle_epochs(org_id,epoch) VALUES($1,1)
		ON CONFLICT(org_id) DO UPDATE SET epoch=provider_lifecycle_epochs.epoch+1`, org)
	return err
}

// Retiring providers cannot acquire new desired-state work. Historical cleanup
// providers are deliberately absent from these exact target Module versions.
func checkProviderRetirement(ctx context.Context, tx Tx, org string, versions []string) error {
	if len(versions) == 0 {
		return nil
	}
	var blocked bool
	err := tx.QueryRowContext(ctx, `WITH refs AS (
		SELECT mapping.value AS provider_id FROM definition_versions v
		CROSS JOIN LATERAL jsonb_each_text(COALESCE(NULLIF(v.provider_mapping,'null'::jsonb),'{}'::jsonb)) mapping
		WHERE v.org_id=$1 AND v.uuid=ANY($2::uuid[])
		UNION
		SELECT mapping.value FROM module_draft_test_snapshots s
		CROSS JOIN LATERAL jsonb_each_text(COALESCE(NULLIF(s.content->'provider_mapping','null'::jsonb),'{}'::jsonb)) mapping
		WHERE s.org_id=$1 AND s.id=ANY($2::uuid[])
	)
	SELECT EXISTS(SELECT 1 FROM refs
	    LEFT JOIN providers p ON p.org_id=$1 AND concat(p.provider_type,'.',p.id)=refs.provider_id
	    WHERE p.id IS NULL OR p.deletion_requested_at IS NOT NULL)`, org, pq.Array(versions)).Scan(&blocked)
	if err != nil {
		return err
	}
	if blocked {
		return ErrConflict{Code: "provider_deletion_pending", Message: "a target Module Version uses a missing Provider or one pending deletion; remove that binding before deploying"}
	}
	return nil
}

type PendingProviderDeletion struct {
	OrgID, ProviderType, ID string
}

// List a bounded fair batch. Only attempted rows are marked checked, so a
// timeout in one organisation does not continually hide unattempted work.
func ListPendingProviderDeletions(ctx context.Context, db Databaser, limit int) ([]PendingProviderDeletion, error) {
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, `SELECT org_id,provider_type,id FROM providers
	    WHERE deletion_requested_at IS NOT NULL
	    ORDER BY retirement_checked_at NULLS FIRST,deletion_requested_at,org_id,provider_type,id LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var result []PendingProviderDeletion
	for rows.Next() {
		var item PendingProviderDeletion
		if err := rows.Scan(&item.OrgID, &item.ProviderType, &item.ID); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func MarkProviderDeletionCheck(ctx context.Context, db Databaser, org string, candidates []PendingProviderDeletion) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	types, ids := make([]string, len(candidates)), make([]string, len(candidates))
	for i, candidate := range candidates {
		if candidate.OrgID != org {
			return NewErrBadRequest("Provider deletion check scope mismatch")
		}
		types[i], ids[i] = candidate.ProviderType, candidate.ID
	}
	if _, err := tx.ExecContext(ctx, `UPDATE providers p SET retirement_checked_at=clock_timestamp()
	    FROM unnest($2::text[],$3::text[]) candidate(provider_type,id)
	    WHERE p.org_id=$1 AND p.provider_type=candidate.provider_type AND p.id=candidate.id
	    AND p.deletion_requested_at IS NOT NULL`, org, pq.Array(types), pq.Array(ids)); err != nil {
		return err
	}
	return tx.Commit()
}

func CaptureProviderDeletionEpoch(ctx context.Context, db Databaser, org string) (int64, error) {
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	return ReadProviderLifecycleEpoch(ctx, tx, org)
}

// FinalizeProviderDeletion rechecks the remote scan's local fence and all
// unresolved admissions in the same transaction as physical removal.
func FinalizeProviderDeletion(ctx context.Context, db Databaser, provider PendingProviderDeletion, expectedEpoch int64) (bool, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := lockProviderLifecycle(ctx, tx, provider.OrgID); err != nil {
		return false, err
	}
	epoch, err := ReadProviderLifecycleEpoch(ctx, tx, provider.OrgID)
	if err != nil || epoch != expectedEpoch {
		return false, err
	}
	var busy bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM deployment_admission_guards WHERE org_id=$1 AND outcome IS NULL)
		OR EXISTS(SELECT 1 FROM external_operation_dispatches WHERE org_id=$1 AND state <> 'settled')`, provider.OrgID).Scan(&busy)
	if err != nil || busy {
		return false, err
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM providers WHERE org_id=$1 AND provider_type=$2 AND id=$3 AND deletion_requested_at IS NOT NULL`, provider.OrgID, provider.ProviderType, provider.ID)
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	if err != nil || count == 0 {
		return false, err
	}
	return true, tx.Commit()
}

// RequiredProviderStateEnvironments distinguishes new never-admitted
// Environments from migrated or previously admitted managed state.
func RequiredProviderStateEnvironments(ctx context.Context, db Databaser, org string) ([]uuid.UUID, error) {
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, `SELECT uuid FROM envs WHERE org_id=$1 AND provider_state_required ORDER BY uuid`, org)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	result := []uuid.UUID{}
	for rows.Next() {
		var environment uuid.UUID
		if err := rows.Scan(&environment); err != nil {
			return nil, err
		}
		result = append(result, environment)
	}
	return result, rows.Err()
}
