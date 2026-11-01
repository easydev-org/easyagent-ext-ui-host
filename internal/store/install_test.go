package store

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// makeTar builds a .tar.gz from a name->content map. A nil content means a directory.
func makeTar(t *testing.T, entries map[string]*string) []byte {
	t.Helper()
	var buf strings.Builder
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range entries {
		if content == nil {
			if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
				t.Fatal(err)
			}
			continue
		}
		body := []byte(*content)
		hdr := &tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return []byte(buf.String())
}

func sptr(s string) *string { return &s }

// server serves a fixed tarball at every path.
func server(t *testing.T, body []byte) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	t.Cleanup(ts.Close)
	return ts
}

type fixedSource struct{ rel Release }

func (f fixedSource) Find(context.Context, string, string, string) (Release, error) {
	return f.rel, nil
}

func installerFor(t *testing.T, url string) (*Installer, *Store) {
	t.Helper()
	st := &Store{Root: t.TempDir()}
	return &Installer{
		Store:    st,
		Source:   fixedSource{rel: Release{Tag: "v1.0.0", AssetName: "ui.tar.gz", AssetURL: url}},
		MaxBytes: 1 << 20,
	}, st
}

func TestInstallUnpacksAndRecords(t *testing.T) {
	tarBytes := makeTar(t, map[string]*string{
		"index.html":    sptr("<html></html>"),
		"assets/app.js": sptr("console.log(1)"),
		"ui.json":       sptr(`{"title":"Git","icon":"/icon.svg"}`),
	})
	ts := server(t, tarBytes)
	in, st := installerFor(t, ts.URL)

	meta, info, err := in.Install(context.Background(), "gitdev", "x/y", "latest", "ui.tar.gz", "")
	if err != nil {
		t.Fatal(err)
	}
	if meta.Version != "v1.0.0" || meta.Files != 3 {
		t.Fatalf("unexpected meta: %+v", meta)
	}
	// The digest is recorded so the next install can pin it.
	if len(meta.SHA256) != 64 {
		t.Fatalf("sha256 should be recorded, got %q", meta.SHA256)
	}
	if info["title"] != "Git" {
		t.Fatalf("ui.json should be read back, got %+v", info)
	}
	// INSTALLING DOES NOT ACTIVATE — that is what makes it safe to attempt.
	if st.Active("gitdev") != "" {
		t.Fatal("an install must not change what is serving")
	}
}

func TestInstallRejectsTraversalOutsideTheBundle(t *testing.T) {
	tarBytes := makeTar(t, map[string]*string{
		"index.html":        sptr("<html></html>"),
		"../../../evil.txt": sptr("pwned"),
	})
	ts := server(t, tarBytes)
	in, _ := installerFor(t, ts.URL)

	// AN ARCHIVE IS UNTRUSTED INPUT. An entry that climbs out of the destination is how a "panel"
	// writes to /etc — and the check is on the resolved path, because "a/../b" is fine.
	_, _, err := in.Install(context.Background(), "gitdev", "x/y", "latest", "", "")
	if err == nil {
		t.Fatal("a traversal entry must fail the install")
	}
	if !strings.Contains(err.Error(), "outside the bundle") {
		t.Fatalf("want a traversal error, got %v", err)
	}
}

func TestInstallRejectsABundleWithNoIndex(t *testing.T) {
	tarBytes := makeTar(t, map[string]*string{"assets/app.js": sptr("x")})
	ts := server(t, tarBytes)
	in, st := installerFor(t, ts.URL)

	// AN EMPTY BUNDLE IS AN INSTALL FAILURE, not a successful install of nothing: without this the
	// panel serves 404 for everything and the cause is three layers away.
	_, _, err := in.Install(context.Background(), "gitdev", "x/y", "latest", "", "")
	if err == nil || !strings.Contains(err.Error(), "index.html") {
		t.Fatalf("want an index.html error, got %v", err)
	}
	if _, statErr := os.Stat(st.VersionDir("gitdev", "v1.0.0")); !os.IsNotExist(statErr) {
		t.Fatal("a failed install must leave no version directory behind")
	}
}

func TestInstallRejectsAHashMismatch(t *testing.T) {
	tarBytes := makeTar(t, map[string]*string{"index.html": sptr("<html></html>")})
	ts := server(t, tarBytes)
	in, _ := installerFor(t, ts.URL)

	_, _, err := in.Install(context.Background(), "gitdev", "x/y", "latest", "", strings.Repeat("0", 64))
	if err == nil || !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Fatalf("want a hash mismatch, got %v", err)
	}
}

func TestInstallRefusesWhenAlreadyPresent(t *testing.T) {
	tarBytes := makeTar(t, map[string]*string{"index.html": sptr("<html></html>")})
	ts := server(t, tarBytes)
	in, _ := installerFor(t, ts.URL)

	if _, _, err := in.Install(context.Background(), "gitdev", "x/y", "latest", "", ""); err != nil {
		t.Fatal(err)
	}
	// A second install would have to delete first, and deleting the serving version is refused — so
	// the honest answer is "already installed" rather than a silent overwrite.
	_, _, err := in.Install(context.Background(), "gitdev", "x/y", "latest", "", "")
	if err == nil || !strings.Contains(err.Error(), "already installed") {
		t.Fatalf("want already-installed, got %v", err)
	}
}

func TestInstallRefusesAnInvalidExtensionID(t *testing.T) {
	tarBytes := makeTar(t, map[string]*string{"index.html": sptr("x")})
	ts := server(t, tarBytes)
	in, _ := installerFor(t, ts.URL)
	// THE ID BECOMES A DIRECTORY NAME, so it is an injection point that is refused rather than
	// sanitized into something the caller did not ask for.
	_, _, err := in.Install(context.Background(), "../etc", "x/y", "latest", "", "")
	if err == nil {
		t.Fatal("an invalid id must be refused")
	}
}
