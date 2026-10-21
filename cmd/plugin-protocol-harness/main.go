package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/plugins"
	"github.com/stellwerk-labs/platform-orchestrator-iam/shared/userid"
)

const (
	actorHeader     = "X-Plugin-Harness-Actor"
	maxBody         = 16 << 20
	responseCode    = "code"
	responseMessage = "message"
)

type config struct {
	ListenAddress string
	BrowserOrigin string
	OrgID         string
	ActorID       string
}

type invocationResponse struct {
	Status int             `json:"status"`
	Body   json.RawMessage `json:"body"`
}

func main() {
	var manifestPath, backend, uiOrigin, signingKey, issuer string
	var cfg config
	flag.StringVar(&cfg.ListenAddress, "listen", "127.0.0.1:18093", "loopback listen address")
	flag.StringVar(&cfg.BrowserOrigin, "browser-origin", "", "exact browser origin permitted by CORS")
	flag.StringVar(&cfg.OrgID, "org", "", "single local harness organization")
	flag.StringVar(&cfg.ActorID, "actor", "", "single local harness actor UUID")
	flag.StringVar(&manifestPath, "manifest", "", "absolute plugin registration manifest JSON path")
	flag.StringVar(&backend, "backend", "", "plugin backend origin")
	flag.StringVar(&uiOrigin, "ui-origin", "", "plugin UI origin, when the manifest contributes a UI")
	flag.StringVar(&signingKey, "signing-key", "", "absolute PKCS8 Ed25519 private key path")
	flag.StringVar(&issuer, "issuer", "cp-local-protocol-harness", "plugin context issuer")
	flag.Parse()

	if err := validateConfig(cfg); err != nil {
		log.Fatal(err)
	}
	manifest, err := readManifest(manifestPath)
	if err != nil {
		log.Fatal(err)
	}
	registry, err := harnessRegistry(cfg.OrgID, manifest, backend, uiOrigin)
	if err != nil {
		log.Fatal(err)
	}
	key, err := plugins.LoadSigningKey(signingKey)
	if err != nil {
		log.Fatal(err)
	}
	gateway := &plugins.Gateway{Registry: registry, SigningKey: key, Issuer: issuer}
	handler, err := newHandler(cfg, gateway)
	if err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	server := &http.Server{Addr: cfg.ListenAddress, Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	errCh := make(chan error, 1)
	go func() { errCh <- server.ListenAndServe() }()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			log.Fatal(err)
		}
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	}
}

func readManifest(path string) ([]byte, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("the plugin manifest requires an absolute path")
	}
	file, err := os.OpenInRoot(filepath.Dir(path), filepath.Base(path))
	if err != nil {
		return nil, fmt.Errorf("open plugin manifest: %w", err)
	}
	defer func() { _ = file.Close() }()
	return io.ReadAll(io.LimitReader(file, plugins.MaxManifestBytes+1))
}

// harnessRegistry serves one plugin, built from its registration manifest,
// as available to the single harness organization. The harness serves no
// plugin service routes, so a package with service grants gets synthetic
// service identities that never authenticate anywhere. A licensed plugin has
// no verifier here and fails closed.
func harnessRegistry(org string, raw []byte, backend, uiOrigin string) (*plugins.InstallationRegistry, error) {
	manifest, err := plugins.DecodeManifest(raw)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(raw)
	item := plugins.Installation{
		InstallationID: uuid.NewString(), ManifestSHA256: hex.EncodeToString(digest[:]), Entitlement: manifest.Entitlement,
		ID: manifest.ID, DisplayName: manifest.DisplayName, Version: manifest.Version, Backend: backend,
		Organizations: map[string]plugins.AvailabilityState{org: plugins.StateAvailable},
		Capabilities:  manifest.Capabilities, Operations: manifest.Operations, ServiceGrants: manifest.ServiceGrants, AuthorizationPolicies: manifest.AuthorizationPolicies,
	}
	if manifest.UI != nil {
		ui := *manifest.UI
		ui.Origin = uiOrigin
		item.UI = &ui
	}
	if len(manifest.ServiceGrants) > 0 {
		item.ServiceUserID, item.AdmissionCheckerID = userid.NewServiceUserTokenId().String(), userid.NewServiceUserTokenId().String()
	}
	return plugins.NewInstallationRegistry([]plugins.Installation{item}, true)
}

func validateConfig(cfg config) error {
	host, _, err := net.SplitHostPort(cfg.ListenAddress)
	if err != nil {
		return fmt.Errorf("listen address must include an explicit loopback host and port: %w", err)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("protocol harness must bind an explicit loopback IP")
	}
	origin, err := url.Parse(cfg.BrowserOrigin)
	if err != nil || origin.Scheme == "" || origin.Host == "" || origin.User != nil ||
		origin.RawQuery != "" || origin.Fragment != "" || (origin.Path != "" && origin.Path != "/") {
		return fmt.Errorf("browser origin must be one exact origin without credentials, path, query or fragment")
	}
	if cfg.OrgID == "" || strings.ContainsAny(cfg.OrgID, "/\\") {
		return fmt.Errorf("one valid harness organization is required")
	}
	if _, err := uuid.Parse(cfg.ActorID); err != nil {
		return fmt.Errorf("one valid harness actor UUID is required")
	}
	return nil
}

