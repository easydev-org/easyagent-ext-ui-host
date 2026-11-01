package store

import (
	"os"
	"path/filepath"
	"testing"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	return &Store{Root: t.TempDir()}
}

// installFake writes a version directory that looks like a completed install.
func installFake(t *testing.T, s *Store, ext, version, indexHTML string) {
	t.Helper()
	dir := filepath.Join(s.VersionDir(ext, version), "dist")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte(indexHTML), 0o644); err != nil {
		t.Fatal(err)
	}
	meta := `{"version":"` + version + `","repo":"x/y","sha256":"abc","files":1,"bytes":1}`
	if err := os.WriteFile(filepath.Join(s.VersionDir(ext, version), MetaFile), []byte(meta), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestActivateIsAtomicAndReversible(t *testing.T) {
	s := newStore(t)
	installFake(t, s, "gitdev", "v1", "one")
	installFake(t, s, "gitdev", "v2", "two")

	if got := s.Active("gitdev"); got != "" {
		t.Fatalf("nothing should be active yet, got %q", got)
	}
	if err := s.Activate("gitdev", "v1"); err != nil {
		t.Fatal(err)
	}
	if got := s.Active("gitdev"); got != "v1" {
		t.Fatalf("active = %q, want v1", got)
	}
	// A rollback is the same operation, which is the point of keeping the old directory.
	if err := s.Activate("gitdev", "v2"); err != nil {
		t.Fatal(err)
	}
	if got := s.Active("gitdev"); got != "v2" {
		t.Fatalf("active = %q, want v2", got)
	}
	if err := s.Activate("gitdev", "v1"); err != nil {
		t.Fatal(err)
	}
	if got := s.Active("gitdev"); got != "v1" {
		t.Fatalf("rollback failed: active = %q", got)
	}
}

func TestActivateRejectsAnUninstalledVersion(t *testing.T) {
	s := newStore(t)
	installFake(t, s, "gitdev", "v1", "one")
	if err := s.Activate("gitdev", "v9"); err == nil {
		t.Fatal("activating a version that is not installed must fail")
	}
}

func TestRemoveRefusesTheServingVersion(t *testing.T) {
	s := newStore(t)
	installFake(t, s, "gitdev", "v1", "one")
	installFake(t, s, "gitdev", "v2", "two")
	if err := s.Activate("gitdev", "v1"); err != nil {
		t.Fatal(err)
	}
	// THE POINT OF THE REFUSAL: deleting what serves leaves a dangling symlink and every request a
	// 404, which looks like a broken panel rather than a deliberate state.
	if err := s.Remove("gitdev", "v1"); err == nil {
		t.Fatal("removing the serving version must be refused")
	}
	// An inactive one goes.
	if err := s.Remove("gitdev", "v2"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.VersionDir("gitdev", "v2")); !os.IsNotExist(err) {
		t.Fatal("v2 should be gone")
	}
}

func TestSanitizers(t *testing.T) {
	// An id is a DNS label. Anything else is refused rather than mangled: rewriting "Git dev" to
	// "gitdev" would install under a name the caller did not ask for.
	if ValidID("gitdev") != nil {
		t.Fatal("gitdev should be valid")
	}
	for _, bad := range []string{"", "../etc", "Git dev", "a/b", "a.b"} {
		if ValidID(bad) == nil {
			t.Fatalf("%q should be refused as an id", bad)
		}
	}
	// A version keeps semver's dots and dashes, and loses separators and leading dots.
	if SanitizeVersion("v0.1.4") != "v0.1.4" {
		t.Fatal("v0.1.4 should survive")
	}
	if SanitizeVersion("../x") != "x" {
		t.Fatalf("a traversal attempt should reduce to a plain name, got %q", SanitizeVersion("../x"))
	}
	if ValidVersion("../x") == nil {
		t.Fatal("a traversal attempt must be refused, not sanitized into place")
	}
}

func TestInstalledSkipsHalfFinishedDirectories(t *testing.T) {
	s := newStore(t)
	installFake(t, s, "gitdev", "v1", "one")
	// An install that failed before its rename leaves a .tmp directory with no metadata.
	if err := os.MkdirAll(filepath.Join(s.ExtDir("gitdev"), ".tmp-install-abc", "dist"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := s.Installed("gitdev")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Version != "v1" {
		t.Fatalf("want only v1, got %+v", got)
	}
}
