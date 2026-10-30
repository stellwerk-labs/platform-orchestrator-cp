package moduleversions

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

var fullGitCommit = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

// ValidateDraftTestSource requires an executable source selector, rather than a
// descriptive revision label. It does not claim artifact digest verification.
func ValidateDraftTestSource(source string, code, revision, digest *string) error {
	if source == "inline" {
		if code == nil || strings.TrimSpace(*code) == "" {
			return fmt.Errorf("inline Draft tests require stored source code")
		}
		if digest != nil {
			return fmt.Errorf("inline Draft tests must omit artifact_digest")
		}
		return nil
	}
	if !strings.HasPrefix(source, "git::") {
		return fmt.Errorf("draft tests require inline source or an explicit Git URL pinned to a full commit")
	}
	address, err := url.Parse(strings.TrimPrefix(source, "git::"))
	if err != nil || address.Host == "" || (address.Scheme != "https" && address.Scheme != "ssh") || address.Fragment != "" || address.Path == "" {
		return fmt.Errorf("draft test Git source must use a valid HTTPS or SSH URL without a fragment")
	}
	if address.User != nil {
		if _, hasPassword := address.User.Password(); hasPassword {
			return fmt.Errorf("draft test source must not embed a password")
		}
	}
	query, err := url.ParseQuery(address.RawQuery)
	if err != nil || len(query) != 1 || len(query["ref"]) != 1 || !fullGitCommit.MatchString(query.Get("ref")) {
		return fmt.Errorf("draft test Git source requires exactly one ref query selecting a full commit; moving refs and shallow clone options are unsupported")
	}
	if revision == nil || *revision != query.Get("ref") {
		return fmt.Errorf("draft test source_revision must equal the full commit in the Git source URL")
	}
	if code != nil {
		return fmt.Errorf("external Draft tests must omit inline source code")
	}
	if digest != nil {
		return ValidateArtifactDigest(*digest)
	}
	return nil
}
