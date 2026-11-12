// ext-ui-host installs extension panels and serves them.
//
// ONE PROCESS FOR BOTH because they are one job. A panel that is installed but not discovered is
// invisible in the console; a panel that is discovered but not installed is a card that opens nothing.
// Two services would mean two places to look when a panel does not appear.
//
// WHERE IT SITS: the console's ingress sends /ext/<id>/ here and /ext/<id>/api/ to the extension's own
// backend. That split is what makes this work at all — the panel is SAME-ORIGIN with the console, so it
// reads the bearer from localStorage exactly as the console does, and the extension's API keeps its own
// authentication unchanged.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"

	uiv1 "github.com/easydev-org/easyagent-ext-ui-host/gen/ui/v1"
	"github.com/easydev-org/easyagent-ext-ui-host/internal/host"
	"github.com/easydev-org/easyagent-ext-ui-host/internal/store"
)

func main() {
	log := slog.Default().With("svc", "ext-ui-host")

	dataDir := envOr("UI_DATA_DIR", "/data")
	port := envOr("PORT", "8080")
	// The mount this service owns. The ingress must send this prefix here and `<prefix>/<id>/api/` to
	// the extension instead; keeping it one constant means the two rules cannot drift apart.
	base := envOr("UI_BASE", "/ext")
	ghToken := os.Getenv("GITHUB_TOKEN")
	// WHERE THE CONSOLE'S OWN RPC GOES. This pod serves the console, so it also has to forward the
	// console's API — otherwise the page loads and every call 404s.
	agentURL := os.Getenv("AGENT_UPSTREAM")
	// The DNS suffix an extension Service lives under. In-cluster default; override for a laptop.
	extSuffix := envOr("EXT_SVC_SUFFIX", ".dev.svc.cluster.local")
	// The console bundle, baked into the image at build time.
	consoleDir := envOr("CONSOLE_DIR", "/srv")

	if _, err := os.Stat(dataDir); err != nil {
		// A missing data directory is fatal, not a directory to create on demand: it means the volume
		// is not mounted, and creating an empty one would make every panel vanish while the process
		// reports itself healthy.
		log.Error("UI_DATA_DIR does not exist — is the volume mounted?", "dir", dataDir, "err", err)
		os.Exit(1)
	}

	st := &store.Store{Root: dataDir}
	inst := &store.Installer{
		Store:  st,
		Source: &store.GitHubReleases{Token: ghToken},
	}
	// WHAT THIS DEPLOYMENT WANTS KEPT UP TO DATE.
	//
	// A JSON list of {extId, repo}, e.g.
	//   PANEL_REPOS='[{"extId":"gitdev","repo":"easydev-org/easyagent-gitdev-ext-ui"}]'
	//
	// IT EXISTS SO AN EMPTY `Sync` CALL IS MEANINGFUL. Without it, "update everything" requires the
	// caller to know every repository — which duplicates knowledge that the deployment already has, and
	// drifts the moment a panel is added.
	//
	// A MALFORMED LIST IS FATAL, unlike a malformed contribution elsewhere. This value decides what
	// gets installed and executed in the console's origin; starting with it half-understood would mean
	// a deployment that believes it is managing panels it is not.
	configured, err := parsePanelRepos(os.Getenv("PANEL_REPOS"))
	if err != nil {
		log.Error("PANEL_REPOS is not valid JSON", "err", err)
		os.Exit(1)
	}
	for _, p := range configured {
		if err := store.ValidID(p.GetExtId()); err != nil {
			log.Error("PANEL_REPOS has an invalid extId", "extId", p.GetExtId(), "err", err)
			os.Exit(1)
		}
	}
	if len(configured) > 0 {
		log.Info("panels configured for Sync", "count", len(configured))
	}

	svc := &host.Service{Store: st, Installer: inst, ConfiguredPanels: configured}
	panels := &host.Panels{Store: st}

	if _, err := os.Stat(consoleDir + "/index.html"); err != nil {
		// THE CONSOLE IS THE POINT OF THIS POD, so a missing bundle is fatal. Starting anyway would
		// serve panels at /ext/ and a 404 at /, which looks like a routing bug rather than a build
		// that did not copy the files.
		log.Error("the console bundle is missing", "dir", consoleDir, "err", err)
		os.Exit(1)
	}
	if agentURL == "" {
		log.Warn("AGENT_UPSTREAM is not set: the console will load but every API call will 404")
	}

	mux := http.NewServeMux()

	// THE ADMIN API, under /admin. Separate from /ext so a single ingress rule cannot accidentally
	// expose it: installing a panel writes files that the console then executes, so who can reach this
	// is a decision, not a default.
	mountAdmin(mux, svc, os.Getenv("ADMIN_TOKEN"), log)

	// THE PANELS.
	app := &host.App{Dir: consoleDir, AgentURL: agentURL, ExtSuffix: extSuffix}
	app.Mount(mux, panels)

	_ = base // the mount is /ext by construction; kept for the log line below

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	srv := &http.Server{
		Addr:    ":" + port,
		Handler: mux,
		// A panel bundle is static files; the only slow request is an install, and that one is bounded
		// by the client's own timeout.
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		log.Info("listening", "port", port, "base", base, "data", dataDir,
			"console", consoleDir, "agent", agentURL, "extSuffix", extSuffix)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("serve", "err", err)
			stop()
		}
	}()
	<-ctx.Done()
	shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutCtx)
}

