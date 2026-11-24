package api

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	api "github.com/stellwerk-labs/platform-orchestrator-cp/internal/pipelineapi"
	p "github.com/stellwerk-labs/platform-orchestrator-cp/shared/v3/pipelineapi"
)

//go:embed pipeline-document.schema.json
var pipelineDocumentSchema []byte

const maxPipelineDocumentBytes = 32768
const maxPipelineDocumentDepth = 24

type pipelineValidationKey struct{}

var pipelineSourcePath = regexp.MustCompile(`^/orgs/[^/]+/projects/[^/]+/pipelines(?:/validate|/[^/]+/document)?$`)
var pipelineSchema = sync.OnceValues(func() (*openapi3.Schema, error) {
	spec, err := api.GetSpec()
	if err != nil {
		return nil, err
	}
	return spec.Components.Schemas["PipelineDraftCreate"].Value, nil
})

// Parse before the generic OpenAPI body decoder, whose YAML decoder permits aliases.
// Authentication and authorization still run through the ordinary strict handlers.
func pipelineSourceBoundary(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		req := c.Request()
		if (req.Method != http.MethodPost && req.Method != http.MethodPut) || !pipelineSourcePath.MatchString(req.URL.Path) {
			return next(c)
		}
		result, err := readPipelineDocument(req)
		if err != nil {
			return err
		}
		validation := strings.HasSuffix(req.URL.Path, "/validate")
		if !validation && !result.Valid {
			return echo.NewHTTPError(400, result.Errors[0].Message)
		}
		normalizePipelineRequest(c, result, validation)
		return next(c)
	}
}

func readPipelineDocument(req *http.Request) (p.PipelineDocumentValidation, error) {
	raw, err := io.ReadAll(io.LimitReader(req.Body, maxPipelineDocumentBytes+1))
	if err != nil {
		return p.PipelineDocumentValidation{}, echo.NewHTTPError(400, "cannot read pipeline document")
	}
	if len(raw) > maxPipelineDocumentBytes {
		return p.PipelineDocumentValidation{}, echo.NewHTTPError(413, "pipeline document exceeds 32 KiB")
	}
	kind, _, err := mime.ParseMediaType(req.Header.Get(echo.HeaderContentType))
	if err != nil || (kind != "application/json" && kind != "application/yaml") {
		return p.PipelineDocumentValidation{}, echo.NewHTTPError(415, "pipeline documents require application/json or application/yaml")
	}
	return validatePipelineDocument(raw, kind), nil
}

func normalizePipelineRequest(c echo.Context, result p.PipelineDocumentValidation, validation bool) {
	req := c.Request()
	req = req.WithContext(context.WithValue(req.Context(), pipelineValidationKey{}, result))
	normalized := []byte(`{}`)
	if !validation {
		normalized, _ = json.Marshal(result.Document)
	}
	req.Body = io.NopCloser(bytes.NewReader(normalized))
	req.ContentLength = int64(len(normalized))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	c.SetRequest(req)
}

func documentError(code, path, message string) p.PipelineDocumentValidation {
	return p.PipelineDocumentValidation{Valid: false, Errors: []p.PipelineDocumentError{{Code: code, Path: path, Message: message}}}
}

func validatePipelineDocument(raw []byte, kind string) p.PipelineDocumentValidation {
	if len(raw) > maxPipelineDocumentBytes || !utf8.Valid(raw) {
		return documentError("source", "", "Document must be UTF-8 and at most 32 KiB.")
	}
	value, err := decodePipelineDocument(raw, kind)
	if err != nil {
		return documentError("source", "", "Invalid document. Use one JSON/YAML object without duplicate keys, anchors, aliases, merge keys, custom tags or excessive nesting.")
	}
	schema, err := pipelineSchema()
	if err != nil {
		return documentError("schema_unavailable", "", "Pipeline schema is unavailable.")
	}
	if err = schema.VisitJSON(value, openapi3.MultiErrors(), openapi3.SetSchemaErrorMessageCustomizer(func(*openapi3.SchemaError) string { return "invalid pipeline document" })); err != nil {
		return p.PipelineDocumentValidation{Errors: pipelineSchemaErrors(err)}
	}
	return validatePipelineSemantics(value)
}

