package host

import (
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/easydev-org/easyagent-ext-ui-host/internal/store"
)

/**
 * App serves the console itself, out of a directory this pod owns.
 *
 * WHY THE CONSOLE LIVES HERE, in the same process as the panels: it is the only arrangement in which
 * "one pod, one service" is true AND the panels are same-origin with the console. Same-origin is not a
 * preference — the panel reads the bearer from the console's localStorage, and there is no other way to
 * hand it a credential that does not involve putting one in a URL or in a postMessage the panel could
 * be tricked into forwarding.
 *
 * The alternative — a separate console pod — needs something in front to make /ext/* same-origin, and
 * that "something" is exactly the reverse proxy this type replaces.
 */
type App struct {
	// Dir is the console's built bundle.
	Dir string
	// AgentURL is where the console's own RPC goes. Empty disables the proxy, which makes the console
	// load and fail at login rather than 404 every call — the more honest failure.
	AgentURL string
	// ExtSuffix is the DNS suffix an extension Service lives under: "<id>-ext" + suffix.
	//
	// CONVENTION OVER CONFIGURATION, deliberately: shipping a panel must not require editing a routing
	// table. The extension's Service is named after its id, and that is the whole registration.
	ExtSuffix string

	// agentProxy is built by Mount from AgentURL. A field rather than a local because the route it
	// serves is decided in the catch-all — see the note there for why it cannot be a mux pattern.
	agentProxy http.Handler
}

/**
 * Mount adds the console's routes to a mux.
 *
 * ORDER MATTERS AND IT IS THE WHOLE DESIGN:
 *
 *	/agent.v1.*         → the agent        (the console's own RPC)
 *	/ext/<id>/api/*     → <id>-ext         (an extension's API)
 *	/ext/<id>/*         → panels           (this pod, registered separately)
 *	everything else     → the console's files, with an SPA fallback
 *
 * The /ext split is what lets one process be both the console's host and the panels' host while the
 * extensions keep their own backends: the browser sees one origin, and each request goes to whoever
 * actually serves it.
 */
func (a *App) Mount(mux *http.ServeMux, panels *Panels) {
	if a.AgentURL != "" {
		// THE CONSOLE'S CONNECT PATHS ARE `/agent.v1.AgentService/Method`, AND THEY CANNOT BE A
		// SERVE-MUX PATTERN.
		//
		// Go 1.22's ServeMux matches a pattern that does not end in "/" against that EXACT path only.
		// So "/agent.v1." matched nothing, every RPC fell through to the SPA, and the console answered
		// its own API with index.html — which a browser reports as a JSON parse error that names no
		// URL. A prefix pattern needs a trailing "/", and "/agent.v1/" is not a prefix of
		// "/agent.v1.AgentService/…" because the separator here is a DOT.
		//
		// So this one route is decided by a path test inside the catch-all, which is the only place
		// the shape can be expressed. It costs one HasPrefix per request and buys the console working.
		a.agentProxy = a.proxy(a.AgentURL)
	}

	// THE PANEL SUBTREE IS SPLIT BETWEEN TWO HANDLERS, and the order here is the split.
	//
	// `/ext/<id>/api/...` goes to the extension's backend. Everything else under `/ext/<id>/` is the
	// panel, which this pod serves. A ServeMux picks the longest matching pattern, so the API handler
	// wins for API paths without either handler inspecting the other's territory — except for the one
	// path that IS the panel's: `/api/v1/ui`, the discovery answer, which the extension cannot give
	// because it no longer ships a UI.
	mux.HandleFunc("/ext/", func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/ext/")
		extID, tail, _ := strings.Cut(rest, "/")
		if tail == "api/v1/ui" {
			panels.Serve(w, r)
			return
		}
		// What this panel's backend serves besides api/, read from its ui.json on every request.
		//
		// ON EVERY REQUEST rather than cached at install: activating a new version changes the
		// declaration, and a cache would keep forwarding the old extension's prefixes to the new one.
		// It is one small file read on a path the browser already round-trips.
		var proxies []string
		if err := store.ValidID(extID); err == nil && panels.Store != nil {
			proxies = panels.Store.Proxies(extID)
		}
		// THE PREFIXES THE EXTENSION'S OWN BACKEND OWNS, DECLARED BY THE PANEL.
		//
		// `/ext/<id>/desktop/...` is a WebSocket-carrying proxy the sandbox extension serves — the
		// easystream UI. Without forwarding it, the request fell into the panel's SPA fallback, so the
		// iframe loaded THE PANEL'S OWN index.html inside itself: a page that renders, looks plausible,
		// and shows no screen. The signed URL in it was never even read.
		//
		// THIS USED TO BE A HARD-CODED LIST. One extension needing a prefix meant a console release,
		// and the comment explaining it had to be re-read by whoever added the next one. Now each
		// panel declares its prefixes in its own ui.json (`contributes.proxies`), and this router only
		// consults that declaration — so the host stays generic and an extension ships its own route.
		//
		// THE PANEL'S OWN ROUTES ARE STILL THE PANEL'S: a rule that forwarded "everything that is not
		// api/" would send the panel's SPA routes to a backend that has no idea what they are. Hence a
		// declared list rather than a catch-all.
		if !strings.HasPrefix(tail, "api/") && !isProxied(tail, proxies) {
			panels.Serve(w, r)
			return
		}
		// AN EXTENSION'S API, BY CONVENTION: the Service is named after the id. Resolved per request,
		// so an extension that appears later needs no change here.
		if err := store.ValidID(extID); err != nil {
			http.Error(w, "invalid extension id", http.StatusBadRequest)
			return
		}
		a.proxy(extID+"-ext"+a.ExtSuffix+":8080").ServeHTTP(w, r)
	})

	// THE CONSOLE ITSELF, AND THE ONE ROUTE THAT CANNOT BE A PATTERN.
	//
	// Registered LAST so every specific route above wins: a bare "/" handler on a ServeMux already
	// loses to longer patterns, but saying it here is what makes the ordering a decision rather than
	// an accident of pattern length.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// THE AGENT'S RPC, DECIDED HERE FOR THE REASON GIVEN ABOVE: its paths are
		// /agent.v1.<Service>/<Method>, the separator is a DOT, and no ServeMux pattern can express
		// "starts with /agent.v1." without also matching paths that are not it.
		if a.agentProxy != nil && strings.HasPrefix(r.URL.Path, "/agent.v1.") {
			a.agentProxy.ServeHTTP(w, r)
			return
		}
		a.serveApp(w, r)
	})
}

