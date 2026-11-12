package host

import (
	"encoding/json"
	"net/http"
	"path"
	"strings"

	"github.com/easydev-org/easyagent-ext-ui-host/internal/store"
)

/**
 * Panels serves what this pod owns under /ext/<id>: the bundle, and the discovery answer.
 *
 * ONE HANDLER FOR THE WHOLE SUBTREE, registered once, because three routes under one prefix registered
 * separately would each have to agree about precedence. One handler decides, in order:
 *
 *	/ext/<id>/api/v1/ui   → "here is my panel" (answered from the installed bundle's ui.json)
 *	/ext/<id>/api/…       → NOT here; that is the extension's backend (the proxy in app.go owns it)
 *	/ext/<id>/…           → a file out of the active bundle, or index.html
 *
 * THE DISCOVERY ANSWER IS THE POINT OF THE WHOLE SERVICE. The console asks every registered extension
 * `/ext/<id>/api/v1/ui` and shows a card only for the ones that answer. An extension no longer ships a
 * UI, so it cannot answer for one — this does, from the panel it has installed.
 */
type Panels struct {
	Store *store.Store
}

// MountPanels registers the panel subtree.
func (p *Panels) MountPanels(mux *http.ServeMux) {
	mux.HandleFunc("/ext/", p.Serve)
}

func (p *Panels) Serve(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/ext/")
	extID, tail, _ := strings.Cut(rest, "/")
	if extID == "" {
		http.NotFound(w, r)
		return
	}
	if err := store.ValidID(extID); err != nil {
		http.Error(w, "invalid extension id: "+extID, http.StatusBadRequest)
		return
	}

	if tail == "api/v1/ui" {
		p.serveInfo(w, extID)
		return
	}
	if strings.HasPrefix(tail, "api/") {
		// The proxy in app.go handles this subtree and registers FIRST, so reaching here means it
		// declined — which it only does for a malformed id, already handled above. Answering plainly
		// is better than a silent 404 that looks like the extension is down.
		http.Error(w, "this path belongs to the extension's backend", http.StatusNotFound)
		return
	}
	p.serveBundle(w, r, extID, tail)
}

/**
 * serveInfo answers the console's probe.
 *
 * 404 WHEN NOTHING IS ACTIVE, not an empty 200. The console treats a non-200 as "no UI" and leaves the
 * extension out of its shelf; an empty 200 would put a card there that opens a frame with nothing in it.
 */
func (p *Panels) serveInfo(w http.ResponseWriter, extID string) {
	active := p.Store.Active(extID)
	if active == "" {
		http.NotFound(w, nil)
		return
	}
	info, err := p.Store.ReadInfo(extID, active)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// THE SHAPE THE CONSOLE ALREADY READS, which is what an extension used to answer: base, id, title,
	// and optionally descriptions and icon. `base` is derived HERE because the mount is this service's
	// to know — a panel that declared its own base could declare one this server does not serve.
	body := map[string]any{
		"id":    extID,
		"base":  "/ext/" + extID,
		"title": extID,
	}
	if v, ok := info["title"].(string); ok && v != "" {
		body["title"] = v
	}
	if v, ok := info["descriptions"].(map[string]any); ok && len(v) > 0 {
		clean := map[string]string{}
		for lang, raw := range v {
			// BLANK ENTRIES ARE DROPPED: the console picks a language and falls back to the first
			// non-empty, and an empty string would win that fallback and render nothing.
			if s, ok := raw.(string); ok && s != "" {
				clean[lang] = s
			}
		}
		if len(clean) > 0 {
			body["descriptions"] = clean
		}
	}
	if v, ok := info["icon"].(string); ok && v != "" {
		body["icon"] = v
	}
	// THE CONTRIBUTIONS, AS THE PANEL DECLARED THEM.
	//
	// PASSED THROUGH VERBATIM rather than typed here, because this service is not the authority on
	// what a contribution means — the console is. Re-encoding into the protobuf shape would mean a
	// field added to `ui.json` needs a change HERE before the console can see it, which is the
	// coupling this whole indirection exists to avoid.
	//
	// MALFORMED IS DROPPED, NOT FORWARDED. The console validates again, but a contribution that
	// arrives as a string where an object belongs would make it log a parse failure on every panel
	// probe rather than once at install.
	if v, ok := info["contributes"]; ok {
		if c, ok := v.(map[string]any); ok && len(c) > 0 {
			body["contributes"] = c
		}
	}
	w.Header().Set("Content-Type", "application/json")
	// NOT CACHED: the answer changes when a version is activated, and a console that cached it would
	// keep opening a version that is no longer serving.
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(body)
}

/**
 * serveBundle serves one file out of the active bundle.
 *
 * SPA FALLBACK, like every single-page app: a path with no file behind it gets index.html, because the
 * panel's own router owns those paths. A path that DOES look like a missing asset gets a 404 instead —
 * answering a .js request with HTML is how a bundling mistake becomes "the page loads but nothing
 * works", with nothing in the console log naming the file.
 */
func (p *Panels) serveBundle(w http.ResponseWriter, r *http.Request, extID, tail string) {
	root, err := p.Store.BundleDir(extID)
	if err != nil {
		// Nothing active: say so plainly. A bare 404 here would be indistinguishable from a missing
		// asset inside a panel that IS installed.
		http.Error(w, "no panel is active for "+extID, http.StatusNotFound)
		return
	}
	rel := strings.TrimPrefix(path.Clean("/"+tail), "/")
	if rel == "" {
		rel = "index.html"
	}
	fs := http.Dir(root)
	f, err := fs.Open("/" + rel)
	if err != nil {
		if wantsAsset(tail) {
			http.NotFound(w, r)
			return
		}
		rel = "index.html"
		f2, err2 := fs.Open("/" + rel)
		if err2 != nil {
			http.NotFound(w, r)
			return
		}
		f = f2
	}
	defer func() { _ = f.Close() }()
	st, _ := f.Stat()
	if st == nil {
		http.NotFound(w, r)
		return
	}
	if rel == "index.html" {
		// THE PANEL'S ENTRY POINT MUST NOT BE CACHED: a stale index references asset hashes the new
		// build no longer has, which fails as a blank panel right after an activation.
		w.Header().Set("Cache-Control", "no-cache")
	}
	http.ServeContent(w, r, st.Name(), st.ModTime(), f)
}

/**
 * wantsAsset reports whether a path is asking for a FILE rather than a route.
 *
 * The difference decides what a miss becomes: index.html for a route (the panel's router handles it),
 * 404 for a file (there is no substitute for a .js the browser asked for).
 */
func wantsAsset(p string) bool {
	switch path.Ext(p) {
	case ".js", ".mjs", ".css", ".map", ".json", ".svg", ".png", ".jpg", ".jpeg", ".gif", ".webp",
		".ico", ".woff", ".woff2", ".ttf", ".otf", ".wasm", ".txt", ".webmanifest":
		return true
	}
	return false
}
