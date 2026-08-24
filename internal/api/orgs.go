package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/pkg/errors"
	"github.com/stellwerk-labs/golib/herrors"
	"github.com/stellwerk-labs/golib/hlogger"
	"github.com/stellwerk-labs/golib/hmessaging/reliableoutbox"
	"go.uber.org/zap"

	"github.com/stellwerk-labs/platform-orchestrator-iam/shared/authz"
	orchestratoriam "github.com/stellwerk-labs/platform-orchestrator-iam/shared/genclient"
	"github.com/stellwerk-labs/platform-orchestrator-iam/shared/userid"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/api/middleware"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/events"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/logging"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/model"
	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/ref"

	"github.com/stellwerk-labs/platform-orchestrator-cp/shared/genevents"
)

const (
	// newOrgAdminRoleDisplayName is the org display name to look up to register the first user.
	// This is controlled by the IAM service.
	newOrgAdminRoleDisplayName = "Admin"
)

func dbOrgToApiInternalOrg(item model.Org) InternalOrganization {
	return InternalOrganization{
		Id:        item.Id,
		Uuid:      item.Uuid,
		CreatedAt: item.CreatedAt,
		CreatedBy: item.CreatedBy,
		UpdatedAt: item.UpdatedAt.Ref(),
		Status:    InternalOrganizationStatus(item.Status),
		Source:    InternalOrganizationSource(item.Source),
		Plan:      string(item.Plan),
	}
}

func dbOrgToApiOrg(item model.Org) Organization {
	return Organization{
		Id:        item.Id,
		Uuid:      item.Uuid,
		CreatedAt: item.CreatedAt,
		CreatedBy: item.CreatedBy,
		UpdatedAt: item.UpdatedAt.Ref(),
		Status:    OrganizationStatus(item.Status),
		Plan:      string(item.Plan),
	}
}

func (s *Server) ListInternalOrganizations(ctx context.Context, request ListInternalOrganizationsRequestObject) (ListInternalOrganizationsResponseObject, error) {
	page, next, err := s.Database.ListOrgs(ctx, nil, ref.DerefOr(request.Params.Page, ""), ref.DerefOr(request.Params.PerPage, 100), model.ListOrgsParams{})
	if err != nil {
		return nil, errors.Wrap(err, "failed to list organizations")
	}
	out := make([]InternalOrganization, len(page))
	for i, item := range page {
		out[i] = dbOrgToApiInternalOrg(item)
	}
	return ListInternalOrganizations200JSONResponse{
		Items:         out,
		NextPageToken: ref.RefStringEmptyNil(next),
	}, nil
}

func (s *Server) GetInternalOrganization(ctx context.Context, request GetInternalOrganizationRequestObject) (GetInternalOrganizationResponseObject, error) {
	if out, err := s.Database.GetOrg(ctx, nil, request.OrgId); err != nil {
		if me, ok := model.IsErrNotFound(err); ok {
			return GetInternalOrganization404JSONResponse{N404NotFoundJSONResponse: Generate404FromModelErr(me)}, nil
		}
		return nil, errors.Wrap(err, "failed to get organization")
	} else {
		return GetInternalOrganization200JSONResponse(dbOrgToApiInternalOrg(*out)), nil
	}
}

func (s *Server) GetOrganization(ctx context.Context, request GetOrganizationRequestObject) (GetOrganizationResponseObject, error) {
	uid, herr := GetAuthenticatedUserIdOr401(ctx)
	if herr != nil {
		return nil, herr
	} else if err := s.checkOrgAuthorization(ctx, uid, request.OrgId, authz.PermissionOrganizationRead); err != nil {
		return nil, err
	}
	if out, err := s.Database.GetOrg(ctx, nil, request.OrgId); err != nil {
		if me, ok := model.IsErrNotFound(err); ok {
			return GetOrganization404JSONResponse{N404NotFoundJSONResponse: Generate404FromModelErr(me)}, nil
		}
		return nil, errors.Wrap(err, "failed to get organization")
	} else {
		return GetOrganization200JSONResponse(dbOrgToApiOrg(*out)), nil
	}
}

