package github

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"connectrpc.com/connect"
	v1 "github.com/aetherix/aetherixcloud/gen/aetherix/v1"
	"github.com/aetherix/aetherixcloud/gen/aetherix/v1/aetherixv1connect"
	"github.com/aetherix/aetherixcloud/internal/build"
	"github.com/aetherix/aetherixcloud/internal/store"
)

// Service implements aetherixv1connect.GitHubServiceHandler.
type Service struct{ M *Module }

var _ aetherixv1connect.GitHubServiceHandler = (*Service)(nil)

func appResponse(cfg *AppConfig) *v1.GetAppResponse {
	if cfg == nil {
		return &v1.GetAppResponse{}
	}
	return &v1.GetAppResponse{Configured: true, AppId: cfg.AppID, Slug: cfg.Slug, HtmlUrl: cfg.HTMLURL, WebhookUrl: cfg.WebhookURL}
}

func rpcErr(err error) error {
	var ce *connect.Error
	var ae *APIError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &ce):
		return err
	case errors.Is(err, errNotConfigured):
		return connect.NewError(connect.CodeFailedPrecondition, err)
	case errors.As(err, &ae) && ae.Status == 404:
		return connect.NewError(connect.CodeNotFound, err)
	case errors.As(err, &ae) && (ae.Status == 401 || ae.Status == 403):
		return connect.NewError(connect.CodePermissionDenied, err)
	case store.IsNotFound(err):
		return connect.NewError(connect.CodeNotFound, err)
	}
	return connect.NewError(connect.CodeInternal, err)
}

// GetApp implements GitHubService.
func (s *Service) GetApp(ctx context.Context, _ *connect.Request[v1.GetAppRequest]) (*connect.Response[v1.GetAppResponse], error) {
	cfg, err := s.M.LoadApp(ctx)
	if err != nil && !store.IsNotFound(err) {
		return nil, rpcErr(err)
	}
	return connect.NewResponse(appResponse(cfg)), nil
}

// BeginManifestSetup implements GitHubService.
func (s *Service) BeginManifestSetup(ctx context.Context, req *connect.Request[v1.BeginManifestSetupRequest]) (*connect.Response[v1.BeginManifestSetupResponse], error) {
	mj, post, state, err := s.M.BeginManifest(ctx, req.Msg.BaseUrl, req.Msg.Organization)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	return connect.NewResponse(&v1.BeginManifestSetupResponse{ManifestJson: mj, PostUrl: post, State: state}), nil
}

// CompleteManifestSetup implements GitHubService.
func (s *Service) CompleteManifestSetup(ctx context.Context, req *connect.Request[v1.CompleteManifestSetupRequest]) (*connect.Response[v1.CompleteManifestSetupResponse], error) {
	cfg, _, err := s.M.CompleteManifest(ctx, req.Msg.Code, req.Msg.State)
	if err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	}
	return connect.NewResponse(&v1.CompleteManifestSetupResponse{App: appResponse(cfg)}), nil
}

// ListInstallations implements GitHubService (refreshing from GitHub when possible).
func (s *Service) ListInstallations(ctx context.Context, _ *connect.Request[v1.ListInstallationsRequest]) (*connect.Response[v1.ListInstallationsResponse], error) {
	if _, err := s.M.LoadApp(ctx); err == nil {
		if err := s.M.SyncInstallations(ctx); err != nil {
			s.M.logger().Debug("sync installations", "err", err)
		}
	}
	list, err := listInstallations(ctx, s.M.Store)
	if err != nil {
		return nil, rpcErr(err)
	}
	return connect.NewResponse(&v1.ListInstallationsResponse{Installations: list}), nil
}

// ListRepositories implements GitHubService.
func (s *Service) ListRepositories(ctx context.Context, req *connect.Request[v1.ListRepositoriesRequest]) (*connect.Response[v1.ListRepositoriesResponse], error) {
	c, err := s.M.Client(ctx)
	if err != nil {
		return nil, rpcErr(err)
	}
	var instIDs []int64
	if req.Msg.InstallationId != 0 {
		instIDs = []int64{req.Msg.InstallationId}
	} else {
		ins, err := listInstallations(ctx, s.M.Store)
		if err != nil {
			return nil, rpcErr(err)
		}
		for _, in := range ins {
			instIDs = append(instIDs, in.Id)
		}
	}
	q := strings.ToLower(req.Msg.Query)
	resp := &v1.ListRepositoriesResponse{}
	for _, id := range instIDs {
		repos, err := c.ListRepos(ctx, id)
		if err != nil {
			return nil, rpcErr(err)
		}
		for _, r := range repos {
			if q != "" && !strings.Contains(strings.ToLower(r.FullName), q) {
				continue
			}
			resp.Repositories = append(resp.Repositories, &v1.Repository{FullName: r.FullName,
				DefaultBranch: r.DefaultBranch, Private: r.Private, InstallationId: id})
		}
	}
	return connect.NewResponse(resp), nil
}

