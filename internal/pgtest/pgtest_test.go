package pgtest_test

import (
	"testing"

	"github.com/stellwerk-labs/platform-orchestrator-cp/internal/pgtest"
)

func TestWithSearchPath(t *testing.T) {
	tests := []struct {
		name   string
		dsn    string
		schema string
		want   string
	}{
		{
			name:   "keyword=value appends search_path",
			dsn:    "host=127.0.0.1 port=5432 user=u password=p dbname=db sslmode=disable",
			schema: "my_schema",
			want:   "host=127.0.0.1 port=5432 user=u password=p dbname=db sslmode=disable search_path=my_schema",
		},
		{
			name:   "keyword=value replaces existing search_path",
			dsn:    "host=127.0.0.1 port=5432 search_path=old sslmode=disable",
			schema: "new_schema",
			want:   "host=127.0.0.1 port=5432 search_path=new_schema sslmode=disable",
		},
		{ //nolint:gosec // fake credentials in a DSN parsing fixture
			name:   "URL form sets search_path as query parameter",
			dsn:    "postgres://u:p@127.0.0.1:5432/db?sslmode=disable",
			schema: "my_schema",
			// url.Values.Encode sorts alphabetically: search_path before sslmode
			want: "postgres://u:p@127.0.0.1:5432/db?search_path=my_schema&sslmode=disable",
		},
		{ //nolint:gosec // fake credentials in a DSN parsing fixture
			name:   "URL form replaces existing search_path query parameter",
			dsn:    "postgres://u:p@127.0.0.1:5432/db?search_path=old&sslmode=disable",
			schema: "new_schema",
			want:   "postgres://u:p@127.0.0.1:5432/db?search_path=new_schema&sslmode=disable",
		},
		{ //nolint:gosec // fake credentials in a DSN parsing fixture
			name:   "postgresql:// scheme is also handled",
			dsn:    "postgresql://u:p@host/db",
			schema: "s",
			want:   "postgresql://u:p@host/db?search_path=s",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := pgtest.WithSearchPath(tc.dsn, tc.schema)
			if got != tc.want {
				t.Errorf("WithSearchPath(%q, %q)\n  got:  %q\n  want: %q", tc.dsn, tc.schema, got, tc.want)
			}
		})
	}
}
