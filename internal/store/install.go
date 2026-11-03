package store

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Release is what the installer needs to know about a published panel.
type Release struct {
	// The tag, as GitHub reports it.
	Tag string
	// AssetName is the file inside the release.
	AssetName string
	// AssetURL is where to fetch it.
	AssetURL string
	// Size in bytes, for a bound on the download.
	Size int64
}

// ReleaseSource finds a release. An interface so a test does not need the network, and so a private
// registry could be plugged in without touching the installer.
type ReleaseSource interface {
	Find(ctx context.Context, repo, version, asset string) (Release, error)
}

/**
 * GitHubReleases resolves owner/name + version to a downloadable asset.
 *
 * IT RESOLVES "latest" ONCE, HERE, and returns the concrete tag. A floating "latest" re-resolved on
 * every request would mean the panel a browser gets depends on when they asked — and the recorded
 * version would be a lie.
 */
type GitHubReleases struct {
	// Token is optional: it raises the rate limit and is required for a private repository.
	Token string
	// APIBase defaults to https://api.github.com.
	APIBase string
	Client  *http.Client
}

func (g *GitHubReleases) Find(ctx context.Context, repo, version, asset string) (Release, error) {
	if g.Client == nil {
		g.Client = &http.Client{Timeout: 30 * time.Second}
	}
	base := g.APIBase
	if base == "" {
		base = "https://api.github.com"
	}
	if asset == "" {
		asset = "ui.tar.gz"
	}
	url := fmt.Sprintf("%s/repos/%s/releases/tags/%s", base, repo, version)
	if version == "" || version == "latest" {
		url = fmt.Sprintf("%s/repos/%s/releases/latest", base, repo)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Release{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if g.Token != "" {
		req.Header.Set("Authorization", "Bearer "+g.Token)
	}
	resp, err := g.Client.Do(req)
	if err != nil {
		return Release{}, fmt.Errorf("cannot reach GitHub: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return Release{}, fmt.Errorf("%s has no release %q", repo, version)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return Release{}, fmt.Errorf("GitHub answered %d for %s: %s", resp.StatusCode, url, strings.TrimSpace(string(body)))
	}
	var out struct {
		TagName string `json:"tag_name"`
		Assets  []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
			Size int64  `json:"size"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return Release{}, fmt.Errorf("malformed release: %w", err)
	}
	for _, a := range out.Assets {
		if a.Name == asset {
			return Release{Tag: out.TagName, AssetName: a.Name, AssetURL: a.URL, Size: a.Size}, nil
		}
	}
	// NAMING THE ASSETS THAT ARE THERE: "ui.tar.gz not found" against a release that calls it
	// "bundle.tar.gz" is a message that sends the reader to look at their spelling instead of the
	// release's contents.
	var names []string
	for _, a := range out.Assets {
		names = append(names, a.Name)
	}
	return Release{}, fmt.Errorf("release %s has no asset %q (it has: %s)",
		out.TagName, asset, strings.Join(names, ", "))
}

// Installer downloads and unpacks one panel version.
type Installer struct {
	Store  *Store
	Source ReleaseSource
	Client *http.Client
	// MaxBytes bounds the download. A panel bundle is a few megabytes; a tarball of a gigabyte is not
	// one, and reading it before finding that out costs the disk it would have filled.
	MaxBytes int64
}

// ErrTooLarge is returned when the archive exceeds MaxBytes.
var ErrTooLarge = errors.New("the archive is larger than this host will unpack")

/**
 * Install fetches a release, unpacks it, and records it as an installed version.
 *
 * IT DOES NOT ACTIVATE. See the note on the Install RPC: installing is safe to attempt precisely
 * because it cannot change what a reader sees.
 *
 * THE UNPACK IS STAGED IN A TEMPORARY DIRECTORY inside the extension's own directory, and renamed
 * into place only when everything succeeded — so a download that fails at 90%, an archive that is not
 * a tarball, and a bundle with no index.html all leave the extension exactly as it was.
 */
func (in *Installer) Install(ctx context.Context, extID, repo, version, asset, wantSHA string) (Meta, map[string]any, error) {
	if err := ValidID(extID); err != nil {
		return Meta{}, nil, err
	}
	if in.Client == nil {
		in.Client = &http.Client{Timeout: 5 * time.Minute}
	}
	if in.MaxBytes == 0 {
		in.MaxBytes = 200 << 20 // 200 MiB
	}

	rel, err := in.Source.Find(ctx, repo, version, asset)
	if err != nil {
		return Meta{}, nil, err
	}
	if err := ValidVersion(rel.Tag); err != nil {
		return Meta{}, nil, err
	}
	// An existing directory means it is already here. Re-installing would have to delete first, and a
	// delete of the SERVING version is refused — so the honest answer is "already installed", not a
	// silent overwrite of bytes a browser may be holding.
	if _, err := os.Stat(in.Store.VersionDir(extID, rel.Tag)); err == nil {
		return Meta{}, nil, fmt.Errorf("%s %s is already installed", extID, rel.Tag)
	}

	extDir := in.Store.ExtDir(extID)
	if err := os.MkdirAll(extDir, 0o755); err != nil {
		return Meta{}, nil, err
	}
	tmp, err := os.MkdirTemp(extDir, ".tmp-install-")
	if err != nil {
		return Meta{}, nil, err
	}
	// Cleaned up on EVERY path out, including success (where it has been renamed away and no longer
	// exists). A leftover .tmp directory is skipped by Installed() but still costs disk forever.
	defer func() { _ = os.RemoveAll(tmp) }()

	sum, n, err := in.download(ctx, rel.AssetURL, filepath.Join(tmp, "ui.tar.gz"))
	if err != nil {
		return Meta{}, nil, err
	}
	if wantSHA != "" && !strings.EqualFold(wantSHA, sum) {
		return Meta{}, nil, fmt.Errorf("sha256 mismatch: got %s, want %s", sum, wantSHA)
	}

	files, err := unpack(filepath.Join(tmp, "ui.tar.gz"), filepath.Join(tmp, "dist"), in.MaxBytes)
	if err != nil {
		return Meta{}, nil, err
	}
	// AN EMPTY OR HEADLESS BUNDLE IS AN INSTALL FAILURE, not a successful install of nothing. Without
	// this the panel serves 404 for everything and the cause is three layers away.
	if _, err := os.Stat(filepath.Join(tmp, "dist", "index.html")); err != nil {
		return Meta{}, nil, errors.New("the archive has no index.html at its root; is this a panel bundle?")
	}
	// The archive itself is not part of the bundle.
	_ = os.Remove(filepath.Join(tmp, "ui.tar.gz"))

	meta := Meta{
		Version: rel.Tag, Repo: repo, Asset: rel.AssetName, SHA256: sum,
		Files: files, Bytes: n, InstalledAt: time.Now().UTC(),
	}
	mb, _ := json.MarshalIndent(meta, "", "  ")
	if err := os.WriteFile(filepath.Join(tmp, MetaFile), mb, 0o644); err != nil {
		return Meta{}, nil, err
	}

	// THE RENAME IS THE COMMIT POINT. Everything above this line is invisible; everything below it
	// addresses a directory a request could now reach.
	final := in.Store.VersionDir(extID, rel.Tag)
	if err := os.Rename(tmp, final); err != nil {
		return Meta{}, nil, err
	}
	info, _ := in.Store.ReadInfo(extID, rel.Tag)
	return meta, info, nil
}

// download fetches the archive and returns its sha256 and size.
func (in *Installer) download(ctx context.Context, url, dest string) (string, int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", 0, err
	}
	resp, err := in.Client.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("cannot download %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("download answered %d", resp.StatusCode)
	}
	f, err := os.Create(dest)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	// READ ONE BYTE PAST THE LIMIT so an oversized archive is refused rather than silently truncated
	// into a tarball that fails to unpack with an unrelated error.
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, in.MaxBytes+1))
	if err != nil {
		return "", 0, err
	}
	if n > in.MaxBytes {
		return "", 0, fmt.Errorf("%w: over %d bytes", ErrTooLarge, in.MaxBytes)
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

/**
 * unpack extracts a tar.gz into dest, returning how many files it wrote.
 *
 * PATHS ARE CHECKED, NOT TRUSTED. A tar entry named "../../etc/passwd" — or an absolute path, or a
 * symlink pointing outside — is how an archive escapes the directory it is unpacked into. The check is
 * on the CLEANED JOINED path, because "a/../b" is fine and "a/../../b" is not, and only resolving says
 * which it is.
 *
 * SYMLINKS ARE REFUSED rather than followed: a bundle has no legitimate need for one, and a link is a
 * way to make a later file write land somewhere else.
 */
func unpack(archive, dest string, maxBytes int64) (int, error) {
	f, err := os.Open(archive)
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return 0, fmt.Errorf("not a gzip archive: %w", err)
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)

	if err := os.MkdirAll(dest, 0o755); err != nil {
		return 0, err
	}
	absDest, err := filepath.Abs(dest)
	if err != nil {
		return 0, err
	}
	files := 0
	var written int64
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return 0, fmt.Errorf("malformed archive: %w", err)
		}
		name := filepath.Clean(hdr.Name)
		if name == "." {
			continue
		}
		target := filepath.Join(absDest, name)
		if target != absDest && !strings.HasPrefix(target, absDest+string(os.PathSeparator)) {
			return 0, fmt.Errorf("the archive contains an entry outside the bundle: %q", hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return 0, err
			}
		case tar.TypeReg:
			if written+hdr.Size > maxBytes {
				return 0, fmt.Errorf("%w: unpacked size over %d bytes", ErrTooLarge, maxBytes)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return 0, err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
			if err != nil {
				return 0, err
			}
			if _, err := io.Copy(out, tr); err != nil {
				_ = out.Close()
				return 0, err
			}
			if err := out.Close(); err != nil {
				return 0, err
			}
			written += hdr.Size
			files++
		case tar.TypeSymlink, tar.TypeLink:
			return 0, fmt.Errorf("the archive contains a link (%q); a panel bundle has no use for one", hdr.Name)
		default:
			// Other entry types (device nodes, fifos, extended headers) are not part of a panel bundle.
			continue
		}
	}
	return files, nil
}
