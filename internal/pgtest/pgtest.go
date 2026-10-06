package pgtest

import (
	"net/url"
	"strings"
)

// WithSearchPath returns a copy of dsn with search_path set to schema. It
// understands both URL-form DSNs (postgres://...) and keyword=value DSNs.
// An existing search_path value is replaced in either form.
//
// lib/pq rejects a URL-form DSN when search_path is appended as a bare
// keyword=value pair because the query-string parser reads
// "?sslmode=disable search_path=foo" as sslmode = "disable search_path=foo".
// This helper uses net/url for URL-form DSNs so the result is always valid.
func WithSearchPath(dsn, schema string) string {
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err == nil {
			q := u.Query()
			q.Set("search_path", schema)
			u.RawQuery = q.Encode()
			return u.String()
		}
	}
	// keyword=value form: walk tokens and replace an existing search_path
	// token, or append one if absent.
	parts := strings.Fields(dsn)
	out := make([]string, 0, len(parts)+1)
	replaced := false
	for _, p := range parts {
		if strings.HasPrefix(p, "search_path=") {
			out = append(out, "search_path="+schema)
			replaced = true
		} else {
			out = append(out, p)
		}
	}
	if !replaced {
		out = append(out, "search_path="+schema)
	}
	return strings.Join(out, " ")
}
