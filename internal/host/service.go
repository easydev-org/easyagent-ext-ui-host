// Package host implements the admin API and the static serving that goes with it.
package host

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"connectrpc.com/connect/v2"

	uiv1 "github.com/easydev-org/easyagent-ext-ui-host/gen/ui/v1"
	"github.com/easydev-org/easyagent-ext-ui-host/gen/ui/v1/uiv1connect"
	"github.com/easydev-org/easyagent-ext-ui-host/internal/store"
)

// Service is the admin API.
type Service struct {
	Store     *store.Store
	Installer *store.Installer
	// ConfiguredPanels is what `Sync` updates when the caller passes no list.
	//
	// FROM THE HOST'S OWN CONFIGURATION (PANEL_REPOS), because "which panels does this deployment
	// want" is a deployment fact, not something a caller should have to repeat on every restart. An
	// empty list means Sync requires an explicit one — the honest failure for a host that was never
	// told, rather than silently doing nothing.
	ConfiguredPanels []*uiv1.PanelRepo
}

var _ uiv1connect.UiHostServiceHandler = (*Service)(nil)

// NewServer builds a Connect server for the admin API, with authentication installed.
//
// THE INTERCEPTOR IS PASSED BY THE CALLER because whether installing needs a credential is a deployment
// decision, not a property of this service — the same code runs behind an ingress that authenticates
// and on a laptop where it does not.
func (s *Service) NewServer(interceptors ...connect.ServerInterceptor) *connect.Server {
	server := connect.NewServer(interceptors...)
	uiv1connect.RegisterUiHostServiceHandler(server, s)
	return server
}

// Install fetches a release and records it, WITHOUT activating it.
func (s *Service) Install(ctx context.Context, req *uiv1.InstallRequest) (*uiv1.InstallResponse, error) {
	if strings.TrimSpace(req.GetRepo()) == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			"repo is required (owner/name of the repository that publishes the panel)")
	}
	if !strings.Contains(req.GetRepo(), "/") {
		return nil, connect.NewError(connect.CodeInvalidArgument, "repo must be owner/name")
	}
	meta, info, err := s.Installer.Install(ctx, req.GetExtId(), req.GetRepo(),
		req.GetVersion(), req.GetAsset(), req.GetSha256())
	if err != nil {
		return nil, codeFor(err)
	}
	return &uiv1.InstallResponse{
		ExtId:   req.GetExtId(),
		Version: meta.Version,
		Files:   int32(meta.Files),
		Bytes:   meta.Bytes,
		Sha256:  meta.SHA256,
		Info:    panelInfo(info),
	}, nil
}

// List reports what is installed and what is serving.
func (s *Service) List(_ context.Context, _ *uiv1.ListRequest) (*uiv1.ListResponse, error) {
	exts, err := s.Store.Extensions()
	if err != nil {
		return nil, codeFor(err)
	}
	out := make([]*uiv1.ExtState, 0, len(exts))
	for _, id := range exts {
		st, err := s.state(id)
		if err != nil {
			return nil, codeFor(err)
		}
		out = append(out, st)
	}
	return &uiv1.ListResponse{Extensions: out}, nil
}

// state assembles one extension's row.
func (s *Service) state(extID string) (*uiv1.ExtState, error) {
	metas, err := s.Store.Installed(extID)
	if err != nil {
		return nil, err
	}
	active := s.Store.Active(extID)
	st := &uiv1.ExtState{ExtId: extID, Active: active}
	for _, m := range metas {
		st.Versions = append(st.Versions, &uiv1.InstalledVersion{
			Version:     m.Version,
			Files:       int32(m.Files),
			Bytes:       m.Bytes,
			Sha256:      m.SHA256,
			InstalledAt: m.InstalledAt.Format("2006-01-02T15:04:05Z07:00"),
			Repo:        m.Repo,
		})
	}
	// The metadata is read from the ACTIVE version only: an inactive version's description is not what
	// the console should show.
	if active != "" {
		info, ierr := s.Store.ReadInfo(extID, active)
		if ierr == nil && len(info) > 0 {
			st.Info = panelInfo(info)
		}
	}
	return st, nil
}

// Activate points the extension at one installed version.
func (s *Service) Activate(_ context.Context, req *uiv1.ActivateRequest) (*uiv1.ActivateResponse, error) {
	if err := store.ValidID(req.GetExtId()); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err.Error())
	}
	if err := store.ValidVersion(req.GetVersion()); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err.Error())
	}
	if err := s.Store.Activate(req.GetExtId(), req.GetVersion()); err != nil {
		return nil, codeFor(err)
	}
	return &uiv1.ActivateResponse{ExtId: req.GetExtId(), Active: req.GetVersion()}, nil
}

// Remove deletes one version, refusing the active one.
func (s *Service) Remove(_ context.Context, req *uiv1.RemoveRequest) (*uiv1.RemoveResponse, error) {
	if err := store.ValidID(req.GetExtId()); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err.Error())
	}
	if err := store.ValidVersion(req.GetVersion()); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err.Error())
	}
	if err := s.Store.Remove(req.GetExtId(), req.GetVersion()); err != nil {
		return nil, codeFor(err)
	}
	return &uiv1.RemoveResponse{
		ExtId:   req.GetExtId(),
		Version: req.GetVersion(),
		Active:  s.Store.Active(req.GetExtId()),
	}, nil
}

