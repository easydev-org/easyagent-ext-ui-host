package host

import (
	"context"
	"strings"

	"connectrpc.com/connect/v2"

	uiv1 "github.com/easydev-org/easyagent-ext-ui-host/gen/ui/v1"
	"github.com/easydev-org/easyagent-ext-ui-host/internal/store"
)

/**
 * Update resolves the newest release, installs it, and activates it — as one operation.
 *
 * THE POINT IS THE ATOMICITY, not the download. `Install` followed by `Activate` is two requests, and
 * the second is easy to leave out: the panel lands on disk, the download reports success, and readers
 * keep getting the old one (or nothing, on a first install). That failure has no error anywhere, which
 * is why it is worth a second RPC rather than a comment telling callers to remember.
 *
 * IT IS STILL SAFE IN THE WAY `Install` IS. A new version is unpacked into its own directory and only
 * activated after the download, the hash check and the index.html check have all passed — so a broken
 * release leaves the active panel untouched. And when the newest version is ALREADY active, nothing is
 * downloaded and `changed` is false, so a cron job calling this every hour costs one API request.
 */
func (s *Service) Update(ctx context.Context, req *uiv1.UpdateRequest) (*uiv1.UpdateResponse, error) {
	extID := strings.TrimSpace(req.GetExtId())
	if err := store.ValidID(extID); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err.Error())
	}

	// THE REPO MAY BE INFERRED FROM WHAT INSTALLED THE ACTIVE VERSION, which is what lets a caller that
	// only knows an ext id (a cron job, a deployment that forgot which repository publishes a panel)
	// still update it. An explicit value still wins, so a panel can be moved to a new repo deliberately.
	repo := strings.TrimSpace(req.GetRepo())
	if repo == "" {
		repo = s.repoOf(extID)
	}
	if repo == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			"repo is required: nothing has been installed for "+extID+" and no repo was given")
	}
	if !strings.Contains(repo, "/") {
		return nil, connect.NewError(connect.CodeInvalidArgument, "repo must be owner/name")
	}

	from := s.Store.Active(extID)

	// RESOLVE ONLY. A dry run must not touch the disk: the caller is asking whether an update exists,
	// and downloading a panel to answer that would make "check for updates" as expensive as updating.
	rel, err := s.Installer.Source.Find(ctx, repo, latestTag, req.GetAsset())
	if err != nil {
		return nil, codeFor(err)
	}
	if err := store.ValidVersion(rel.Tag); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err.Error())
	}

	if rel.Tag == from {
		// ALREADY CURRENT. Reported rather than treated as an error: this is the ordinary answer for a
		// scheduled check, and an error would turn a healthy no-op into red in whatever calls it.
		return &uiv1.UpdateResponse{
			ExtId: extID, From: from, To: rel.Tag, Changed: false, Repo: repo,
			Info: s.activeInfo(extID),
		}, nil
	}

	if req.GetDryRun() {
		return &uiv1.UpdateResponse{
			ExtId: extID, From: from, To: rel.Tag, Changed: false, Repo: repo,
			Info: s.activeInfo(extID),
		}, nil
	}

	// INSTALL, THEN ACTIVATE — and only then. An error in either leaves `from` serving.
	meta, info, err := s.Installer.Install(ctx, extID, repo, rel.Tag, req.GetAsset(), "")
	if err != nil && !isAlreadyInstalled(err) {
		return nil, codeFor(err)
	}
	// A version that is on disk but not serving (an earlier install that failed to activate, or a
	// deliberate install-then-look) is not an error here: the goal is for it to be serving.
	if err := s.Store.Activate(extID, rel.Tag); err != nil {
		return nil, codeFor(err)
	}

	resp := &uiv1.UpdateResponse{
		ExtId: extID, From: from, To: rel.Tag, Changed: true, Repo: repo,
		Info: panelInfo(info),
	}
	if meta.SHA256 != "" {
		resp.Sha256 = meta.SHA256
		resp.Files = int32(meta.Files)
		resp.Bytes = meta.Bytes
	} else if info == nil {
		// Already-installed path: recover the recorded digest so the response is still useful, and so a
		// caller pinning the next download has a value.
		if m, ok := s.metaOf(extID, rel.Tag); ok {
			resp.Sha256, resp.Files, resp.Bytes = m.SHA256, int32(m.Files), m.Bytes
			resp.Info = s.infoOf(extID, rel.Tag)
		}
	}
	return resp, nil
}

