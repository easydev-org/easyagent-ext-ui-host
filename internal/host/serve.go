package host

import (
	"encoding/json"
	"net/http"
	"path"
	"strings"

	"github.com/easydev-org/easyagent-ext-ui-host/internal/store"
)

/**
 * Mounts the panel surface: /ext/<id>/ for the bundle and /ext/<id>/api/v1/ui for discovery.
 *
 * ONE MOUNT FOR BOTH, because they answer the same question from the console's side — "is there a panel
 * here, and where is it". The console probes `/ext/<id>/api/v1/ui` and, if it answers, opens
 * `/ext/<id>/` in a frame.
 *
 * THE `/api/` PREFIX IS NOT SERVED HERE. The extension's own API lives at `/ext/<id>/api/…` too, and it
 * belongs to the extension's backend — so whatever routes /ext/<id>/api/ to this service must route it
 * to the extension instead. This handler answers exactly ONE path under it (`v1/ui`) and 404s the rest,
 * which turns a misrouted call into a clear 404 rather than an empty panel.
 */
func Mount(mux *http.ServeMux, st *store.Store, base string) {
	prefix := strings.TrimSuffix(base, "/") // "/ext"

	mux.HandleFunc(prefix+"/", func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, prefix+"/")
		extID, tail, _ := strings.Cut(rest, "/")
		if extID == "" {
			http.NotFound(w, r)
			return
		}
		if err := store.ValidID(extID); err != nil {
			logBadID(w, r, extID)
			return
		}

		// The discovery answer. It is mounted HERE rather than left to the extension because the
		// extension no longer has a UI: it cannot say what it does not ship.
		if tail == "api/v1/ui" {
			serveInfo(w, st, extID)
			return
		}
		if strings.HasPrefix(tail, "api/") {
			// Somebody routed an extension API call here. Say so: this service answers one path under
			// /api, and silence would look like the extension is down.
			http.Error(w, "this path belongs to the extension's backend, not to its panel", http.StatusNotFound)
			return
		}
		serveBundle(w, r, st, extID, tail)
	})
}

/**
 * serveInfo answers the console's probe.
 *
 * IT ANSWERS 404 WHEN NOTHING IS ACTIVE, not an empty 200. The console treats a non-200 as "no UI" and
 * leaves the extension out of the shelf; an empty 200 would put a card there that opens a frame with
 * nothing in it.
 */
func serveInfo(w http.ResponseWriter, st *store.Store, extID string) {
	active := st.Active(extID)
	if active == "" {
		http.NotFound(w, nil)
		return
	}
	info, err := st.ReadInfo(extID, active)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// THE SHAPE THE CONSOLE READS, which is what an extension used to answer: base, id, title, and
	// optionally descriptions and icon. `base` is derived here because the mount is this service's to
	// know — a panel that declared its own base could declare one this server does not serve.
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
 * panel's own router owns those paths (it routes on the hash, but a deep link may still arrive as a
 * path). A path that DOES look like a missing asset gets a 404 instead — answering a .js request with
 * HTML is how a bundling mistake becomes "the page loads but nothing works".
 */
func serveBundle(w http.ResponseWriter, r *http.Request, st *store.Store, extID, tail string) {
	root, err := st.BundleDir(extID)
	if err != nil {
		// Nothing active: say so plainly. A 404 from a file server here would be indistinguishable
		// from a missing asset in a panel that IS installed.
		http.Error(w, "no panel is active for "+extID, http.StatusNotFound)
		return
	}
	rel := strings.TrimPrefix(path.Clean("/"+tail), "/")
	if rel == "" {
		rel = "index.html"
	}
	full := root + "/" + rel
	fs := http.Dir(root)
	f, err := fs.Open("/" + rel)
	if err != nil {
		if wantsAsset(tail) {
			http.NotFound(w, r)
			return
		}
		rel = "index.html"
		full = root + "/" + rel
		if f2, err2 := fs.Open("/" + rel); err2 == nil {
			f = f2
		} else {
			http.NotFound(w, r)
			return
		}
	}
	defer func() { _ = f.Close() }()
	st2, _ := f.Stat()
	if st2 != nil && st2.IsDir() {
		// A directory has no index of its own here; the app's entry point is what a reader wants.
		http.Redirect(w, r, r.URL.Path+"/../", http.StatusFound)
		return
	}
	_ = full
	http.ServeContent(w, r, st2.Name(), st2.ModTime(), f)
}

/**
 * wantsAsset reports whether a path is asking for a FILE rather than a route.
 *
 * The extension decides which of the two a miss becomes: index.html for a route (the panel's router
 * handles it), 404 for a file (there is no substitute for a .js the browser asked for).
 */
func wantsAsset(p string) bool {
	ext := path.Ext(p)
	switch ext {
	case ".js", ".mjs", ".css", ".map", ".json", ".svg", ".png", ".jpg", ".jpeg", ".gif", ".webp",
		".ico", ".woff", ".woff2", ".ttf", ".otf", ".wasm", ".txt", ".webmanifest":
		return true
	}
	return false
}

// logBadID answers an id that cannot be a directory name.
func logBadID(w http.ResponseWriter, r *http.Request, id string) {
	http.Error(w, "invalid extension id: "+id, http.StatusBadRequest)
}
