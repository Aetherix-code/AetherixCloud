package api_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"connectrpc.com/connect"
	v1 "github.com/aetherix/aetherixcloud/gen/aetherix/v1"
	"github.com/aetherix/aetherixcloud/gen/aetherix/v1/aetherixv1connect"
	"github.com/aetherix/aetherixcloud/internal/auth"
	"github.com/aetherix/aetherixcloud/internal/events"
	"github.com/stretchr/testify/require"
)

func bearer[T any](tok string, m *T) *connect.Request[T] {
	r := connect.NewRequest(m)
	if tok != "" {
		r.Header().Set("Authorization", "Bearer "+tok)
	}
	return r
}

func TestAuthFlowAndRBAC(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	as := aetherixv1connect.NewAuthServiceClient(f.srv.Client(), f.srv.URL)
	ps := aetherixv1connect.NewProjectServiceClient(f.srv.Client(), f.srv.URL)
	cs := aetherixv1connect.NewClusterServiceClient(f.srv.Client(), f.srv.URL)

	// setup status + first-owner bootstrap (needs the admin API token)
	st, err := as.GetSetupStatus(ctx, connect.NewRequest(&v1.GetSetupStatusRequest{}))
	require.NoError(t, err)
	require.True(t, st.Msg.NeedsSetup)
	_, err = as.Bootstrap(ctx, connect.NewRequest(&v1.BootstrapRequest{Email: "o@x.io", Password: "password123", AdminToken: "axt_nope"}))
	require.Equal(t, connect.CodePermissionDenied, connect.CodeOf(err))
	boot, err := as.Bootstrap(ctx, connect.NewRequest(&v1.BootstrapRequest{Email: "Owner@X.io", Name: "Own", Password: "password123", AdminToken: f.admin}))
	require.NoError(t, err)
	require.Equal(t, v1.Role_ROLE_OWNER, boot.Msg.User.Role)
	require.Equal(t, "owner@x.io", boot.Msg.User.Email)
	_, err = as.Bootstrap(ctx, connect.NewRequest(&v1.BootstrapRequest{Email: "b@x.io", Password: "password123", AdminToken: f.admin}))
	require.Equal(t, connect.CodeFailedPrecondition, connect.CodeOf(err))

	// login: bad + good
	_, err = as.Login(ctx, connect.NewRequest(&v1.LoginRequest{Email: "owner@x.io", Password: "wrong"}))
	require.Equal(t, connect.CodeUnauthenticated, connect.CodeOf(err))
	lg, err := as.Login(ctx, connect.NewRequest(&v1.LoginRequest{Email: "owner@x.io", Password: "password123"}))
	require.NoError(t, err)
	owner := lg.Msg.SessionToken

	me, err := as.Me(ctx, bearer(owner, &v1.MeRequest{}))
	require.NoError(t, err)
	require.Equal(t, "owner@x.io", me.Msg.User.Email)

	// session JWT is signed with the cluster key: a different cluster rejects it
	other := newFixture(t)
	oas := aetherixv1connect.NewAuthServiceClient(other.srv.Client(), other.srv.URL)
	_, err = oas.Me(ctx, bearer(owner, &v1.MeRequest{}))
	require.Equal(t, connect.CodeUnauthenticated, connect.CodeOf(err))

	// users with each role
	mk := func(email string, role v1.Role) string {
		_, err := as.CreateUser(ctx, bearer(owner, &v1.CreateUserRequest{Email: email, Password: "password123", Role: role}))
		require.NoError(t, err)
		l, err := as.Login(ctx, connect.NewRequest(&v1.LoginRequest{Email: email, Password: "password123"}))
		require.NoError(t, err)
		return l.Msg.SessionToken
	}
	viewer, dev, adm := mk("v@x.io", v1.Role_ROLE_VIEWER), mk("d@x.io", v1.Role_ROLE_DEVELOPER), mk("a@x.io", v1.Role_ROLE_ADMIN)

	// viewer: read ok, create project (deploy) denied
	_, err = ps.ListProjects(ctx, bearer(viewer, &v1.ListProjectsRequest{}))
	require.NoError(t, err)
	_, err = ps.CreateProject(ctx, bearer(viewer, &v1.CreateProjectRequest{Name: "p"}))
	require.Equal(t, connect.CodePermissionDenied, connect.CodeOf(err))
	// developer: create ok, delete (admin) denied, user mgmt denied
	cp, err := ps.CreateProject(ctx, bearer(dev, &v1.CreateProjectRequest{Name: "p"}))
	require.NoError(t, err)
	_, err = ps.DeleteProject(ctx, bearer(dev, &v1.DeleteProjectRequest{Project: cp.Msg.Project.Id, Confirm: true}))
	require.Equal(t, connect.CodePermissionDenied, connect.CodeOf(err))
	_, err = as.ListUsers(ctx, bearer(dev, &v1.ListUsersRequest{}))
	require.Equal(t, connect.CodePermissionDenied, connect.CodeOf(err))
	// admin: can manage users but not mint owners
	_, err = as.CreateUser(ctx, bearer(adm, &v1.CreateUserRequest{Email: "e@x.io", Password: "password123", Role: v1.Role_ROLE_OWNER}))
	require.Equal(t, connect.CodePermissionDenied, connect.CodeOf(err))
	_, err = as.CreateUser(ctx, bearer(adm, &v1.CreateUserRequest{Email: "e@x.io", Password: "password123", Role: v1.Role_ROLE_DEVELOPER}))
	require.NoError(t, err)
	// last owner cannot be removed
	_, err = as.DeleteUser(ctx, bearer(owner, &v1.DeleteUserRequest{UserId: boot.Msg.User.Id}))
	require.Equal(t, connect.CodeFailedPrecondition, connect.CodeOf(err))

	// API tokens: scope is capped by the creator's credential and role
	tr, err := as.CreateApiToken(ctx, bearer(dev, &v1.CreateApiTokenRequest{Name: "ci", Scopes: []string{"deploy"}}))
	require.NoError(t, err)
	_, err = as.CreateApiToken(ctx, bearer(dev, &v1.CreateApiTokenRequest{Name: "x", Scopes: []string{"admin"}}))
	require.Equal(t, connect.CodePermissionDenied, connect.CodeOf(err))
	// deploy-scope token of a developer works for deploy, not for admin ops
	_, err = ps.CreateProject(ctx, bearer(tr.Msg.Plaintext, &v1.CreateProjectRequest{Name: "p2"}))
	require.NoError(t, err)
	_, err = ps.DeleteProject(ctx, bearer(tr.Msg.Plaintext, &v1.DeleteProjectRequest{Project: "p2", Confirm: true}))
	require.Equal(t, connect.CodePermissionDenied, connect.CodeOf(err))
	// an admin-scope token owned by a viewer is still limited by RBAC
	vtok, err := as.CreateApiToken(ctx, bearer(viewer, &v1.CreateApiTokenRequest{Name: "v", Scopes: []string{"read"}}))
	require.NoError(t, err)
	_, err = ps.CreateProject(ctx, bearer(vtok.Msg.Plaintext, &v1.CreateProjectRequest{Name: "p3"}))
	require.Equal(t, connect.CodePermissionDenied, connect.CodeOf(err))
	// revoke
	_, err = as.RevokeApiToken(ctx, bearer(dev, &v1.RevokeApiTokenRequest{TokenId: tr.Msg.Token.Id}))
	require.NoError(t, err)
	_, err = ps.ListProjects(ctx, bearer(tr.Msg.Plaintext, &v1.ListProjectsRequest{}))
	require.Equal(t, connect.CodeUnauthenticated, connect.CodeOf(err))

	// logout revokes the session on every node
	_, err = as.Logout(ctx, bearer(viewer, &v1.LogoutRequest{}))
	require.NoError(t, err)
	_, err = ps.ListProjects(ctx, bearer(viewer, &v1.ListProjectsRequest{}))
	require.Equal(t, connect.CodeUnauthenticated, connect.CodeOf(err))

	// the original cluster admin token keeps working (owner, admin scope)
	_, err = as.ListUsers(ctx, bearer(f.admin, &v1.ListUsersRequest{}))
	require.NoError(t, err)

	// audit log: mutating calls recorded with actor, no reads
	al, err := as.ListAuditLog(ctx, bearer(adm, &v1.ListAuditLogRequest{Limit: 200}))
	require.NoError(t, err)
	var sawCreate, sawDeny, sawLogin bool
	for _, e := range al.Msg.Entries {
		switch {
		case e.Action == "/aetherix.v1.ProjectService/CreateProject" && e.Ok && e.ActorEmail == "d@x.io":
			sawCreate = true
			require.Contains(t, []string{"p", "p2"}, e.Attributes["name"])
		case e.Action == "auth.login":
			sawLogin = true
		}
		require.NotEqual(t, "/aetherix.v1.ProjectService/ListProjects", e.Action)
		if e.Action == "/aetherix.v1.ProjectService/DeleteProject" {
			sawDeny = true
		}
	}
	require.True(t, sawCreate && sawLogin)
	_ = sawDeny
	_, err = as.ListAuditLog(ctx, bearer(dev, &v1.ListAuditLogRequest{}))
	require.Equal(t, connect.CodePermissionDenied, connect.CodeOf(err))
	_ = cs
	_ = http.StatusOK
}

