package host

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	uiv1 "github.com/easydev-org/easyagent-ext-ui-host/gen/ui/v1"
	"github.com/easydev-org/easyagent-ext-ui-host/internal/store"
)

/**
 * Update and Sync.
 *
 * EVERY TEST HERE IS ABOUT A FAILURE THAT WOULD BE SILENT IN PRODUCTION:
 *
 *   * a second Update re-downloading a version that is already serving (waste, and a window where a
 *     half-written directory could be active);
 *   * a dry run touching the disk at all;
 *   * one panel's broken release preventing the others from updating;
 *   * Update reporting `changed` when nothing changed, which a deploy script would read as progress.
 */

// panelTar builds a minimal valid panel bundle.
func panelTar(t *testing.T, uiJSON string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	add := func(name, body string) {
		if err := tw.WriteHeader(&tar.Header{
			Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	add("index.html", "<!doctype html><html>PANEL</html>")
	add("ui.json", uiJSON)
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// updateFixture wires a Service against a release served by a test HTTP server.
func updateFixture(t *testing.T, tag, uiJSON string) (*Service, *httptest.Server) {
	t.Helper()
	body := panelTar(t, uiJSON)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body)
	}))
	t.Cleanup(ts.Close)
	st := &store.Store{Root: t.TempDir()}
	return &Service{
		Store: st,
		Installer: &store.Installer{
			Store:    st,
			Source:   fixedRelease{tag: tag, url: ts.URL},
			MaxBytes: 1 << 20,
		},
	}, ts
}

// fixedRelease resolves any request to one release.
type fixedRelease struct{ tag, url string }

func (f fixedRelease) Find(context.Context, string, string, string) (store.Release, error) {
	return store.Release{Tag: f.tag, AssetName: "ui.tar.gz", AssetURL: f.url}, nil
}

func TestUpdateInstallsAndActivatesInOneCall(t *testing.T) {
	svc, _ := updateFixture(t, "v1.0.0", `{"title":"Git"}`)

	got, err := svc.Update(context.Background(), &uiv1.UpdateRequest{
		ExtId: "gitdev", Repo: "o/n",
	})
	if err != nil {
		t.Fatal(err)
	}
	// THE WHOLE REASON THIS RPC EXISTS: one call, and the panel is serving. Install alone would have
	// left `active` empty, which reads from the console as "the panel is not installed".
	if got.GetChanged() != true {
		t.Fatalf("want changed, got %+v", got)
	}
	if got.GetTo() != "v1.0.0" || got.GetFrom() != "" {
		t.Fatalf("want from='' to=v1.0.0, got from=%q to=%q", got.GetFrom(), got.GetTo())
	}
	if svc.Store.Active("gitdev") != "v1.0.0" {
		t.Fatalf("the version is not serving: %q", svc.Store.Active("gitdev"))
	}
	if got.GetInfo().GetTitle() != "Git" {
		t.Fatalf("metadata was not reported: %+v", got.GetInfo())
	}
	if got.GetSha256() == "" {
		t.Fatal("the digest should be reported so the next update can pin it")
	}
}

func TestUpdateIsANoOpWhenAlreadyCurrent(t *testing.T) {
	svc, _ := updateFixture(t, "v1.0.0", `{"title":"Git"}`)
	if _, err := svc.Update(context.Background(), &uiv1.UpdateRequest{ExtId: "gitdev", Repo: "o/n"}); err != nil {
		t.Fatal(err)
	}

	// THE ORDINARY CASE FOR A CRON JOB. It must not re-download, must not fail, and must not claim to
	// have changed anything — a deploy script reading `changed: true` would report progress every hour.
	got, err := svc.Update(context.Background(), &uiv1.UpdateRequest{ExtId: "gitdev", Repo: "o/n"})
	if err != nil {
		t.Fatal(err)
	}
	if got.GetChanged() {
		t.Fatal("a second Update reported a change it did not make")
	}
	if got.GetFrom() != "v1.0.0" || got.GetTo() != "v1.0.0" {
		t.Fatalf("want both v1.0.0, got %q -> %q", got.GetFrom(), got.GetTo())
	}
}