/**
 * proxy builds a reverse proxy for one upstream host.
 *
 * THE HOST HEADER IS REWRITTEN to the upstream's own name. The browser reaches this pod by whatever
 * name it was given; an upstream that routes on Host, or a framework that builds absolute URLs from it,
 * would otherwise see a name it does not recognise.
 */
func (a *App) proxy(host string) http.Handler {
	up := httputil.NewSingleHostReverseProxy(&url.URL{Scheme: "http", Host: host})
	orig := up.Director
	up.Director = func(req *http.Request) {
		orig(req)
		req.Host = host
	}
	return up
}

/**
 * serveApp serves one file of the console, falling back to index.html.
 *
 * A MISSING ASSET IS A 404, NOT THE APP. Answering a .js request with HTML makes the browser fail to
 * parse it, and the page loads looking broken with nothing in the console log that names the file.
 */
func (a *App) serveApp(w http.ResponseWriter, r *http.Request) {
	if a.Dir == "" {
		http.Error(w, "the console is not installed on this host", http.StatusNotFound)
		return
	}
	rel := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	if rel == "" {
		rel = "index.html"
	}
	fs := http.Dir(a.Dir)
	f, err := fs.Open("/" + rel)
	if err != nil {
		if wantsAsset(rel) {
			http.NotFound(w, r)
			return
		}
		rel = "index.html"
		if f2, err2 := fs.Open("/" + rel); err2 == nil {
			f = f2
		} else {
			http.Error(w, "the console bundle has no index.html", http.StatusInternalServerError)
			return
		}
	}
	defer func() { _ = f.Close() }()
	st, _ := f.Stat()
	if st == nil {
		http.NotFound(w, r)
		return
	}
	// THE CONSOLE IS A SINGLE-PAGE APP and its index must not be cached: a stale index references
	// asset hashes the new build no longer has, which fails as a blank page after every deploy.
	if rel == "index.html" {
		w.Header().Set("Cache-Control", "no-cache")
	}
	http.ServeContent(w, r, st.Name(), st.ModTime(), f)
}

// FindConsole locates a console bundle inside a data directory, for a deployment that installs it
// there rather than baking it into the image.
func FindConsole(dataDir string) string {
	p := filepath.Join(dataDir, "console", "current", "dist")
	if _, err := os.Stat(filepath.Join(p, "index.html")); err == nil {
		return p
	}
	return ""
}

/**
 * isProxied reports whether tail belongs to a prefix the panel declared as its backend's.
 *
 * THE BARE PREFIX COUNTS as well as the subtree: the first request a browser makes for a live screen
 * is the bare path, and a rule that only matched `desktop/` would hand that one request to the panel
 * and every later one to the extension — a screen that never connects, with no error anywhere.
 */
func isProxied(tail string, prefixes []string) bool {
	for _, p := range prefixes {
		if tail == p || strings.HasPrefix(tail, p+"/") {
			return true
		}
	}
	return false
}
