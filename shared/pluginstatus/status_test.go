package pluginstatus

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

var valid = Report{
	InstallationID:      "0199a0c1-1d6e-72f1-8000-0000000000aa",
	PackageSHA256:       strings.Repeat("ab", 32),
	MigrationGeneration: 3,
}

func serve(t *testing.T, report func(context.Context) (Report, error)) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle(Path, Handler(report))
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func TestHandlerServesWhatDecodeAccepts(t *testing.T) {
	generation := int64(2)
	server := serve(t, func(context.Context) (Report, error) {
		generation++
		current := valid
		current.MigrationGeneration = generation
		return current, nil
	})
	for _, want := range []int64{3, 4} {
		response, err := http.Get(server.URL + Path)
		if err != nil {
			t.Fatal(err)
		}
		got, err := Decode(response.Body)
		_ = response.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != http.StatusOK || response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("Content-Type") != "application/json" {
			t.Fatalf("unexpected response %d %v", response.StatusCode, response.Header)
		}
		want := Report{InstallationID: valid.InstallationID, PackageSHA256: valid.PackageSHA256, MigrationGeneration: want}
		if got != want {
			t.Fatalf("got %+v, want %+v: the generation is read on every request", got, want)
		}
	}
}

func TestHandlerNeverServesAnInvalidReport(t *testing.T) {
	broken := valid
	broken.InstallationID = ""
	for name, report := range map[string]func(context.Context) (Report, error){
		"error":   func(context.Context) (Report, error) { return Report{}, errors.New("database unavailable") },
		"invalid": func(context.Context) (Report, error) { return broken, nil },
	} {
		t.Run(name, func(t *testing.T) {
			response, err := http.Get(serve(t, report).URL + Path)
			if err != nil {
				t.Fatal(err)
			}
			_ = response.Body.Close()
			if response.StatusCode != http.StatusServiceUnavailable {
				t.Fatalf("status %d", response.StatusCode)
			}
		})
	}
	response, err := http.Post(serve(t, func(context.Context) (Report, error) { return valid, nil }).URL+Path, "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusMethodNotAllowed || response.Header.Get("Allow") != "GET, HEAD" {
		t.Fatalf("status %d", response.StatusCode)
	}
}

func TestDecodeRequiresTheCompleteContract(t *testing.T) {
	digest := strings.Repeat("ab", 32)
	for name, body := range map[string]string{
		"missing generation":   `{"installation_id":"0199a0c1-1d6e-72f1-8000-0000000000aa","package_sha256":"` + digest + `"}`,
		"missing installation": `{"package_sha256":"` + digest + `","migration_generation":0}`,
		"uppercase uuid":       `{"installation_id":"0199A0C1-1D6E-72F1-8000-0000000000AA","package_sha256":"` + digest + `","migration_generation":0}`,
		"short digest":         `{"installation_id":"0199a0c1-1d6e-72f1-8000-0000000000aa","package_sha256":"abcd","migration_generation":0}`,
		"negative generation":  `{"installation_id":"0199a0c1-1d6e-72f1-8000-0000000000aa","package_sha256":"` + digest + `","migration_generation":-1}`,
		"fractional":           `{"installation_id":"0199a0c1-1d6e-72f1-8000-0000000000aa","package_sha256":"` + digest + `","migration_generation":1.5}`,
		"two documents":        `{"installation_id":"0199a0c1-1d6e-72f1-8000-0000000000aa","package_sha256":"` + digest + `","migration_generation":0}{}`,
		"oversized":            `{"padding":"` + strings.Repeat("x", MaxBytes) + `"}`,
		"not json":             `ready`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Decode(strings.NewReader(body)); !errors.Is(err, ErrInvalid) {
				t.Fatalf("accepted or misclassified: %v", err)
			}
		})
	}
	got, err := Decode(strings.NewReader(`{"installation_id":"0199a0c1-1d6e-72f1-8000-0000000000aa","package_sha256":"` + digest + `","migration_generation":0,"decommission_ready":true}`))
	if err != nil || got.MigrationGeneration != 0 {
		t.Fatalf("a later additive field must be ignored: %+v %v", got, err)
	}
}