func pipelineSchemaErrors(err error) []p.PipelineDocumentError {
	switch typed := err.(type) {
	case openapi3.MultiError:
		violations := []p.PipelineDocumentError{}
		for _, child := range typed {
			violations = append(violations, pipelineSchemaErrors(child)...)
		}
		return violations
	case *openapi3.SchemaError:
		path := ""
		for _, part := range typed.JSONPointer() {
			path += "/" + strings.ReplaceAll(strings.ReplaceAll(part, "~", "~0"), "/", "~1")
		}
		return []p.PipelineDocumentError{{Code: typed.SchemaField, Path: path, Message: "Document does not satisfy schema rule: " + typed.SchemaField + "."}}
	default:
		return []p.PipelineDocumentError{{Code: "schema", Path: "", Message: "Document does not match the pipeline schema."}}
	}
}

func validatePipelineSemantics(value any) p.PipelineDocumentValidation {
	normalized, err := json.Marshal(value)
	if err != nil {
		return documentError("source", "", "Document contains a value that JSON cannot represent.")
	}
	var document p.PipelineDraftCreate
	if err = json.Unmarshal(normalized, &document); err != nil {
		return documentError("schema", "", "Document does not match the pipeline schema.")
	}
	if err = p.ValidateDraft(document.Slug, document.Name, document.Definition); err != nil {
		return documentError("semantics", "/definition", err.Error())
	}
	return p.PipelineDocumentValidation{Valid: true, Document: &document, Errors: []p.PipelineDocumentError{}}
}