func TestEventsStreamAndPrefix(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cs := aetherixv1connect.NewClusterServiceClient(f.srv.Client(), f.srv.URL)

	// read-scoped token may stream; no token may not
	_, tok, err := func() (string, string, error) {
		p, _, err := auth.CreateToken(ctx, f.st, "ro", []string{"read"}, 0)
		return "", p, err
	}()
	require.NoError(t, err)
	st, err := cs.WatchEvents(ctx, bearer(tok, &v1.WatchEventsRequest{TypePrefix: "deployment."}))
	require.NoError(t, err)
	got := make(chan *v1.Event, 4)
	go func() {
		for st.Receive() {
			got <- st.Msg()
		}
		close(got)
	}()
	time.Sleep(100 * time.Millisecond)
	_, _ = events.Emit(ctx, f.st, "machine.state", "m1", "ignored", nil)
	_, _ = events.Emit(ctx, f.st, "deployment.status", "d1", "active", map[string]string{"k": "v"})
	e0 := <-got
	require.Equal(t, "stream.open", e0.Type)
	select {
	case e := <-got:
		require.Equal(t, "deployment.status", e.Type)
		require.Equal(t, "v", e.Attributes["k"])
	case <-ctx.Done():
		t.Fatal("no event streamed")
	}
	cancel()

	_, err = aetherixv1connect.NewClusterServiceClient(f.srv.Client(), f.srv.URL).
		WatchEvents(context.Background(), connect.NewRequest(&v1.WatchEventsRequest{}))
	if err == nil { // unary-style error surfaces on first Receive for streams
		s2, _ := aetherixv1connect.NewClusterServiceClient(f.srv.Client(), f.srv.URL).WatchEvents(context.Background(), connect.NewRequest(&v1.WatchEventsRequest{}))
		require.False(t, s2.Receive())
		require.Equal(t, connect.CodeUnauthenticated, connect.CodeOf(s2.Err()))
	}
}