func newHandler(cfg config, gateway *plugins.Gateway) (http.Handler, error) {
	if err := validateConfig(cfg); err != nil {
		return nil, err
	}
	if err := gateway.Validate(); err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /orgs/{orgId}/plugins", func(writer http.ResponseWriter, request *http.Request) {
		if !authorizeHarnessRequest(writer, request, cfg) {
			return
		}
		items := make([]map[string]any, 0)
		for _, descriptor := range gateway.Registry.List(request.Context(), cfg.OrgID) {
			installation, _, _ := gateway.Registry.Installation(request.Context(), cfg.OrgID, descriptor.ID)
			operations := make([]map[string]any, 0)
			for _, operation := range installation.Operations {
				if descriptor.Mode == plugins.ModeFull ||
					(descriptor.Mode == plugins.ModeSafetyOnly && (!operation.Mutating || operation.Safety || len(operation.SafetyPathValues) > 0)) {
					operations = append(operations, map[string]any{"id": operation.ID, "method": operation.Method, "mutating": operation.Mutating})
				}
			}
			item := map[string]any{"id": descriptor.ID, "display_name": descriptor.DisplayName, "version": descriptor.Version, "state": descriptor.State, "mode": descriptor.Mode, "operations": operations}
			if descriptor.Mode != plugins.ModeHidden && installation.UI != nil {
				item["ui"] = installation.UI
			}
			items = append(items, item)
		}
		writeJSON(writer, http.StatusOK, map[string]any{"items": items})
	})
	mux.HandleFunc("POST /orgs/{orgId}/plugins/{pluginId}/invoke", func(writer http.ResponseWriter, request *http.Request) {
		if !authorizeHarnessRequest(writer, request, cfg) {
			return
		}
		decoder := json.NewDecoder(io.LimitReader(request.Body, maxBody+1))
		decoder.DisallowUnknownFields()
		var invocation plugins.Invocation
		if err := decoder.Decode(&invocation); err != nil {
			writeJSON(writer, http.StatusBadRequest, map[string]any{responseCode: "invalid_invocation", responseMessage: "one bounded invocation document is required"})
			return
		}
		if err := decoder.Decode(new(any)); err != io.EOF {
			writeJSON(writer, http.StatusBadRequest, map[string]any{responseCode: "invalid_invocation", responseMessage: "one invocation document is required"})
			return
		}
		result, err := gateway.Invoke(request.Context(), cfg.OrgID, request.PathValue("pluginId"), cfg.ActorID, invocation)
		if err != nil {
			var gatewayError *plugins.GatewayError
			if errors.As(err, &gatewayError) {
				writeJSON(writer, gatewayError.Status, map[string]any{responseCode: gatewayError.Code, responseMessage: gatewayError.Message})
				return
			}
			writeJSON(writer, http.StatusInternalServerError, map[string]any{responseCode: "harness_error", responseMessage: "protocol invocation failed"})
			return
		}
		writeJSON(writer, http.StatusOK, invocationResponse{Status: result.Status, Body: result.Body})
	})
	return exactCORS(cfg.BrowserOrigin, mux), nil
}

func authorizeHarnessRequest(writer http.ResponseWriter, request *http.Request, cfg config) bool {
	if request.PathValue("orgId") != cfg.OrgID {
		writeJSON(writer, http.StatusForbidden, map[string]any{responseCode: "harness_org_denied"})
		return false
	}
	if request.Header.Get(actorHeader) != cfg.ActorID {
		writeJSON(writer, http.StatusUnauthorized, map[string]any{responseCode: "harness_actor_required"})
		return false
	}
	return true
}

func exactCORS(origin string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requestOrigin := request.Header.Get("Origin")
		if requestOrigin != "" && requestOrigin != origin {
			writeJSON(writer, http.StatusForbidden, map[string]any{responseCode: "harness_origin_denied"})
			return
		}
		if requestOrigin == origin {
			writer.Header().Set("Access-Control-Allow-Origin", origin)
			writer.Header().Set("Access-Control-Allow-Credentials", "true")
			writer.Header().Set("Vary", "Origin")
		}
		if request.Method == http.MethodOptions {
			writer.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			writer.Header().Set("Access-Control-Allow-Headers", "Content-Type, "+actorHeader)
			writer.Header().Set("Access-Control-Max-Age", "300")
			writer.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(writer, request)
	})
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}
