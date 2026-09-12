package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NakliTechie/continuum/api"
)

func TestO4CheckerResponseBounds(t *testing.T) {
	for _, c := range []struct {
		op     string
		n      int
		accept bool
	}{{"events", (16 << 20) - 1024, true}, {"events", (16 << 20) + 1, false}, {"status", (1 << 20) - 1024, true}, {"status", (1 << 20) + 1, false}} {
		t.Run(c.op+"/"+map[bool]string{true: "below", false: "above"}[c.accept], func(t *testing.T) {
			dir := t.TempDir()
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				enc := json.NewEncoder(w)
				enc.SetEscapeHTML(false)
				_ = enc.Encode(api.Result("o4", map[string]string{"data": strings.Repeat("x", c.n)}))
			}))
			serveOnSocket(t, server, dir)
			for name, value := range map[string]string{"operator.token": "disposable"} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0600); err != nil {
					t.Fatal(err)
				}
			}
			result := call(context.Background(), dir, false, api.Request{Operation: c.op})
			if c.accept && result.Class != "ok" {
				t.Fatalf("below limit failed %s/%s", result.Class, result.Code)
			}
			if !c.accept && (result.Class != "indeterminate" || result.Code != "invalid_response") {
				t.Fatalf("over-limit response accepted %s/%s", result.Class, result.Code)
			}
		})
	}
}
