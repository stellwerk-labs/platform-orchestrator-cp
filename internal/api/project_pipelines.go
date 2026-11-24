package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
	strictecho "github.com/oapi-codegen/runtime/strictmiddleware/echo"
	"github.com/stellwerk-labs/golib/hecho"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/api/middleware"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	api "github.com/stellwerk-labs/platform-orchestrator-cp/internal/pipelineapi"
	p "github.com/stellwerk-labs/platform-orchestrator-cp/shared/v3/pipelineapi"
)

const (
	PermissionPipelineRead          = "pipeline_read"
	PermissionPipelineWrite         = "pipeline_write"
	PermissionPipelinePublish       = "pipeline.publish"
	expectedPipelineResourceVersion = "expected_resource_version"
)

func pipelineJSON[T any](value any) T {
	var result T
	raw, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		panic(err)
	}
	return result
}

func (s *Server) registerPipelineRoutes(e *echo.Echo) {
	e.Pre(pipelineSourceBoundary)
	middlewares := []api.StrictMiddlewareFunc{
		func(next api.StrictHandlerFunc, id string) api.StrictHandlerFunc {
			return api.StrictHandlerFunc(hecho.OperationIdCollectorMiddleware(strictecho.StrictEchoHandlerFunc(next), id))
		},
		func(next api.StrictHandlerFunc, id string) api.StrictHandlerFunc {
			return api.StrictHandlerFunc(hecho.BuildContextTimeoutMiddlewareWithDuration(30*time.Second)(strictecho.StrictEchoHandlerFunc(next), id))
		},
		func(next api.StrictHandlerFunc, id string) api.StrictHandlerFunc {
			return api.StrictHandlerFunc(middleware.NewAuthZAsserter(regexp.MustCompile(`^$`))(strictecho.StrictEchoHandlerFunc(next), id))
		},
		func(next api.StrictHandlerFunc, id string) api.StrictHandlerFunc {
			return api.StrictHandlerFunc(hecho.AuthMiddleware(string(api.UserIdHeaderScopes))(strictecho.StrictEchoHandlerFunc(next), id))
		},
	}
	api.RegisterHandlers(e.Group("", strictPipelineJSON), api.NewStrictHandler(s, middlewares))
}

// The generated binder remains useful for route/response typing. This boundary also
// rejects unknown fields, trailing JSON and executable/credential-shaped additions.
var pipelineDocumentRequiredFields = []string{"slug", "name", "definition"}

func pipelineBodyContract(method, path string) (any, []string) {
	switch {
	case strings.HasSuffix(path, "/validate"):
		return nil, nil
	case method == http.MethodPut && strings.HasSuffix(path, "/document"):
		return &p.PipelineDraftCreate{}, pipelineDocumentRequiredFields
	case method == http.MethodPut && strings.HasSuffix(path, "/policy"):
		return &p.PipelinePolicyWrite{}, []string{expectedPipelineResourceVersion, "enabled", "allowed_version_ids"}
	case method == http.MethodPut:
		return &p.PipelineDraftWrite{}, []string{expectedPipelineResourceVersion, "name", "definition"}
	case method == http.MethodPost && strings.HasSuffix(path, "/publish"):
		return &p.PipelinePublish{}, []string{expectedPipelineResourceVersion, "request_key"}
	case method == http.MethodPost && strings.HasSuffix(path, "/pipelines"):
		return &p.PipelineDraftCreate{}, pipelineDocumentRequiredFields
	default:
		return nil, nil
	}
}
func requirePipelineFields(raw []byte, required []string) error {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return echo.NewHTTPError(400, "pipeline request must be a JSON object")
	}
	if value, provided := fields["rollback_defaults"]; provided && bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
		return echo.NewHTTPError(400, "rollback_defaults must be an object; use an empty object to clear defaults")
	}
	for _, field := range required {
		if len(fields[field]) == 0 || string(fields[field]) == "null" {
			return echo.NewHTTPError(400, "required pipeline field: "+field)
		}
	}
	return nil
}
func validatePipelineBody(request *http.Request, body any, required []string) error {
	maxPipelineRequestBytes := 32768
	if _, policy := body.(*p.PipelinePolicyWrite); policy {
		maxPipelineRequestBytes = 256 * 1024
	}
	raw, err := io.ReadAll(io.LimitReader(request.Body, int64(maxPipelineRequestBytes)+1))
	if err != nil {
		return err
	}
	if len(raw) > maxPipelineRequestBytes {
		return echo.NewHTTPError(413, "pipeline request exceeds its bounded size limit")
	}
	strict := json.NewDecoder(bytes.NewReader(raw))
	if _, err := pipelineJSONValue(strict, 0); err != nil {
		return echo.NewHTTPError(400, "pipeline request requires unique JSON keys and bounded nesting")
	}
	if err := requirePipelineFields(raw, required); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(body); err != nil {
		return echo.NewHTTPError(400, "invalid pipeline request: "+err.Error())
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return echo.NewHTTPError(400, "exactly one pipeline JSON object required")
	}
	request.Body = io.NopCloser(bytes.NewReader(raw))
	return nil
}
func strictPipelineJSON(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		body, required := pipelineBodyContract(c.Request().Method, c.Path())
		if body == nil {
			return next(c)
		}
		if err := validatePipelineBody(c.Request(), body, required); err != nil {
			return err
		}
		return next(c)
	}
}

