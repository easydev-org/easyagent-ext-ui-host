package host

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/easydev-org/easyagent-ext-ui-host/internal/store"
)

// consoleFixture writes a minimal console bundle.
func consoleFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<!doctype html><html>CONSOLE</html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// routing builds the mux the way main does.
func routing(t *testing.T, agentURL string) (*http.ServeMux, *store.Store) {
	t.Helper()
	st := &store.Store{Root: t.TempDir()}
	panels := &Panels{Store: st}
	app := &App{Dir: consoleFixture(t), AgentURL: agentURL, ExtSuffix: ".test.local"}
	mux := http.NewServeMux()
	app.Mount(mux, panels)
	return mux, st
}

func do(mux *http.ServeMux, method, path string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	return w
}

/**
 * THE ROUTES THAT MUST NOT BE THE SPA.
 *
 * Each of these, answered with index.html, produces a failure a browser reports without naming a URL:
 * a JSON parse error the console logs as "unexpected token <". The test asserts the SHAPE of the
 * answer, not just the status, because index.html is also a 200.
 */
func TestAgentRPCIsProxiedNotServedAsTheApp(t *testing.T) {
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"proxied":true,"path":"` + r.URL.Path + `"}`))
	}))
	defer agent.Close()

	mux, _ := routing(t, agent.Listener.Addr().String())
	w := do(mux, http.MethodPost, "/agent.v1.AgentService/ListSessions")
	// DIAGNOSTIC: the mux may have rewritten the path, which would explain a route that looks correct
	// in the source and matches nothing at runtime.
	{
		r := httptest.NewRequest(http.MethodPost, "/agent.v1.AgentService/ListSessions", nil)
		t.Logf("DEBUG path=%q isPrefix=%v", r.URL.Path,
			len(r.URL.Path) >= 9 && r.URL.Path[:9] == "/agent.v1")
	}

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 from the agent", w.Code)
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		// THIS IS THE FAILURE THE ROUTE EXISTED TO PREVENT: the console answering its own RPC with
		// HTML, which shows up as a parse error naming no URL.
		t.Fatalf("the agent's answer was not JSON — the request fell through to the console: %s", w.Body.String())
	}
	if got["proxied"] != true {
		t.Fatalf("the request did not reach the agent: %+v", got)
	}
	if got["path"] != "/agent.v1.AgentService/ListSessions" {
		t.Fatalf("the path was rewritten: %v", got["path"])
	}
}

func TestConsoleIsServedWithSPAFallback(t *testing.T) {
	mux, _ := routing(t, "")
	for _, p := range []string{"/", "/index.html", "/some/deep/route"} {
		w := do(mux, http.MethodGet, p)
		if w.Code != http.StatusOK {
			t.Fatalf("%s = %d, want 200", p, w.Code)
		}
		if want := "CONSOLE"; !strings.Contains(w.Body.String(), want) {
			t.Fatalf("%s did not serve the console: %s", p, w.Body.String())
		}
	}
	// A MISSING ASSET IS NOT THE APP. Answering a .js request with HTML is how a bundling mistake
	// becomes "the page loads but nothing works".
	w := do(mux, http.MethodGet, "/assets/missing.js")
	if w.Code != http.StatusNotFound {
		t.Fatalf("a missing asset = %d, want 404", w.Code)
	}
}

func TestExtensionAPIIsProxiedToItsOwnService(t *testing.T) {
	// The suffix points at a host that does not resolve, so the answer is a 502 — what matters is
	// that the request LEFT this process rather than being answered by the panel or the console.
	mux, _ := routing(t, "")
	w := do(mux, http.MethodGet, "/ext/gitdev/api/v1/whoami")
	if w.Code == http.StatusOK {
		t.Fatalf("an extension API call was answered locally: %s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), "CONSOLE") {
		t.Fatal("an extension API call fell through to the console")
	}
}

func TestPanelsWithoutAnInstallSaySo(t *testing.T) {
	mux, _ := routing(t, "")
	// The discovery probe: 404 so the console leaves the extension out of its shelf, rather than a
	// 200 that would put a card there opening a frame with nothing in it.
	if w := do(mux, http.MethodGet, "/ext/gitdev/api/v1/ui"); w.Code != http.StatusNotFound {
		t.Fatalf("discovery with nothing installed = %d, want 404", w.Code)
	}
	w := do(mux, http.MethodGet, "/ext/gitdev/")
	if w.Code != http.StatusNotFound {
		t.Fatalf("a panel with nothing installed = %d, want 404", w.Code)
	}
	if strings.Contains(w.Body.String(), "CONSOLE") {
		t.Fatal("a panel request fell through to the console")
	}
}