func (s *Server) createOrganizationCore(
	ctx context.Context,
	logger *zap.Logger,
	tx model.TxWithCommit,
	req model.Org,
	optionalAdminUser *uuid.UUID,
) (*model.Org, error) {
	if req.CreatedAt.IsZero() {
		req.CreatedAt = time.Now().UTC()
	}
	if req.Source == "" {
		req.Source = model.OrgSourcePublic
	}
	if req.Plan == "" {
		req.Plan = model.OrgPlanCustom
	}
	org, err := s.Database.CreateOrg(ctx, tx, &req)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create organization")
	}
	logger = logger.With(logging.ZapOrgId(org.Id))
	messages, err := s.Database.InsertPendingEventMessages(ctx, tx, events.AsMessages(events.CloudEvent[any]{
		Type: genevents.IoPlatformOrchestratorOrganizationCreated,
		Time: org.CreatedAt,
		Data: genevents.OrgChangedData{OrgId: org.Id, OrgUuid: org.Uuid},
	}))
	if err != nil {
		return nil, errors.Wrap(err, "failed to insert pending event messages")
	}

	// we must persist the organization creation event before creating the membership
	if err := tx.Commit(); err != nil {
		return nil, errors.Wrap(err, "failed to commit transaction")
	}

	if optionalAdminUser != nil {
		var createMembershipError error

		if roleResp, err := s.IamClient.ListRolesWithResponse(ctx, org.Id, &orchestratoriam.ListRolesParams{}); err != nil {
			createMembershipError = errors.Wrapf(err, "failed to list roles for organization %s", org.Id)
		} else if roleResp.StatusCode() != http.StatusOK {
			createMembershipError = errors.Errorf("failed to list roles for organization %s: unexpected status code %v", org.Id, roleResp.Status())
		} else if roleIdx := slices.IndexFunc(roleResp.JSON200.Items, func(role orchestratoriam.Role) bool {
			return role.DisplayName == newOrgAdminRoleDisplayName
		}); roleIdx < 0 {
			createMembershipError = errors.Errorf("failed to list roles for organization %s: role Admin not found", org.Id)
		} else if r, err := s.IamClient.InternalCreateOrgMembershipWithResponse(ctx, org.Id, orchestratoriam.InternalMembershipCreateBody{
			UserId:      *optionalAdminUser,
			SubjectType: "role",
			Subject:     roleResp.JSON200.Items[roleIdx].Id.String(),
		}); err != nil {
			createMembershipError = errors.Wrapf(err, "failed to add user %s to organization %s", *optionalAdminUser, org.Id)
		} else if r.StatusCode() == http.StatusNotFound {
			createMembershipError = herrors.NewWithStatus(http.StatusNotFound, fmt.Sprintf("failed to add user %s to organization %s: %s", *optionalAdminUser, org.Id, r.JSON404.Message), nil)
		} else if r.StatusCode() == http.StatusConflict {
			createMembershipError = herrors.NewWithStatus(http.StatusConflict, fmt.Sprintf("failed to add user %s to organization %s: %s", *optionalAdminUser, org.Id, r.JSON409.Message), nil)
		} else if r.StatusCode() != http.StatusCreated {
			createMembershipError = errors.Errorf("failed to add user %s to organization %s: unexpected status code %v", *optionalAdminUser, org.Id, r.Status())
		} else {
			logger.Info("user added to organization")
		}

		// if something went wrong, we need to remove the organization we created
		if createMembershipError != nil {
			if err := s.Database.DeleteOrg(ctx, nil, org.Id); err != nil {
				logger.Error("failed to delete organization", zap.Error(err))
			}
			return nil, createMembershipError
		}
	}

	logger.Info("created org", logging.ZapOrgId(org.Id), logging.ZapOrgUuid(org.Uuid))
	reliableoutbox.OptimisticPublish(ctx, logger, s.Database.AsReliableOutboxStore(), s.Publisher, messages)
	return org, nil
}