// mountAdmin registers the admin RPCs under /admin.
func mountAdmin(mux *http.ServeMux, svc *host.Service, token string, log *slog.Logger) {
	if token == "" {
		// SAID OUT LOUD, not left to be discovered. Installing a panel writes the files the console
		// then executes at its own origin, so an open install API is a real capability; a deployment
		// that wants that should have been told it has it.
		log.Warn("ADMIN_TOKEN is not set: anyone who can reach /admin can install a panel")
	}
	admin := http.NewServeMux()
	connecthttp.Mount(admin, svc.NewServer(tokenAuth(token)))
	mux.Handle("/admin/", http.StripPrefix("/admin", admin))
}

/**
 * tokenAuth requires a bearer when a token is configured, and does nothing when it is not.
 *
 * connect v2 expresses this as a ServerInterceptor — it wraps the whole service, so a method added
 * later cannot skip it. That is the same reason the extension's UI API moved to an interceptor: a
 * per-method check is a check somebody can forget to write.
 */
func tokenAuth(token string) connect.ServerInterceptor {
	return func(next connect.ServerFunc) connect.ServerFunc {
		return func(ctx context.Context, spec connect.Spec, stream connect.ServerStream) error {
			if token == "" {
				return next(ctx, spec, stream)
			}
			info, ok := connect.CallInfoForServerContext(ctx)
			if !ok {
				return connect.NewError(connect.CodeInternal, "no call info on the request context")
			}
			v := strings.TrimSpace(strings.TrimPrefix(info.RequestHeader().Get("Authorization"), "Bearer "))
			if v != token {
				return connect.NewError(connect.CodeUnauthenticated, "a valid admin token is required")
			}
			return next(ctx, spec, stream)
		}
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

/**
 * parsePanelRepos reads PANEL_REPOS into the proto shape.
 *
 * ACCEPTS TWO SPELLINGS of the same list, because the field is written into a YAML manifest by hand
 * and a wrong key would otherwise be a silent no-op:
 *
 *   [{"extId":"gitdev","repo":"o/n"}]      camelCase, like the JSON the API uses
 *   [{"ext_id":"gitdev","repo":"o/n"}]     snake_case, like the proto field
 *
 * An empty value is an empty list — a host that was not told which panels to manage still works, and
 * `Sync` says so rather than doing nothing.
 */
func parsePanelRepos(raw string) ([]*uiv1.PanelRepo, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var rows []struct {
		ExtID      string `json:"extId"`
		ExtIDSnake string `json:"ext_id"`
		Repo       string `json:"repo"`
	}
	if err := json.Unmarshal([]byte(raw), &rows); err != nil {
		return nil, err
	}
	out := make([]*uiv1.PanelRepo, 0, len(rows))
	for _, r := range rows {
		id := r.ExtID
		if id == "" {
			id = r.ExtIDSnake
		}
		if id == "" || r.Repo == "" {
			// SKIPPED RATHER THAN REJECTED as a whole: one bad row should not discard the others, and
			// the caller sees the count in the log line below. A row with no repo could never install
			// anything, so keeping it would only fail later with a worse message.
			continue
		}
		out = append(out, &uiv1.PanelRepo{ExtId: id, Repo: r.Repo})
	}
	return out, nil
}