func pipelineTransaction[T any](ctx context.Context, s *Server, org, project, permission string, write bool, action func(model.Tx, *model.Project, uuid.UUID) (T, error)) (T, error) {
	var zero T
	actor, err := GetAuthenticatedUserIdOr401(ctx)
	if err != nil {
		return zero, err
	}
	if err := s.checkProjectAuthorization(ctx, actor, org, project, permission); err != nil {
		return zero, err
	}
	isolation := sql.LevelRepeatableRead
	if write {
		isolation = sql.LevelReadCommitted
	}
	tx, err := s.Database.BeginTx(ctx, &sql.TxOptions{ReadOnly: !write, Isolation: isolation})
	if err != nil {
		return zero, err
	}
	defer func() { _ = tx.Rollback() }()
	var scope *model.Project
	if write {
		scope, err = model.LockPipelineProject(ctx, s.Database, tx, org, project)
	} else {
		scope, err = s.Database.GetProject(ctx, tx, org, project, model.GetModeDefault)
	}
	if err != nil {
		return zero, moduleDraftError(err)
	}
	value, err := action(tx, scope, actor)
	if err != nil {
		return zero, moduleDraftError(err)
	}
	if err := tx.Commit(); err != nil {
		return zero, err
	}
	return value, nil
}

func (s *Server) ListProjectPipelines(ctx context.Context, r api.ListProjectPipelinesRequestObject) (api.ListProjectPipelinesResponseObject, error) {
	value, err := pipelineTransaction(ctx, s, r.OrgId, r.ProjectId, PermissionPipelineRead, false, func(tx model.Tx, scope *model.Project, _ uuid.UUID) (p.PipelineCatalogue, error) {
		return model.ReadPipelineCatalogue(ctx, tx, scope)
	})
	return api.ListProjectPipelines200JSONResponse(pipelineJSON[api.PipelineCatalogue](value)), err
}
func (s *Server) CreateProjectPipeline(ctx context.Context, r api.CreateProjectPipelineRequestObject) (api.CreateProjectPipelineResponseObject, error) {
	if r.JSONBody == nil {
		return nil, echo.NewHTTPError(400, "pipeline draft required")
	}
	value, err := pipelineTransaction(ctx, s, r.OrgId, r.ProjectId, PermissionPipelineWrite, true, func(tx model.Tx, scope *model.Project, actor uuid.UUID) (p.PipelineDraft, error) {
		return model.CreatePipelineDraft(ctx, tx, scope, actor, pipelineJSON[p.PipelineDraftCreate](r.JSONBody))
	})
	return api.CreateProjectPipeline201JSONResponse(pipelineJSON[api.PipelineDraft](value)), err
}
func (s *Server) GetProjectPipelineDraft(ctx context.Context, r api.GetProjectPipelineDraftRequestObject) (api.GetProjectPipelineDraftResponseObject, error) {
	value, err := pipelineTransaction(ctx, s, r.OrgId, r.ProjectId, PermissionPipelineRead, false, func(tx model.Tx, scope *model.Project, _ uuid.UUID) (p.PipelineDraft, error) {
		return model.ReadPipelineDraft(ctx, tx, scope, r.PipelineId)
	})
	return api.GetProjectPipelineDraft200JSONResponse(pipelineJSON[api.PipelineDraft](value)), err
}
func (s *Server) SaveProjectPipelineDraft(ctx context.Context, r api.SaveProjectPipelineDraftRequestObject) (api.SaveProjectPipelineDraftResponseObject, error) {
	if r.Body == nil {
		return nil, echo.NewHTTPError(400, "pipeline draft required")
	}
	value, err := pipelineTransaction(ctx, s, r.OrgId, r.ProjectId, PermissionPipelineWrite, true, func(tx model.Tx, scope *model.Project, actor uuid.UUID) (p.PipelineDraft, error) {
		return model.SavePipelineDraft(ctx, tx, scope, r.PipelineId, actor, pipelineJSON[p.PipelineDraftWrite](r.Body))
	})
	return api.SaveProjectPipelineDraft200JSONResponse(pipelineJSON[api.PipelineDraft](value)), err
}
func (s *Server) PublishProjectPipeline(ctx context.Context, r api.PublishProjectPipelineRequestObject) (api.PublishProjectPipelineResponseObject, error) {
	if r.Body == nil {
		return nil, echo.NewHTTPError(400, "publication request required")
	}
	value, err := pipelineTransaction(ctx, s, r.OrgId, r.ProjectId, PermissionPipelinePublish, true, func(tx model.Tx, scope *model.Project, actor uuid.UUID) (p.PublishedPipelineVersion, error) {
		return model.PublishPipeline(ctx, tx, scope, r.PipelineId, actor, pipelineJSON[p.PipelinePublish](r.Body))
	})
	return api.PublishProjectPipeline200JSONResponse(pipelineJSON[api.PublishedPipelineVersion](value)), err
}
func (s *Server) GetPublishedProjectPipeline(ctx context.Context, r api.GetPublishedProjectPipelineRequestObject) (api.GetPublishedProjectPipelineResponseObject, error) {
	actor, err := GetAuthenticatedUserIdOr401(ctx)
	if err != nil {
		return nil, err
	}
	permission := PermissionPipelineRead
	if err := s.checkProjectAuthorization(ctx, actor, r.OrgId, r.ProjectId, permission); err != nil {
		permission = "deployment_read"
	}
	value, err := pipelineTransaction(ctx, s, r.OrgId, r.ProjectId, permission, false, func(tx model.Tx, scope *model.Project, _ uuid.UUID) (p.PublishedPipelineVersion, error) {
		return model.ReadPublishedPipeline(ctx, tx, scope, r.VersionId)
	})
	return api.GetPublishedProjectPipeline200JSONResponse(pipelineJSON[api.PublishedPipelineVersion](value)), err
}
func (s *Server) RetireProjectPipeline(ctx context.Context, r api.RetireProjectPipelineRequestObject) (api.RetireProjectPipelineResponseObject, error) {
	value, err := pipelineTransaction(ctx, s, r.OrgId, r.ProjectId, PermissionPipelinePublish, true, func(tx model.Tx, scope *model.Project, actor uuid.UUID) (p.PublishedPipelineVersion, error) {
		return model.RetirePipeline(ctx, tx, scope, r.VersionId, actor)
	})
	return api.RetireProjectPipeline200JSONResponse(pipelineJSON[api.PublishedPipelineVersion](value)), err
}
func (s *Server) GetProjectPipelinePolicy(ctx context.Context, r api.GetProjectPipelinePolicyRequestObject) (api.GetProjectPipelinePolicyResponseObject, error) {
	value, err := pipelineTransaction(ctx, s, r.OrgId, r.ProjectId, PermissionPipelineRead, false, func(tx model.Tx, scope *model.Project, _ uuid.UUID) (p.PipelinePolicy, error) {
		return model.ReadPipelinePolicy(ctx, tx, scope.Uuid)
	})
	return api.GetProjectPipelinePolicy200JSONResponse(pipelineJSON[api.PipelinePolicy](value)), err
}
func (s *Server) SetProjectPipelinePolicy(ctx context.Context, r api.SetProjectPipelinePolicyRequestObject) (api.SetProjectPipelinePolicyResponseObject, error) {
	if r.Body == nil {
		return nil, echo.NewHTTPError(400, "project policy required")
	}
	value, err := pipelineTransaction(ctx, s, r.OrgId, r.ProjectId, PermissionPipelinePublish, true, func(tx model.Tx, scope *model.Project, actor uuid.UUID) (p.PipelinePolicy, error) {
		return model.SetPipelinePolicy(ctx, tx, scope, actor, pipelineJSON[p.PipelinePolicyWrite](r.Body))
	})
	return api.SetProjectPipelinePolicy200JSONResponse(pipelineJSON[api.PipelinePolicy](value)), err
}
func (s *Server) authorizePipelineChoices(ctx context.Context, org, project, environment string) error {
	actor, err := GetAuthenticatedUserIdOr401(ctx)
	if err != nil {
		return err
	}
	if err := s.checkEnvAuthorization(ctx, actor, org, project, environment, "deployment_read"); err == nil {
		return nil
	}
	return s.checkEnvAuthorization(ctx, actor, org, project, environment, "deployment_write")
}
func readPipelineChoices(ctx context.Context, tx model.Tx, project *model.Project) (p.PipelineChoices, error) {
	policy, err := model.ReadPipelinePolicy(ctx, tx, project.Uuid)
	if err != nil {
		return p.PipelineChoices{}, err
	}
	versions, err := model.ListPipelineVersions(ctx, tx, project)
	if err != nil {
		return p.PipelineChoices{}, err
	}
	choices := p.PipelineChoices{Policy: policy, Versions: []p.PublishedPipelineVersion{}}
	for _, version := range versions {
		if version.RetiredAt == nil && (!policy.Enabled || slices.Contains(policy.AllowedVersionIds, version.Id)) {
			choices.Versions = append(choices.Versions, version)
		}
	}
	return choices, nil
}
func (s *Server) ListEnvironmentPipelineChoices(ctx context.Context, r api.ListEnvironmentPipelineChoicesRequestObject) (api.ListEnvironmentPipelineChoicesResponseObject, error) {
	if err := s.authorizePipelineChoices(ctx, r.OrgId, r.ProjectId, r.EnvId); err != nil {
		return nil, err
	}
	tx, err := s.Database.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	env, err := s.Database.GetEnvironment(ctx, tx, r.OrgId, r.ProjectId, r.EnvId, model.GetModeDefault)
	if err != nil {
		return nil, err
	}
	choices, err := s.environmentPipelineChoices(ctx, tx, env)
	if err != nil {
		return nil, err
	}
	return api.ListEnvironmentPipelineChoices200JSONResponse(pipelineJSON[api.PipelineChoices](choices)), nil
}

var _ api.StrictServerInterface = (*Server)(nil)
