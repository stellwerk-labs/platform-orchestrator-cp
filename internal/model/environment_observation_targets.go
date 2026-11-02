package model

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"

	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/stellwerk-labs/platform-orchestrator-cp/shared/v3/plugincontext"
)

type ObservationTargetSelector struct {
	ProjectIDs            *[]string    `json:"project_ids,omitempty"`
	EnvironmentTypes      *[]string    `json:"environment_types,omitempty"`
	Production            *bool        `json:"production,omitempty"`
	EnvironmentIDs        *[]uuid.UUID `json:"environment_ids,omitempty"`
	ExcludeEnvironmentIDs []uuid.UUID  `json:"exclude_environment_ids,omitempty"`
}
type ObservationTargetMetadata struct {
	ProjectUUID     uuid.UUID `json:"project_uuid"`
	EnvironmentUUID uuid.UUID `json:"environment_uuid"`
	ProjectID       string    `json:"project_id"`
	EnvironmentID   string    `json:"environment_id"`
	EnvironmentType string    `json:"environment_type"`
	Production      bool      `json:"production"`
}
type observationTargetCursor struct {
	Org            string    `json:"org"`
	Plugin         string    `json:"plugin"`
	SelectorDigest string    `json:"selector_digest"`
	After          uuid.UUID `json:"after"`
}

// QueryObservationTargetsTx pushes filtering and keyset pagination into the
// authoritative store. Nil includes all matching resources; explicit empty
// include lists remain empty and never widen into an organization-wide scan.
func QueryObservationTargetsTx(ctx context.Context, tx Tx, org, plugin string, selector ObservationTargetSelector, page string, limit int) ([]ObservationTargetMetadata, string, error) {
	if org == "" || !externalPluginIdentity.MatchString(plugin) || limit < 1 || limit > 200 || len(page) > 2048 {
		return nil, "", NewErrBadRequest("bounded target query and page required")
	}
	projectIDs, projectFilter := observationStringFilter(selector.ProjectIDs)
	envTypes, typeFilter := observationStringFilter(selector.EnvironmentTypes)
	if len(projectIDs) > 1000 || len(envTypes) > 1000 || len(selector.ExcludeEnvironmentIDs) > 10000 {
		return nil, "", NewErrBadRequest("target selector exceeds resource bounds")
	}
	for _, values := range [][]string{projectIDs, envTypes} {
		seen := map[string]bool{}
		for _, value := range values {
			if strings.TrimSpace(value) == "" || len(value) > 63 || seen[value] {
				return nil, "", NewErrBadRequest("target filters require unique bounded canonical identifiers")
			}
			seen[value] = true
		}
	}
	include := []string{}
	if selector.EnvironmentIDs != nil {
		if len(*selector.EnvironmentIDs) > 10000 {
			return nil, "", NewErrBadRequest("too many selected Environments")
		}
		for _, id := range *selector.EnvironmentIDs {
			if id == uuid.Nil {
				return nil, "", NewErrBadRequest("Environment identities cannot be empty")
			}
			include = append(include, id.String())
		}
	}
	exclude := []string{}
	for _, id := range selector.ExcludeEnvironmentIDs {
		if id == uuid.Nil {
			return nil, "", NewErrBadRequest("Environment identities cannot be empty")
		}
		exclude = append(exclude, id.String())
	}
	encoded, err := json.Marshal(selector)
	if err != nil {
		return nil, "", err
	}
	digest := plugincontext.Digest(encoded)
	cursor := observationTargetCursor{Org: org, Plugin: plugin, SelectorDigest: digest}
	if page != "" {
		raw, err := base64.RawURLEncoding.DecodeString(page)
		if err != nil || json.Unmarshal(raw, &cursor) != nil || cursor.Org != org || cursor.Plugin != plugin || cursor.SelectorDigest != digest || cursor.After == uuid.Nil {
			return nil, "", NewErrBadRequest("page cursor must match this exact target query")
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT e.project_uuid,e.uuid,e.project_id,e.id,e.env_type_id,t.is_production
 FROM envs e JOIN env_types t ON t.org_id=e.org_id AND t.uuid=e.env_type_uuid
 JOIN projects p ON p.org_id=e.org_id AND p.uuid=e.project_uuid
 WHERE e.org_id=$1 AND e.status='active' AND p.status='active'
 AND (NOT $2 OR e.project_id=ANY($3::text[]))
 AND (NOT $4 OR e.env_type_id=ANY($5::text[]))
 AND ($6::boolean IS NULL OR t.is_production=$6)
 AND (NOT $7 OR e.uuid=ANY($8::uuid[]))
 AND NOT (e.uuid=ANY($9::uuid[]))
 AND ($10::uuid IS NULL OR e.uuid>$10)
 ORDER BY e.uuid LIMIT $11`, org, projectFilter, pq.Array(projectIDs), typeFilter, pq.Array(envTypes), selector.Production, selector.EnvironmentIDs != nil, pq.Array(include), pq.Array(exclude), nullableObservationUUID(cursor.After), limit+1)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = rows.Close() }()
	items := []ObservationTargetMetadata{}
	for rows.Next() {
		var target ObservationTargetMetadata
		if err := rows.Scan(&target.ProjectUUID, &target.EnvironmentUUID, &target.ProjectID, &target.EnvironmentID, &target.EnvironmentType, &target.Production); err != nil {
			return nil, "", err
		}
		items = append(items, target)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	next := ""
	if len(items) > limit {
		items = items[:limit]
		cursor.After = items[len(items)-1].EnvironmentUUID
		raw, err := json.Marshal(cursor)
		if err != nil {
			return nil, "", err
		}
		next = base64.RawURLEncoding.EncodeToString(raw)
	}
	return items, next, nil
}
func observationStringFilter(values *[]string) ([]string, bool) {
	if values == nil {
		return []string{}, false
	}
	return *values, true
}