/**
 * Sync updates several panels, reporting each outcome separately.
 *
 * ONE FAILURE DOES NOT ABORT THE REST. A release that is unreachable, a repo that was renamed, a panel
 * whose archive is missing its index.html — each is a fact about ONE extension, and letting it stop the
 * others would make a single bad panel freeze the whole deployment's updates. The caller sees exactly
 * which entries failed and why.
 */
func (s *Service) Sync(ctx context.Context, req *uiv1.SyncRequest) (*uiv1.SyncResponse, error) {
	panels := req.GetPanels()
	if len(panels) == 0 {
		// THE HOST'S OWN LIST, so a deployment can come back to a known state with one empty call.
		panels = s.ConfiguredPanels
	}
	if len(panels) == 0 {
		return nil, connect.NewError(connect.CodeFailedPrecondition,
			"no panels to sync: pass a list, or configure PANEL_REPOS on this host")
	}

	out := &uiv1.SyncResponse{Results: make([]*uiv1.SyncResult, 0, len(panels))}
	for _, p := range panels {
		res := &uiv1.SyncResult{ExtId: p.GetExtId(), Repo: p.GetRepo()}
		upd, err := s.Update(ctx, &uiv1.UpdateRequest{
			ExtId:  p.GetExtId(),
			Repo:   p.GetRepo(),
			DryRun: req.GetDryRun(),
		})
		if err != nil {
			// THE MESSAGE, NOT THE CODE. A SyncResult is read by a human or a deploy script; the
			// Connect code is already lost by the time it is printed, and "internal" says nothing
			// about which panel or why.
			res.Error = err.Error()
		} else {
			res.Update = upd
			res.Repo = upd.GetRepo()
		}
		out.Results = append(out.Results, res)
	}
	return out, nil
}

/** latestTag is the sentinel the release source resolves to "the newest release". */
const latestTag = "latest"

// isAlreadyInstalled reports whether err is the store's "this version is on disk" case.
//
// THE INSTALLER REFUSES TO OVERWRITE, deliberately: replacing bytes a browser may still be holding is
// worse than saying no. For Update that refusal is not a failure — the version we wanted is present,
// and the next step (Activate) is what the caller actually asked for.
func isAlreadyInstalled(err error) bool {
	return err != nil && strings.Contains(err.Error(), "is already installed")
}

// repoOf reports which repository installed the active version, or "".
func (s *Service) repoOf(extID string) string {
	active := s.Store.Active(extID)
	if active == "" {
		return ""
	}
	if m, ok := s.metaOf(extID, active); ok {
		return m.Repo
	}
	return ""
}

// metaOf reads one installed version's recorded metadata.
func (s *Service) metaOf(extID, version string) (store.Meta, bool) {
	metas, err := s.Store.Installed(extID)
	if err != nil {
		return store.Meta{}, false
	}
	for _, m := range metas {
		if m.Version == version {
			return m, true
		}
	}
	return store.Meta{}, false
}

// activeInfo is the active version's panel metadata, or nil.
func (s *Service) activeInfo(extID string) *uiv1.PanelInfo {
	if active := s.Store.Active(extID); active != "" {
		return s.infoOf(extID, active)
	}
	return nil
}

// infoOf reads one version's ui.json into the proto shape.
func (s *Service) infoOf(extID, version string) *uiv1.PanelInfo {
	info, err := s.Store.ReadInfo(extID, version)
	if err != nil || len(info) == 0 {
		return nil
	}
	return panelInfo(info)
}