func (s *Server) CreateInternalOrganization(ctx context.Context, request CreateInternalOrganizationRequestObject) (CreateInternalOrganizationResponseObject, error) {
	ids, ctx := hlogger.EnsurePlatformOrchestratorIdsOnCtx(ctx)
	logger := hlogger.TraceScopedLoggerFromCtx(s.Logger, ctx).WithLazy(ids.AsLogField())

	var newOrgId string
	if request.Body.Id != nil {
		newOrgId = *request.Body.Id
	} else {
		newOrgId = generateRandomOrgNameAndSuffix(rand.IntN, ref.DerefOr(request.Body.IdPrefix, ""))
	}
	ids.OrgId = newOrgId

	tx, err := s.Database.BeginTx(ctx, nil)
	if err != nil {
		return nil, errors.Wrap(err, "failed to begin transaction")
	}
	defer func() {
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			logger.Error("failed to rollback transaction", zap.Error(err))
		}
	}()

	org, err := s.createOrganizationCore(ctx, logger, tx, model.Org{Id: newOrgId, CreatedAt: time.Now().UTC(), CreatedBy: userid.InternalSystemUuid, Source: model.OrgSourceInternal}, nil)
	if err != nil {
		if me, ok := model.IsErrConflict(err); ok {
			return CreateInternalOrganization409JSONResponse{N409ConflictJSONResponse: Generate409FromModelErr(me)}, nil
		}
		return nil, errors.Wrap(err, "failed to create organization")
	}
	return CreateInternalOrganization201JSONResponse(dbOrgToApiInternalOrg(*org)), nil
}

// Create a new organization with a random name and suffix.
// (POST /orgs)
const forbiddenErrorCode = "HTTP-403"

func (s *Server) CreateOrganization(ctx context.Context, request CreateOrganizationRequestObject) (CreateOrganizationResponseObject, error) {
	uid, herr := GetAuthenticatedUserIdOr401(ctx)
	if herr != nil {
		return nil, herr
	} else if userid.IsServiceUser(uid) {
		return CreateOrganization403JSONResponse{N403ForbiddenJSONResponse: N403ForbiddenJSONResponse{Error: forbiddenErrorCode, Message: "service users are not allowed to create organizations"}}, nil
	} else {
		middleware.SetAuthCheckedCtx(ctx)
	}
	ids, ctx := hlogger.EnsurePlatformOrchestratorIdsOnCtx(ctx)
	ids.UserId = uid.String()
	logger := hlogger.TraceScopedLoggerFromCtx(s.Logger, ctx).WithLazy(ids.AsLogField())
	if resp, err := s.IamClient.ListUserMembershipsWithResponse(ctx, uid, &orchestratoriam.ListUserMembershipsParams{}); err != nil {
		return nil, err
	} else if resp.StatusCode() == http.StatusForbidden {
		var herr *herrors.PlatformOrchestratorError
		if err := json.Unmarshal(resp.Body, &herr); err != nil || herr.Message == "" {
			return CreateOrganization403JSONResponse{N403ForbiddenJSONResponse: N403ForbiddenJSONResponse{Error: forbiddenErrorCode, Message: fmt.Sprintf("user %s is not allowed to create organizations", uid)}}, nil
		} else {
			return CreateOrganization403JSONResponse{N403ForbiddenJSONResponse: N403ForbiddenJSONResponse{Error: forbiddenErrorCode, Message: herr.Message}}, nil
		}
	} else if resp.StatusCode() != http.StatusOK {
		return nil, errors.Errorf("unexpected status code getting current user memberships: %v", resp.Status())
	} else if len(resp.JSON200.Items) > 0 {
		memberships := make([]string, len(resp.JSON200.Items))
		for i, membership := range resp.JSON200.Items {
			memberships[i] = membership.OrgId
		}
		return CreateOrganization409JSONResponse{N409ConflictJSONResponse: Generate409Response(fmt.Sprintf("user %s is already a member of organizations %v", uid, memberships))}, nil
	}
	tx, err := s.Database.BeginTx(ctx, nil)
	if err != nil {
		return nil, errors.Wrap(err, "failed to begin transaction")
	}
	defer func() {
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			logger.Error("failed to rollback transaction", zap.Error(err))
		}
	}()

	newOrgId := generateRandomOrgNameAndSuffix(rand.IntN, "")
	org, err := s.createOrganizationCore(
		ctx, logger, tx, model.Org{Id: newOrgId, CreatedAt: time.Now().UTC(), CreatedBy: uid, Source: model.OrgSourcePublic},
		&uid,
	)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create organization")
	}
	return CreateOrganization201JSONResponse(dbOrgToApiOrg(*org)), nil
}

