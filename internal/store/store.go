// Package store owns the on-disk layout of installed panels.
//
// EVERY VERSION LIVES IN ITS OWN DIRECTORY, and `current` is a symlink that decides which one serves.
// That is the whole design:
//
//   * an install writes to a new directory, so a failed or broken package can never damage the version
//     a reader is looking at;
//   * a switch is one `rename` of a symlink, which is atomic — there is no window where the extension
//     has no panel or two;
//   * a rollback is the same operation, because the old version is still on disk.
//
// A single directory overwritten in place would give none of those, and the failure would be a panel
// that 404s for everybody at once.
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// CurrentLink is the symlink that says which version is serving.
const CurrentLink = "current"

// MetaFile records what was installed and where it came from.
const MetaFile = "install.json"

// InfoFile is the panel's own declaration of itself, inside the unpacked bundle.
const InfoFile = "ui.json"

// Meta is what an installed version records about itself.
//
// THE DIGEST IS STORED so the next install can pin it, and so a reader can tell whether the bytes on
// disk are still the bytes that were downloaded. Without it, "verify the supply chain" would mean
// re-hashing against a value nobody wrote down.
type Meta struct {
	Version     string    `json:"version"`
	Repo        string    `json:"repo"`
	Asset       string    `json:"asset"`
	SHA256      string    `json:"sha256"`
	Files       int       `json:"files"`
	Bytes       int64     `json:"bytes"`
	InstalledAt time.Time `json:"installedAt"`
}

// Store is a directory of installed panels.
type Store struct {
	Root string
}

// ExtDir is where one extension's versions live.
func (s *Store) ExtDir(extID string) string {
	return filepath.Join(s.Root, SanitizeID(extID))
}

// VersionDir is one installed version's directory.
func (s *Store) VersionDir(extID, version string) string {
	return filepath.Join(s.ExtDir(extID), SanitizeVersion(version))
}

// BundleDir is the directory a request is served from: the active version's `dist`.
//
// `dist` rather than the version directory itself, because the bundle carries its own metadata
// (ui.json) beside the assets, and serving that file would put the panel's own description at a URL.
func (s *Store) BundleDir(extID string) (string, error) {
	p := filepath.Join(s.ExtDir(extID), CurrentLink, "dist")
	st, err := os.Stat(p)
	if err != nil {
		return "", err
	}
	if !st.IsDir() {
		return "", fmt.Errorf("%s is not a directory", p)
	}
	return p, nil
}

/**
 * SanitizeID makes an extension id safe to use as one path element.
 *
 * THE ID COMES FROM AN API CALLER and becomes a directory name, so it is an injection point: `..`
 * would climb out of the data root, and a `/` would write outside the extension's own tree. The
 * allowed set is deliberately narrow — an extension id is a DNS label, and refusing anything else is
 * better than mangling it into something that collides with another id.
 */
func SanitizeID(id string) string {
	clean := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return -1
		}
	}, strings.ToLower(id))
	return clean
}

// SanitizeVersion makes a version tag safe as one path element. Dots and '+' are legal in a semver
// pre-release/build suffix, so they are kept; separators and `..` are not.
func SanitizeVersion(v string) string {
	clean := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r == '.', r == '-', r == '_', r == '+':
			return r
		default:
			return -1
		}
	}, v)
	return strings.Trim(clean, ".")
}

// ValidID reports whether an id is usable, and why not when it is not.
//
// CHECKED BEFORE SANITISING, not after: silently rewriting "Git dev" to "gitdev" would install a panel
// under a name the caller did not ask for, and the console would look for the name they did ask for.
func ValidID(id string) error {
	if id == "" {
		return errors.New("ext_id is required")
	}
	if SanitizeID(id) != id {
		return fmt.Errorf("ext_id %q must be lowercase letters, digits, dashes and underscores", id)
	}
	return nil
}

// ValidVersion reports whether a version is usable.
func ValidVersion(v string) error {
	if v == "" {
		return errors.New("version is required")
	}
	if SanitizeVersion(v) != v {
		return fmt.Errorf("version %q contains characters a directory name cannot", v)
	}
	return nil
}

// Installed lists an extension's versions, newest-installed first.
//
// THE ORDER IS BY INSTALL TIME, not by version string: "v0.10.0" sorts before "v0.9.0" lexically, and
// a list that puts the older one first is actively misleading when somebody is choosing what to
// activate.
func (s *Store) Installed(extID string) ([]Meta, error) {
	dir := s.ExtDir(extID)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Meta
	for _, e := range entries {
		if !e.IsDir() || e.Name() == CurrentLink || strings.HasPrefix(e.Name(), ".tmp-") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name(), MetaFile))
		if err != nil {
			// A directory without metadata is a half-finished install. Skipped rather than reported:
			// it was never visible to a reader, and naming it as a version would offer something that
			// cannot be activated.
			continue
		}
		var m Meta
		if json.Unmarshal(b, &m) != nil {
			continue
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].InstalledAt.After(out[j].InstalledAt) })
	return out, nil
}

// Active reports which version is serving, or "".
func (s *Store) Active(extID string) string {
	target, err := os.Readlink(filepath.Join(s.ExtDir(extID), CurrentLink))
	if err != nil {
		return ""
	}
	return filepath.Base(target)
}

/**
 * Activate points the extension at one installed version.
 *
 * THE SWITCH IS A RENAME OF A SYMLINK, which is atomic on a POSIX filesystem: a request either sees
 * the old target or the new one. Writing the link in place — or removing it and creating it again —
 * leaves a window where the extension has no panel, and a request in that window 404s.
 */
func (s *Store) Activate(extID, version string) error {
	dir := s.ExtDir(extID)
	if _, err := os.Stat(filepath.Join(dir, version, "dist")); err != nil {
		return fmt.Errorf("%s version %s is not installed", extID, version)
	}
	link := filepath.Join(dir, CurrentLink)
	// A temporary name in the SAME directory: rename cannot cross filesystems, and the link must end
	// up in the extension's own directory.
	tmp := filepath.Join(dir, ".tmp-link-"+version)
	_ = os.Remove(tmp)
	if err := os.Symlink(version, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, link); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

/**
 * Remove deletes one installed version.
 *
 * IT REFUSES THE ACTIVE ONE. Deleting what is serving leaves the extension with a dangling symlink and
 * every request to it a 404 — which, from the console, is indistinguishable from a broken panel. The
 * caller has to activate something else (or nothing) first, so the state they end up in is one they
 * chose.
 */
func (s *Store) Remove(extID, version string) error {
	if s.Active(extID) == version {
		return fmt.Errorf("%s version %s is serving; activate another version first", extID, version)
	}
	dir := s.VersionDir(extID, version)
	if _, err := os.Stat(dir); err != nil {
		return fmt.Errorf("%s version %s is not installed", extID, version)
	}
	return os.RemoveAll(dir)
}

// ReadInfo reads a version's ui.json, falling back to the installed copy.
//
// THE INSTALLED COPY IS INSIDE THE BUNDLE, so this reads from disk rather than from memory: a panel
// whose metadata changed on disk (an operator fixed a typo) should be reported as it is now, not as it
// was at install time.
func (s *Store) ReadInfo(extID, version string) (map[string]any, error) {
	p := filepath.Join(s.VersionDir(extID, version), "dist", InfoFile)
	b, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]any{}, nil
		}
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		return map[string]any{}, fmt.Errorf("%s: %w", p, err)
	}
	return out, nil
}