func TestUpdateDryRunTouchesNothing(t *testing.T) {
	svc, _ := updateFixture(t, "v2.0.0", `{"title":"Git"}`)

	got, err := svc.Update(context.Background(), &uiv1.UpdateRequest{
		ExtId: "gitdev", Repo: "o/n", DryRun: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.GetChanged() {
		t.Fatal("a dry run must not report a change")
	}
	if got.GetTo() != "v2.0.0" {
		t.Fatalf("the dry run should still report what is available, got %q", got.GetTo())
	}
	// THE POINT OF A DRY RUN: answering "is there an update" must not cost a download.
	if svc.Store.Active("gitdev") != "" {
		t.Fatal("a dry run activated a version")
	}
	if metas, _ := svc.Store.Installed("gitdev"); len(metas) != 0 {
		t.Fatalf("a dry run wrote to disk: %+v", metas)
	}
}

func TestUpdateInfersTheRepoFromTheActiveVersion(t *testing.T) {
	svc, _ := updateFixture(t, "v1.0.0", `{"title":"Git"}`)
	if _, err := svc.Update(context.Background(), &uiv1.UpdateRequest{ExtId: "gitdev", Repo: "o/n"}); err != nil {
		t.Fatal(err)
	}

	// A caller that knows only the ext id — a cron job, a deployment that forgot the repository — can
	// still update, because the repo was recorded at install time.
	got, err := svc.Update(context.Background(), &uiv1.UpdateRequest{ExtId: "gitdev"})
	if err != nil {
		t.Fatal(err)
	}
	if got.GetRepo() != "o/n" {
		t.Fatalf("want the recorded repo, got %q", got.GetRepo())
	}
}

func TestUpdateRequiresARepoWhenNothingIsInstalled(t *testing.T) {
	svc, _ := updateFixture(t, "v1.0.0", `{}`)
	_, err := svc.Update(context.Background(), &uiv1.UpdateRequest{ExtId: "gitdev"})
	if err == nil {
		t.Fatal("an update with no repo and nothing installed cannot resolve anything")
	}
}

func TestSyncReportsEachPanelSeparately(t *testing.T) {
	svc, _ := updateFixture(t, "v1.0.0", `{"title":"Git"}`)
	// One entry is valid; the other names a repo the source resolves anyway, so the failure has to come
	// from the ID — which is what a typo in PANEL_REPOS produces.
	resp, err := svc.Sync(context.Background(), &uiv1.SyncRequest{
		Panels: []*uiv1.PanelRepo{
			{ExtId: "gitdev", Repo: "o/n"},
			{ExtId: "../evil", Repo: "o/n"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.GetResults()) != 2 {
		t.Fatalf("want 2 results, got %d", len(resp.GetResults()))
	}
	if resp.GetResults()[0].GetError() != "" {
		t.Fatalf("the valid entry failed: %s", resp.GetResults()[0].GetError())
	}
	// ONE BAD ENTRY MUST NOT STOP THE REST, and must not fail the whole call: a deploy script needs to
	// see which panels succeeded.
	if resp.GetResults()[1].GetError() == "" {
		t.Fatal("the invalid entry reported success")
	}
	if svc.Store.Active("gitdev") != "v1.0.0" {
		t.Fatal("the valid panel was not installed")
	}
}

func TestSyncUsesTheConfiguredListWhenNoneIsGiven(t *testing.T) {
	svc, _ := updateFixture(t, "v1.0.0", `{"title":"Git"}`)
	svc.ConfiguredPanels = []*uiv1.PanelRepo{{ExtId: "gitdev", Repo: "o/n"}}

	// AN EMPTY CALL IS MEANINGFUL when the host was configured: it is how a deployment returns to a
	// known state without repeating its own configuration.
	resp, err := svc.Sync(context.Background(), &uiv1.SyncRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.GetResults()) != 1 || resp.GetResults()[0].GetExtId() != "gitdev" {
		t.Fatalf("want the configured panel, got %+v", resp.GetResults())
	}
}

func TestSyncWithoutAListOrConfigurationSaysSo(t *testing.T) {
	svc, _ := updateFixture(t, "v1.0.0", `{}`)
	// DOING NOTHING SILENTLY WOULD BE WORSE: a host nobody configured reporting "0 results, no error"
	// looks like a successful run of an empty plan.
	if _, err := svc.Sync(context.Background(), &uiv1.SyncRequest{}); err == nil {
		t.Fatal("an unconfigured host should refuse rather than silently do nothing")
	}
}

/**
 * The RPC answer must carry the same contributions the HTTP answer does.
 *
 * TWO VIEWS OF ONE PANEL THAT DISAGREE IS THE BUG THIS PREVENTS: /ext/<id>/api/v1/ui passes the raw
 * ui.json through, so it always had `contributes`; PanelInfo did not map the field, so every RPC answer
 * omitted it. A script reading Install/Update/List got a panel with no session views and no proxies,
 * which is indistinguishable from a panel that declared none.
 */
func TestPanelInfoCarriesContributions(t *testing.T) {
	info := panelInfo(map[string]any{
		"title": "Sandboxes",
		"contributes": map[string]any{
			"session_views": []any{
				map[string]any{
					"id": "sandbox", "route": "/", "title": "Sandbox",
					"menu_label": "Sandbox", "selection_namespace": "sandbox",
				},
				map[string]any{"id": "no-label", "route": "/x"}, // dropped: cannot be opened
			},
			"proxies": []any{"desktop", ""},
			"pages":   []any{map[string]any{"route": "/pulls", "title": "Pulls"}},
		},
	})
	c := info.GetContributes()
	if c == nil {
		t.Fatal("contributions were dropped from the RPC answer")
	}
	if len(c.GetSessionViews()) != 1 || c.GetSessionViews()[0].GetSelectionNamespace() != "sandbox" {
		t.Fatalf("want one session view with its namespace, got %+v", c.GetSessionViews())
	}
	if len(c.GetProxies()) != 1 || c.GetProxies()[0] != "desktop" {
		t.Fatalf("want [desktop], got %v", c.GetProxies())
	}
	if len(c.GetPages()) != 1 || c.GetPages()[0].GetRoute() != "/pulls" {
		t.Fatalf("want one page, got %+v", c.GetPages())
	}
}

func TestPanelInfoAcceptsCamelCaseContributions(t *testing.T) {
	// The wire uses snake_case; a panel author following the TypeScript interface may write camelCase.
	// Both are accepted so a declaration is never silently ignored.
	info := panelInfo(map[string]any{
		"contributes": map[string]any{
			"sessionViews": []any{
				map[string]any{
					"id": "a", "route": "/s", "menuLabel": "A", "selectionNamespace": "ns",
				},
			},
		},
	})
	c := info.GetContributes()
	if len(c.GetSessionViews()) != 1 || c.GetSessionViews()[0].GetMenuLabel() != "A" {
		t.Fatalf("camelCase was not accepted: %+v", c.GetSessionViews())
	}
	if c.GetSessionViews()[0].GetSelectionNamespace() != "ns" {
		t.Fatalf("camelCase namespace was not accepted: %+v", c.GetSessionViews()[0])
	}
}