// panelInfo converts a panel's ui.json into the proto shape.
func panelInfo(m map[string]any) *uiv1.PanelInfo {
	if len(m) == 0 {
		return nil
	}
	out := &uiv1.PanelInfo{}
	if v, ok := m["title"].(string); ok {
		out.Title = v
	}
	if v, ok := m["icon"].(string); ok {
		out.Icon = v
	}
	if raw, ok := m["descriptions"].(map[string]any); ok {
		out.Descriptions = make(map[string]string, len(raw))
		for lang, v := range raw {
			// Blank entries are DROPPED: the console picks a language and falls back to the first
			// non-empty, and an empty string would win that fallback and render nothing.
			if s, ok := v.(string); ok && s != "" {
				out.Descriptions[lang] = s
			}
		}
	}
	// THE CONTRIBUTIONS, so an admin caller sees the same thing the console will.
	//
	// WITHOUT THIS the HTTP answer at /ext/<id>/api/v1/ui carries them (that path passes the raw
	// ui.json through) while every RPC answer omits them — two views of one panel that disagree, and
	// the one a script reads is the one that is wrong.
	if c, ok := m["contributes"].(map[string]any); ok {
		out.Contributes = contributesInfo(c)
	}
	return out
}

/**
 * contributesInfo converts the panel's `contributes` object into the proto shape.
 *
 * THE WIRE SPELLS IT `session_views` while the interface uses camelCase elsewhere; both are accepted
 * here for the same reason PANEL_REPOS accepts both — a panel author writing either should not end up
 * with a declaration that is silently ignored.
 *
 * A MALFORMED ENTRY IS DROPPED, NOT THE WHOLE TABLE: a session view with no menu label cannot be
 * opened, but it must not cost the panel its other views.
 */
func contributesInfo(c map[string]any) *uiv1.Contributes {
	out := &uiv1.Contributes{}
	if pages, ok := c["pages"].([]any); ok {
		for _, raw := range pages {
			p, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			route, _ := p["route"].(string)
			if route == "" {
				continue
			}
			page := &uiv1.Page{Route: route}
			page.Title, _ = p["title"].(string)
			page.Icon, _ = p["icon"].(string)
			if d, ok := p["descriptions"].(map[string]any); ok {
				page.Descriptions = stringMap(d)
			}
			out.Pages = append(out.Pages, page)
		}
	}
	rawViews, ok := c["session_views"].([]any)
	if !ok {
		rawViews, _ = c["sessionViews"].([]any)
	}
	for _, raw := range rawViews {
		s, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		id, _ := s["id"].(string)
		route, _ := s["route"].(string)
		label, _ := s["menu_label"].(string)
		if label == "" {
			label, _ = s["menuLabel"].(string)
		}
		if id == "" || route == "" || label == "" {
			continue
		}
		view := &uiv1.SessionView{Id: id, Route: route, MenuLabel: label}
		view.Title, _ = s["title"].(string)
		ns, _ := s["selection_namespace"].(string)
		if ns == "" {
			ns, _ = s["selectionNamespace"].(string)
		}
		view.SelectionNamespace = ns
		out.SessionViews = append(out.SessionViews, view)
	}
	if proxies, ok := c["proxies"].([]any); ok {
		for _, raw := range proxies {
			if s, ok := raw.(string); ok && s != "" {
				out.Proxies = append(out.Proxies, s)
			}
		}
	}
	return out
}

// stringMap keeps only non-empty string values, for the language-keyed maps.
func stringMap(raw map[string]any) map[string]string {
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		if s, ok := v.(string); ok && s != "" {
			out[k] = s
		}
	}
	return out
}

/**
 * codeFor maps a store error to a Connect code.
 *
 * THE DISTINCTION THAT MATTERS IS "the caller can fix this" versus "the server is broken". A version
 * that is not installed, a hash that does not match, an archive with no index.html — all of those are
 * facts about the request and the artifact, and answering INTERNAL for them sends the reader looking
 * for a server bug.
 */
func codeFor(err error) error {
	msg := err.Error()
	switch {
	case errors.Is(err, store.ErrTooLarge):
		return connect.NewError(connect.CodeResourceExhausted, msg)
	case strings.Contains(msg, "is already installed"),
		strings.Contains(msg, "is not installed"),
		strings.Contains(msg, "is serving"),
		strings.Contains(msg, "has no release"),
		strings.Contains(msg, "has no asset"),
		strings.Contains(msg, "no index.html"),
		strings.Contains(msg, "sha256 mismatch"),
		strings.Contains(msg, "outside the bundle"),
		strings.Contains(msg, "not a gzip"),
		strings.Contains(msg, "must be lowercase"),
		strings.Contains(msg, "contains characters"):
		return connect.NewError(connect.CodeInvalidArgument, msg)
	case strings.Contains(msg, "cannot reach GitHub"), strings.Contains(msg, "cannot download"):
		// A dependency being unreachable is not a client error and not a bug here.
		return connect.NewError(connect.CodeUnavailable, msg)
	default:
		return connect.NewError(connect.CodeInternal, fmt.Sprintf("install: %v", err))
	}
}