func (s *Server) pipelineAuthorPermission(ctx context.Context, org, project string) (string, error) {
	actor, err := GetAuthenticatedUserIdOr401(ctx)
	if err != nil {
		return "", err
	}
	if err = s.checkProjectAuthorization(ctx, actor, org, project, PermissionPipelineRead); err == nil {
		return PermissionPipelineRead, nil
	}
	return PermissionPipelineWrite, s.checkProjectAuthorization(ctx, actor, org, project, PermissionPipelineWrite)
}
func (s *Server) GetProjectPipelineSchema(ctx context.Context, r api.GetProjectPipelineSchemaRequestObject) (api.GetProjectPipelineSchemaResponseObject, error) {
	permission, err := s.pipelineAuthorPermission(ctx, r.OrgId, r.ProjectId)
	if err != nil {
		return nil, err
	}
	_, err = pipelineTransaction(ctx, s, r.OrgId, r.ProjectId, permission, false, func(tx model.Tx, scope *model.Project, _ uuid.UUID) (bool, error) { return true, nil })
	if err != nil {
		return nil, err
	}
	var schema map[string]any
	if err = json.Unmarshal(pipelineDocumentSchema, &schema); err != nil {
		return nil, err
	}
	return api.GetProjectPipelineSchema200JSONResponse(schema), nil
}
func (s *Server) ValidateProjectPipelineDocument(ctx context.Context, r api.ValidateProjectPipelineDocumentRequestObject) (api.ValidateProjectPipelineDocumentResponseObject, error) {
	permission, err := s.pipelineAuthorPermission(ctx, r.OrgId, r.ProjectId)
	if err != nil {
		return nil, err
	}
	result, err := pipelineTransaction(ctx, s, r.OrgId, r.ProjectId, permission, false, func(tx model.Tx, scope *model.Project, _ uuid.UUID) (p.PipelineDocumentValidation, error) {
		value, ok := ctx.Value(pipelineValidationKey{}).(p.PipelineDocumentValidation)
		if !ok {
			return p.PipelineDocumentValidation{}, echo.NewHTTPError(400, "raw pipeline document required")
		}
		return value, nil
	})
	return api.ValidateProjectPipelineDocument200JSONResponse(pipelineJSON[api.PipelineDocumentValidation](result)), err
}
func pipelineETag(version int64) string { return fmt.Sprintf("\"%d\"", version) }
func pipelineExpectedVersion(tag string) (int64, error) {
	if len(tag) < 3 || tag[0] != '"' || tag[len(tag)-1] != '"' {
		return 0, echo.NewHTTPError(400, "If-Match must contain one quoted positive draft resource version")
	}
	version, err := strconv.ParseInt(tag[1:len(tag)-1], 10, 64)
	if err != nil || version < 1 || pipelineETag(version) != tag {
		return 0, echo.NewHTTPError(400, "invalid pipeline If-Match resource version")
	}
	return version, nil
}
func (s *Server) GetProjectPipelineDocument(ctx context.Context, r api.GetProjectPipelineDocumentRequestObject) (api.GetProjectPipelineDocumentResponseObject, error) {
	value, err := pipelineTransaction(ctx, s, r.OrgId, r.ProjectId, PermissionPipelineRead, false, func(tx model.Tx, scope *model.Project, _ uuid.UUID) (p.PipelineDraft, error) {
		return model.ReadPipelineDraft(ctx, tx, scope, r.PipelineId)
	})
	if err != nil {
		return nil, err
	}
	return api.GetProjectPipelineDocument200JSONResponse{Body: pipelineJSON[api.PipelineDraftCreate](p.PipelineDraftCreate{Slug: value.Slug, Name: value.Name, Definition: value.Definition}), Headers: api.GetProjectPipelineDocument200ResponseHeaders{ETag: pipelineETag(value.ResourceVersion)}}, nil
}
func (s *Server) SaveProjectPipelineDocument(ctx context.Context, r api.SaveProjectPipelineDocumentRequestObject) (api.SaveProjectPipelineDocumentResponseObject, error) {
	if r.JSONBody == nil {
		return nil, echo.NewHTTPError(400, "pipeline document required")
	}
	expected, err := pipelineExpectedVersion(r.Params.IfMatch)
	if err != nil {
		return nil, err
	}
	value, err := pipelineTransaction(ctx, s, r.OrgId, r.ProjectId, PermissionPipelineWrite, true, func(tx model.Tx, scope *model.Project, actor uuid.UUID) (p.PipelineDraft, error) {
		current, err := model.ReadPipelineDraft(ctx, tx, scope, r.PipelineId)
		if err != nil {
			return current, err
		}
		if current.Slug != r.JSONBody.Slug {
			return current, model.NewErrBadRequest("pipeline slug is immutable")
		}
		return model.SavePipelineDraft(ctx, tx, scope, r.PipelineId, actor, p.PipelineDraftWrite{ExpectedResourceVersion: expected, Name: r.JSONBody.Name, Definition: pipelineJSON[p.PipelineRecipe](r.JSONBody.Definition)})
	})
	if err != nil {
		return nil, err
	}
	return api.SaveProjectPipelineDocument200JSONResponse{Body: pipelineJSON[api.PipelineDraft](value), Headers: api.SaveProjectPipelineDocument200ResponseHeaders{ETag: pipelineETag(value.ResourceVersion)}}, nil
}
func (s *Server) DeleteProjectPipelineDraft(ctx context.Context, r api.DeleteProjectPipelineDraftRequestObject) (api.DeleteProjectPipelineDraftResponseObject, error) {
	expected, err := pipelineExpectedVersion(r.Params.IfMatch)
	if err != nil {
		return nil, err
	}
	_, err = pipelineTransaction(ctx, s, r.OrgId, r.ProjectId, PermissionPipelineWrite, true, func(tx model.Tx, scope *model.Project, actor uuid.UUID) (bool, error) {
		return true, model.ArchivePipelineDraft(ctx, tx, scope, r.PipelineId, actor, expected)
	})
	if err != nil {
		return nil, err
	}
	return api.DeleteProjectPipelineDraft204Response{}, nil
}