const trueAdjective = "true"

var orgNameAdjectives = []string{
	"accomplished", "acoustic", "active", "adaptable", "adorable", "advanced", "adventurous", "aerodynamic", "agile",
	"alert", "amazing", "amber", "ancient", "arctic", "artistic", "astute", "atomic", "authentic", "automated", "azure",
	"balanced", "balmy", "beautiful", "big", "binary", "blazing", "blissful", "blooming", "bold", "bouncy", "brave",
	"breezy", "bright", "brilliant", "brisk", "bronze", "bubbly", "calm", "caring", "celestial", "charming", "cheerful",
	"chiming", "circular", "classic", "clean", "clear", "clever", "close", "coastal", "colossal", "colorful",
	"comfortable", "compact", "complete", "confident", "cool", "copper", "coral", "cosmic", "cozy", "crafty",
	"creative", "crimson", "crisp", "crystalline", "curious", "curved", "cushy", "cyber", "dancing", "daring",
	"dazzling", "deep", "delicate", "delightful", "dense", "determined", "dewy", "diamond", "digital", "diplomatic",
	"distant", "dynamic", "eager", "echoing", "electric", "elegant", "elite", "emerald", "enchanted", "energetic",
	"enormous", "enthusiastic", "eternal", "ethereal", "excellent", "exceptional", "excited", "expert", "expressive",
	"extraordinary", "fabulous", "fair", "familiar", "fantastic", "fast", "fearless", "fierce", "fine", "flexible",
	"flowing", "fluffy", "fluid", "focused", "foggy", "free", "fresh", "friendly", "funky", "futuristic", "fuzzy",
	"gentle", "giant", "gifted", "gigantic", "gleaming", "glowing", "golden", "graceful", "grand", "great", "green",
	"groovy", "happy", "hard", "harmonic", "harmonious", "hazy", "healthy", "heavy", "heroic", "high", "honest",
	"huge", "hyper", "ideal", "imaginative", "immense", "incredible", "independent", "infinite", "ingenious",
	"innovative", "inspired", "instant", "intelligent", "intense", "inventive", "inviting", "jazzy", "jeweled",
	"joyful", "joyous", "keen", "kind", "large", "leafy", "light", "lively", "logical", "loving",
	"loyal", "lucky", "luminous", "lunar", "magical", "magnetic", "magnificent", "majestic", "marvelous", "massive",
	"masterful", "mega", "melodic", "merry", "micro", "mighty", "mild", "mini", "misty", "modern", "musical",
	"mystical", "nano", "natural", "neat", "new", "nifty", "nimble", "noble", "oceanic", "open", "optimistic",
	"organic", "original", "outstanding", "panoramic", "passionate", "peaceful", "peppy", "perfect", "petite",
	"pioneering", "playful", "pleasant", "pleased", "plush", "polar", "polished", "positive", "powerful", "precious",
	"premium", "pretty", "prime", "pristine", "progressive", "pure", "quantum", "quick", "quiet", "radiant", "rapid",
	"rare", "rational", "reasonable", "refined", "reliable", "remarkable", "resonant", "revolutionary", "rhythmic",
	"rich", "robust", "round", "savvy", "seamless", "secure", "serene", "sharp", "shimmering", "shiny", "silent",
	"silky", "silver", "simple", "singing", "skillful", "sleek", "small", "smart", "smooth", "snappy", "snazzy",
	"soft", "solar", "solid", "sonic", "sparkling", "special", "spectacular", "speedy", "spiffy", "spirited",
	"splendid", "spunky", "square", "stable", "starry", "steady", "stellar", "stormy", "streamlined", "strong",
	"stunning", "sturdy", "successful", "sunny", "super", "superb", "supple", "supreme", "swift", "talented", "tall",
	"tender", "thick", "thunderous", "timeless", "tiny", "top", "tough", "towering", "transparent", "tropical",
	"triumphant", trueAdjective, "turbo", "ultra", "unique", "universal", "upbeat", "vast", "velvet", "versatile", "vibrant",
	"victorious", "vintage", "visionary", "vivid", "warm", "wide", "wild", "winning", "wireless", "wise", "witty",
	"wonderful", "zesty", "zippy",
}