// ListBranches implements GitHubService.
func (s *Service) ListBranches(ctx context.Context, req *connect.Request[v1.ListBranchesRequest]) (*connect.Response[v1.ListBranchesResponse], error) {
	if req.Msg.Repo == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("repo is required"))
	}
	c, err := s.M.Client(ctx)
	if err != nil {
		return nil, rpcErr(err)
	}
	inst, err := s.M.resolveInstallation(ctx, req.Msg.InstallationId, req.Msg.Repo)
	if err != nil {
		return nil, rpcErr(err)
	}
	br, err := c.ListBranches(ctx, inst, req.Msg.Repo)
	if err != nil {
		return nil, rpcErr(err)
	}
	return connect.NewResponse(&v1.ListBranchesResponse{Branches: br}), nil
}

var shaRe = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)

// EnqueuePushBuild resolves the latest commit for the service's GitHub source
// and enqueues the same build a push webhook would. ref may be empty (branch).
func (m *Module) EnqueuePushBuild(ctx context.Context, serviceID, ref string) (buildID, commitSHA string, err error) {
	svc, err := store.GetService(ctx, m.Store, serviceID)
	if err != nil {
		return "", "", fmt.Errorf("service %q not found", serviceID)
	}
	gh := svc.Spec.GetSource().GetGithub()
	if gh == nil || gh.Repo == "" {
		return "", "", errors.New("service has no GitHub source")
	}
	c, err := m.Client(ctx)
	if err != nil {
		return "", "", err
	}
	inst, err := m.resolveInstallation(ctx, gh.InstallationId, gh.Repo)
	if err != nil {
		return "", "", err
	}
	ref = firstNonEmpty(ref, gh.Branch)
	if ref == "" {
		return "", "", errors.New("service has no branch: pass ref")
	}
	cm, err := c.GetCommit(ctx, inst, gh.Repo, ref)
	if err != nil {
		return "", "", err
	}
	refName := ref
	if !strings.HasPrefix(refName, "refs/") && !shaRe.MatchString(ref) {
		refName = "refs/heads/" + ref
	} else if shaRe.MatchString(ref) {
		refName = "refs/heads/" + gh.Branch
	}
	b, _, err := build.Enqueue(ctx, m.Store, build.JobMeta{ServiceID: svc.Id, Kind: "github", Repo: gh.Repo,
		InstallationID: inst, Ref: refName, SHA: cm.SHA, Message: cm.Message, Author: cm.Author})
	if err != nil {
		return "", "", err
	}
	return b.Id, cm.SHA, nil
}

// SimulatePush implements GitHubService: it resolves the latest commit of the
// service's branch through the GitHub API and enqueues the same build that a
// real push webhook would.
func (s *Service) SimulatePush(ctx context.Context, req *connect.Request[v1.SimulatePushRequest]) (*connect.Response[v1.SimulatePushResponse], error) {
	id, sha, err := s.M.EnqueuePushBuild(ctx, req.Msg.ServiceId, req.Msg.Ref)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			return nil, connect.NewError(connect.CodeNotFound, err)
		}
		if strings.Contains(err.Error(), "no GitHub source") || strings.Contains(err.Error(), "no branch") {
			return nil, connect.NewError(connect.CodeFailedPrecondition, err)
		}
		if strings.Contains(err.Error(), "pass ref") {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
		return nil, rpcErr(err)
	}
	return connect.NewResponse(&v1.SimulatePushResponse{BuildId: id, CommitSha: sha}), nil
}

// SetWebhookUrl implements GitHubService: it stores the URL and, when the App
// exists, updates the App's webhook configuration on GitHub.
func (s *Service) SetWebhookUrl(ctx context.Context, req *connect.Request[v1.SetWebhookUrlRequest]) (*connect.Response[v1.SetWebhookUrlResponse], error) {
	u := strings.TrimSpace(req.Msg.Url)
	if !strings.HasPrefix(u, "https://") && !strings.HasPrefix(u, "http://") {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("url must be http(s)"))
	}
	cfg, err := s.M.LoadApp(ctx)
	if err != nil {
		return nil, rpcErr(errNotConfigured)
	}
	c, err := s.M.Client(ctx)
	if err != nil {
		return nil, rpcErr(err)
	}
	if err := c.SetWebhookURL(ctx, u); err != nil {
		return nil, rpcErr(err)
	}
	cfg.WebhookURL = u
	if err := s.M.SaveApp(ctx, cfg); err != nil {
		return nil, rpcErr(err)
	}
	return connect.NewResponse(&v1.SetWebhookUrlResponse{}), nil
}

// DisconnectApp implements GitHubService: drops cluster credentials and
// installation cache. confirm must be true.
func (s *Service) DisconnectApp(ctx context.Context, req *connect.Request[v1.DisconnectAppRequest]) (*connect.Response[v1.DisconnectAppResponse], error) {
	if !req.Msg.Confirm {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("confirm must be true"))
	}
	if _, err := s.M.LoadApp(ctx); err != nil {
		if store.IsNotFound(err) {
			return connect.NewResponse(&v1.DisconnectAppResponse{}), nil
		}
		return nil, rpcErr(err)
	}
	if err := s.M.ClearApp(ctx); err != nil {
		return nil, rpcErr(err)
	}
	return connect.NewResponse(&v1.DisconnectAppResponse{}), nil
}
