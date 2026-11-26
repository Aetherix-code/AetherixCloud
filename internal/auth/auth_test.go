package auth_test

import (
	"context"
	"strings"
	"testing"
	"time"

	v1 "github.com/aetherix/aetherixcloud/gen/aetherix/v1"
	"github.com/aetherix/aetherixcloud/internal/auth"
	"github.com/aetherix/aetherixcloud/internal/secrets"
	"github.com/aetherix/aetherixcloud/internal/store"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
)

func TestPasswordAndJWT(t *testing.T) {
	h, err := auth.HashPassword("hunter2hunter2")
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(h, "$argon2id$"))
	require.True(t, auth.VerifyPassword("hunter2hunter2", h))
	require.False(t, auth.VerifyPassword("hunter3hunter2", h))
	require.False(t, auth.VerifyPassword("x", "garbage"))

	k, _ := secrets.NewKey()
	k2, _ := secrets.NewKey()
	sg, _ := auth.NewSigner(k.String())
	sg2, _ := auth.NewSigner(k2.String())
	tok := sg.Sign(auth.Claims{Sub: "u", Typ: auth.KindSession, Exp: time.Now().Add(time.Hour).Unix()})
	c, err := sg.Parse(tok)
	require.NoError(t, err)
	require.Equal(t, "u", c.Sub)
	_, err = sg2.Parse(tok)
	require.ErrorIs(t, err, auth.ErrBadJWT)
	_, err = sg.Parse(sg.Sign(auth.Claims{Sub: "u", Exp: time.Now().Add(-time.Second).Unix()}))
	require.ErrorIs(t, err, auth.ErrBadJWT)
	parts := strings.Split(tok, ".")
	_, err = sg.Parse(parts[0] + "." + parts[1] + "x." + parts[2])
	require.ErrorIs(t, err, auth.ErrBadJWT)
	_, err = sg.Parse("a.b.c")
	require.ErrorIs(t, err, auth.ErrBadJWT)
}

// Every RPC of every service has an explicit, sane rule: new RPCs added by
// other workers must not silently become public.
func TestPolicyCoversAllProcedures(t *testing.T) {
	n := 0
	protoregistry.GlobalFiles.RangeFilesByPackage("aetherix.v1", func(fd protoreflect.FileDescriptor) bool {
		svcs := fd.Services()
		for i := 0; i < svcs.Len(); i++ {
			s := svcs.Get(i)
			for j := 0; j < s.Methods().Len(); j++ {
				proc := "/aetherix.v1." + string(s.Name()) + "/" + string(s.Methods().Get(j).Name())
				r := auth.RuleFor(proc)
				n++
				if r.Access == auth.AccessPublic {
					require.Contains(t, []string{
						"/aetherix.v1.AuthService/Login", "/aetherix.v1.AuthService/GetSetupStatus",
						"/aetherix.v1.AuthService/Bootstrap", "/aetherix.v1.MachineService/JoinMachine"}, proc)
				}
			}
		}
		return true
	})
	require.Greater(t, n, 50)
	require.Equal(t, auth.LevelRead, auth.RuleFor("/aetherix.v1.ServiceService/ListServices").Level)
	require.Equal(t, auth.LevelDeploy, auth.RuleFor("/aetherix.v1.ServiceService/ScaleService").Level)
	require.Equal(t, auth.LevelDeploy, auth.RuleFor("/aetherix.v1.ServiceService/Exec").Level)
	require.Equal(t, auth.LevelAdmin, auth.RuleFor("/aetherix.v1.ServiceService/DeleteService").Level)
	require.Equal(t, auth.LevelAdmin, auth.RuleFor("/aetherix.v1.ClusterService/Backup").Level)
	require.Equal(t, auth.LevelAdmin, auth.RuleFor("/aetherix.v1.ClusterService/Restore").Level)
	require.Equal(t, auth.LevelAdmin, auth.RuleFor("/aetherix.v1.MachineService/HostExec").Level)
	require.Equal(t, auth.LevelAdmin, auth.RuleFor("/aetherix.v1.GitHubService/SetWebhookUrl").Level)
	require.Equal(t, auth.LevelAdmin, auth.RuleFor("/aetherix.v1.GitHubService/DisconnectApp").Level)
	require.Equal(t, auth.LevelAdmin, auth.RuleFor("/aetherix.v1.AuthService/SomethingNew").Level)
}

func TestPrincipalLevels(t *testing.T) {
	p := &auth.Principal{Role: v1.Role_ROLE_ADMIN, Scopes: []string{"read"}}
	require.True(t, p.Allows(auth.LevelRead))
	require.False(t, p.Allows(auth.LevelDeploy)) // scope caps the role
	p = &auth.Principal{Role: v1.Role_ROLE_VIEWER, Scopes: []string{"admin"}}
	require.False(t, p.Allows(auth.LevelDeploy)) // role caps the scope
	p = &auth.Principal{Role: v1.Role_ROLE_DEVELOPER, Scopes: []string{"deploy"}}
	require.True(t, p.Allows(auth.LevelDeploy))
	require.False(t, p.Allows(auth.LevelAdmin))
}

func TestUsersAndRateLimit(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	u, err := auth.CreateUser(ctx, st, " A@B.io ", "", "password1", v1.Role_ROLE_OWNER)
	require.NoError(t, err)
	require.Equal(t, "a@b.io", u.Email)
	_, err = auth.CreateUser(ctx, st, "a@b.io", "", "password1", v1.Role_ROLE_VIEWER)
	require.ErrorIs(t, err, auth.ErrUserExists)
	_, err = auth.CreateUser(ctx, st, "c@b.io", "", "short", v1.Role_ROLE_VIEWER)
	require.Error(t, err)
	_, err = auth.CheckPassword(ctx, st, "nobody@b.io", "password1")
	require.ErrorIs(t, err, auth.ErrBadLogin)
	_, err = auth.CheckPassword(ctx, st, "a@b.io", "password1")
	require.NoError(t, err)
	viewer := v1.Role_ROLE_VIEWER
	_, err = auth.UpdateUser(ctx, st, u.Id, nil, &viewer, nil)
	require.ErrorIs(t, err, auth.ErrLastOwner)

	now := time.Now()
	rl := auth.NewRateLimiter(60, 3)
	rl.Now = func() time.Time { return now }
	for i := 0; i < 3; i++ {
		ok, _ := rl.Allow("k")
		require.True(t, ok)
	}
	ok, wait := rl.Allow("k")
	require.False(t, ok)
	require.Greater(t, wait, time.Duration(0))
	ok, _ = rl.Allow("other")
	require.True(t, ok)
	now = now.Add(2 * time.Second)
	ok, _ = rl.Allow("k")
	require.True(t, ok)
}
