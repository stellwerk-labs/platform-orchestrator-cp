// Package pluginstatus is the v1alpha status contract between a running plugin
// and Core. The plugin serves its Report at Path. Core reads it when an
// operator activates or upgrades the installation and serves the package's
// registrations only when the plugin reports exactly the installation UUID,
// package digest and migration generation Core recorded.
//
//	plugin runtime ──verified digest, installation UUID──▶ plugin process
//	plugin process ──applied migration generation──────▶ Report
//	Core ──GET <backend origin>/.well-known/stellwerk/plugin-status──▶ Report
//	Core: Report == recorded installation? ──yes──▶ registrations served
package pluginstatus

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
)

// Path is where a plugin backend serves its status. Core reserves the
// /.well-known/ prefix of a plugin backend; plugin operations never use it.
const Path = "/.well-known/stellwerk/plugin-status"

// MaxBytes bounds one encoded report.
const MaxBytes = 16 << 10

// Report is what a running plugin process states about itself.
type Report struct {
	// InstallationID is the installation UUID Core assigned, in canonical
	// lowercase form, as the plugin runtime configured it.
	InstallationID string `json:"installation_id"`
	// PackageSHA256 is the lowercase hex SHA-256 of the package manifest the
	// plugin runtime verified before it started the process.
	PackageSHA256 string `json:"package_sha256"`
	// MigrationGeneration is the plugin-owned schema generation applied in
	// the plugin's database when the report is served, never the generation
	// the code expects. It is 0 while the plugin owns no schema.
	MigrationGeneration int64 `json:"migration_generation"`
}

var (
	canonicalUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	sha256Hex     = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// ErrInvalid marks a report that does not follow this contract.
var ErrInvalid = errors.New("invalid plugin status report")

// Validate checks the form of every field.
func (r Report) Validate() error {
	switch {
	case !canonicalUUID.MatchString(r.InstallationID):
		return fmt.Errorf("%w: installation_id must be a canonical lowercase UUID", ErrInvalid)
	case !sha256Hex.MatchString(r.PackageSHA256):
		return fmt.Errorf("%w: package_sha256 must be a lowercase SHA-256 hex digest", ErrInvalid)
	case r.MigrationGeneration < 0:
		return fmt.Errorf("%w: migration_generation must not be negative", ErrInvalid)
	}
	return nil
}

// Decode reads exactly one bounded report. Every field of this contract is
// required; fields it does not know are ignored, so a plugin built against a
// later additive contract still reports to this Core.
func Decode(r io.Reader) (Report, error) {
	raw, err := io.ReadAll(io.LimitReader(r, MaxBytes+1))
	if err != nil {
		return Report{}, err
	}
	if len(raw) > MaxBytes {
		return Report{}, fmt.Errorf("%w: the report exceeds %d bytes", ErrInvalid, MaxBytes)
	}
	var fields struct {
		InstallationID      *string `json:"installation_id"`
		PackageSHA256       *string `json:"package_sha256"`
		MigrationGeneration *int64  `json:"migration_generation"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(&fields); err != nil {
		return Report{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return Report{}, fmt.Errorf("%w: the report must be one JSON document", ErrInvalid)
	}
	if fields.InstallationID == nil || fields.PackageSHA256 == nil || fields.MigrationGeneration == nil {
		return Report{}, fmt.Errorf("%w: installation_id, package_sha256 and migration_generation are required", ErrInvalid)
	}
	report := Report{InstallationID: *fields.InstallationID, PackageSHA256: *fields.PackageSHA256, MigrationGeneration: *fields.MigrationGeneration}
	return report, report.Validate()
}

// Handler serves the report the plugin computes for every request, so the
// migration generation is read from the database each time Core asks. A
// failing or invalid report is 503 without detail: Core treats it as a
// failed probe and the plugin logs its own cause.
func Handler(report func(context.Context) (Report, error)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		current, err := report(r.Context())
		if err == nil {
			err = current.Validate()
		}
		if err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		encoded, err := json.Marshal(current)
		if err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(encoded)
	})
}