var orgNameNouns = []string{
	"albatross", "alligator", "alpaca", "anchovy", "angelfish", "ant", "anteater", "antelope", "ape", "armadillo",
	"barracuda", "bass", "bat", "bear", "beaver", "bee", "beetle", "bison", "blackbird", "bluebird", "bobcat", "buffalo",
	"bull", "bulldog", "butterfly", "buzzard", "camel", "canary", "cardinal", "caribou", "carp", "cat", "caterpillar",
	"catfish", "cheetah", "chicken", "chipmunk", "clam", "cobra", "cod", "condor", "coot", "coral", "coyote", "crab",
	"crane", "cricket", "crocodile", "crow", "deer", "dingo", "dog", "dolphin", "donkey", "dove", "dragonfly", "duck",
	"eagle", "eel", "elephant", "elk", "emu", "falcon", "ferret", "finch", "fish", "flamingo", "flounder", "fly", "fox",
	"frog", "gazelle", "gecko", "giraffe", "goat", "goose", "gopher", "gorilla", "grasshopper", "grouper", "grouse",
	"gull", "hamster", "hare", "hawk", "hedgehog", "heron", "hippo", "horse", "hummingbird", "hyena", "ibis", "iguana",
	"impala", "jackal", "jaguar", "jay", "jellyfish", "kangaroo", "kingfisher", "koala", "ladybug", "lamb", "leopard",
	"lion", "lizard", "llama", "lobster", "lynx", "macaw", "mackerel", "magpie", "manta", "marlin", "meerkat", "mole",
	"mongoose", "monkey", "moose", "moth", "mouse", "mule", "newt", "nightingale", "octopus", "orca", "otter", "owl",
	"ox", "oyster", "panda", "panther", "parrot", "peacock", "pelican", "penguin", "perch", "pig", "pigeon", "pike",
	"platypus", "porcupine", "porpoise", "puma", "quail", "rabbit", "raccoon", "ram", "rat", "raven", "ray", "rhino",
	"robin", "salamander", "salmon", "sardine", "scorpion", "seal", "shark", "sheep", "shrimp", "skunk", "sloth",
	"snail", "snake", "snapper", "sparrow", "spider", "squid", "squirrel", "starfish", "stingray", "stork", "swan",
	"swordfish", "tiger", "toad", "tortoise", "toucan", "trout", "tuna", "turkey", "turtle", "viper", "walrus",
	"wasp", "whale", "wolf", "woodpecker", "worm", "wren", "yak", "zebra",
}

func generateRandomOrgNameAndSuffix(rf func(n int) int, prefix string) string {
	var name string
	if prefix == "" {
		prefix = orgNameAdjectives[rf(len(orgNameAdjectives))]
	}
	for name == "" || len(name) > 50 {
		name = fmt.Sprintf(
			"%s-%s-%s-%04d",
			prefix,
			orgNameAdjectives[rf(len(orgNameAdjectives))],
			orgNameNouns[rf(len(orgNameNouns))],
			rf(10000),
		)
	}
	return name
}
