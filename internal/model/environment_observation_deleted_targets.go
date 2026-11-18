package model

import (
	"context"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// DeletedObservationTargetsTx checks exact UUIDs, independently of inventory
// filters. Existing deleting/delete_failed rows are not deletion evidence.
func DeletedObservationTargetsTx(ctx context.Context, tx Tx, org string, ids []uuid.UUID) ([]uuid.UUID, error) {
	if len(ids) > 10000 {
		return nil, NewErrBadRequest("too many selected Environments")
	}
	deleted := []uuid.UUID{}
	if len(ids) == 0 {
		return deleted, nil
	}
	values := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == uuid.Nil {
			return nil, NewErrBadRequest("Environment identities cannot be empty")
		}
		values = append(values, id.String())
	}
	rows, err := tx.QueryContext(ctx, `SELECT wanted.id FROM unnest($2::uuid[]) WITH ORDINALITY wanted(id,ordinal)
 LEFT JOIN envs e ON e.org_id=$1 AND e.uuid=wanted.id
 LEFT JOIN projects p ON p.org_id=$1 AND p.uuid=e.project_uuid
 WHERE e.uuid IS NULL OR p.uuid IS NULL ORDER BY wanted.ordinal`, org, pq.Array(values))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		deleted = append(deleted, id)
	}
	return deleted, rows.Err()
}

func normalizeObservationGrantTargetsTx(ctx context.Context, tx Tx, org string, previous, requested []ExternalOperationEnvironment, ids []string) ([]ExternalOperationEnvironment, []uuid.UUID, error) {
	rows, err := tx.QueryContext(ctx, `SELECT uuid,project_uuid FROM envs WHERE org_id=$1 AND uuid=ANY($2::uuid[]) FOR KEY SHARE`, org, pq.Array(ids))
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = rows.Close() }()
	current := map[uuid.UUID]uuid.UUID{}
	for rows.Next() {
		var id, project uuid.UUID
		if err := rows.Scan(&id, &project); err != nil {
			return nil, nil, err
		}
		current[id] = project
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	prior := map[uuid.UUID]uuid.UUID{}
	for _, target := range previous {
		prior[target.EnvironmentID] = target.ProjectID
	}
	kept := []ExternalOperationEnvironment{}
	var pruned []uuid.UUID
	for _, target := range requested {
		if project, ok := current[target.EnvironmentID]; ok {
			if project != target.ProjectID {
				return nil, nil, NewErrConflict("observation target project changed")
			}
			kept = append(kept, target)
		} else if prior[target.EnvironmentID] == target.ProjectID {
			pruned = append(pruned, target.EnvironmentID)
		} else {
			return nil, nil, ErrObservationTargetsUnavailable
		}
	}
	return kept, pruned, nil
}
