package ingress

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "github.com/aetherix/aetherixcloud/gen/aetherix/v1"
	"github.com/aetherix/aetherixcloud/internal/lease"
	"github.com/aetherix/aetherixcloud/internal/secrets"
	"github.com/aetherix/aetherixcloud/internal/store"
	"github.com/stretchr/testify/require"
)

// fakeCaddy records /load bodies.
type fakeCaddy struct {
	mu    sync.Mutex
	loads []map[string]any
	fail  bool
	srv   *httptest.Server
}

func newFakeCaddy(t *testing.T) *fakeCaddy {
	f := &fakeCaddy{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch r.URL.Path {
		case "/load":
			if f.fail {
				http.Error(w, "boom", 500)
				return
			}
			var m map[string]any
			_ = json.NewDecoder(r.Body).Decode(&m)
			f.loads = append(f.loads, m)
		case "/config/":
			_, _ = w.Write([]byte("{}"))
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeCaddy) count() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.loads) }

func seed(t *testing.T, st store.Store) {
	ctx := context.Background()
	require.NoError(t, store.PutCluster(ctx, st, &v1.Cluster{Id: "c1", Name: "c"}))
	require.NoError(t, store.PutMachine(ctx, st, &v1.Machine{Id: "m1", Name: "n1", State: v1.MachineState_MACHINE_STATE_UP,
		Endpoints: []string{"203.0.113.7:51820"}, Roles: []v1.MachineRole{v1.MachineRole_MACHINE_ROLE_INGRESS, v1.MachineRole_MACHINE_ROLE_WORKER}}))
	require.NoError(t, store.PutMachine(ctx, st, &v1.Machine{Id: "m2", Name: "n2", State: v1.MachineState_MACHINE_STATE_UP,
		Roles: []v1.MachineRole{v1.MachineRole_MACHINE_ROLE_WORKER}}))
	require.NoError(t, store.PutProject(ctx, st, &v1.Project{Id: "p1", Name: "My App"}))
	spec := &v1.ServiceSpec{Image: "x", Ports: []*v1.ServicePort{{ContainerPort: 3000, Mode: "https"}},
		HealthCheck: &v1.HealthCheck{HttpPath: "/healthz"}}
	require.NoError(t, store.PutService(ctx, st, &v1.Service{Id: "s1", ProjectId: "p1", Name: "web", Spec: spec, ActiveDeploymentId: "d1"}))
	require.NoError(t, store.PutDeployment(ctx, st, &v1.Deployment{Id: "d1", ServiceId: "s1", Spec: spec}))
	for _, r := range []*v1.Replica{
		{Id: "r1", ServiceId: "s1", DeploymentId: "d1", MachineId: "m1", ContainerIp: "10.210.1.130", State: v1.ReplicaState_REPLICA_STATE_RUNNING, DesiredRunning: true},
		{Id: "r2", ServiceId: "s1", DeploymentId: "d1", MachineId: "m2", ContainerIp: "10.210.2.130", State: v1.ReplicaState_REPLICA_STATE_RUNNING, DesiredRunning: true},
		{Id: "r3", ServiceId: "s1", DeploymentId: "d1", MachineId: "m2", ContainerIp: "10.210.2.131", State: v1.ReplicaState_REPLICA_STATE_STARTING, DesiredRunning: true},
	} {
		require.NoError(t, store.PutReplica(ctx, st, r))
	}
}

func TestDefaultHostnames(t *testing.T) {
	require.Equal(t, "web-my-app.203.0.113.7.sslip.io", DefaultHostname("web", "My App", HostOptions{PublicIP: "203.0.113.7"}))
	require.Equal(t, "web-my-app.localhost", DefaultHostname("web", "My App", HostOptions{Dev: true, PublicIP: "1.2.3.4"}))
	require.Equal(t, "web-p.example.com", DefaultHostname("web", "p", HostOptions{ClusterDomain: "example.com.", PublicIP: "1.2.3.4"}))
	require.Equal(t, "web-p.2001-db8--1.sslip.io", DefaultHostname("web", "p", HostOptions{PublicIP: "2001:db8::1"}))
	require.Equal(t, "", DefaultHostname("web", "p", HostOptions{}))
}

func TestEnsureDomainsIdempotentAndFollowsIP(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	seed(t, st)
	n, err := EnsureDomains(ctx, st, HostOptions{PublicIP: "203.0.113.7"})
	require.NoError(t, err)
	require.Equal(t, 1, n)
	n, _ = EnsureDomains(ctx, st, HostOptions{PublicIP: "203.0.113.7"})
	require.Zero(t, n)
	ds, _ := store.ListDomains(ctx, st, "s1")
	require.Len(t, ds, 1)
	require.True(t, ds[0].Generated)
	require.Equal(t, int32(3000), ds[0].Port)
	// cluster domain configured later: same row, new hostname
	_, err = EnsureDomains(ctx, st, HostOptions{ClusterDomain: "apps.test", PublicIP: "203.0.113.7"})
	require.NoError(t, err)
	ds, _ = store.ListDomains(ctx, st, "s1")
	require.Len(t, ds, 1)
	require.Equal(t, "web-my-app.apps.test", ds[0].Hostname)
}

func TestGenerateRoutesOnlyHealthyUndrained(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	seed(t, st)
	require.NoError(t, store.PutDomain(ctx, st, &v1.Domain{Id: "dom", ServiceId: "s1", Hostname: "app.example.com"}))
	c := &Controller{Store: st, Self: "m1"}
	in, err := c.Snapshot(ctx)
	require.NoError(t, err)
	cfg, routes := Generate(in)
	require.Equal(t, []Route{{Hostname: "app.example.com", Upstreams: []string{"10.210.1.130:3000", "10.210.2.130:3000"}}}, routes)
	raw, _ := json.Marshal(cfg)
	require.Contains(t, string(raw), `"uri":"/healthz"`)
	require.Contains(t, string(raw), `"module":"acme"`)

	in.Draining = map[string]bool{"r1": true}
	_, routes = Generate(in)
	require.Equal(t, []string{"10.210.2.130:3000"}, routes[0].Upstreams)

	in.Machines["m2"].State = v1.MachineState_MACHINE_STATE_DOWN
	cfg, routes = Generate(in)
	require.Empty(t, routes[0].Upstreams)
	raw, _ = json.Marshal(cfg)
	require.Contains(t, string(raw), `"status_code":503`)
}

func TestGenerateDevAndDNS01AndStorage(t *testing.T) {
	in := Input{Domains: []*v1.Domain{{Id: "a", ServiceId: "s", Hostname: "web-p.localhost"}},
		Services: map[string]*v1.Service{}, Options: Options{Dev: true, Storage: &StorageRef{Endpoint: "http://127.0.0.1:8080/x/", Token: "tok"}}}
	cfg, _ := Generate(in)
	raw, _ := json.Marshal(cfg)
	require.Contains(t, string(raw), `"module":"internal"`)
	require.NotContains(t, string(raw), `"module":"acme"`)
	require.Contains(t, string(raw), `"module":"aetherix"`)

	in = Input{Domains: []*v1.Domain{{Id: "a", Hostname: "web.apps.test"}, {Id: "b", Hostname: "a.b.apps.test"}, {Id: "c", Hostname: "custom.io"}},
		Services: map[string]*v1.Service{},
		Options:  Options{DNS01: &DNS01{Provider: "cloudflare", APIToken: "cf", Wildcards: []string{"apps.test"}}}}
	cfg, _ = Generate(in)
	raw, _ = json.Marshal(cfg)
	s := string(raw)
	require.Contains(t, s, `"subjects":["*.apps.test"]`)
	require.Contains(t, s, `"automate":["*.apps.test"]`)
	require.Contains(t, s, `"api_token":"cf"`)
}

func TestDNSHintsAndVerify(t *testing.T) {
	h := DNSHints("app.example.com", []string{"203.0.113.7"}, "web-p.203.0.113.7.sslip.io")
	require.Contains(t, h[0], "A  203.0.113.7")
	require.Contains(t, h[1], "CNAME")
	require.Contains(t, DNSHints("x.localhost", nil, "")[0], "127.0.0.1")
	r := fakeResolver{"app.example.com": {"203.0.113.7"}, "bad.example.com": {"9.9.9.9"}}
	require.True(t, VerifyDomain(context.Background(), r, "app.example.com", []string{"203.0.113.7"}).Verified)
	res := VerifyDomain(context.Background(), r, "bad.example.com", []string{"203.0.113.7"})
	require.False(t, res.Verified)
	require.Contains(t, res.Message, "expected one of")
	require.False(t, VerifyDomain(context.Background(), r, "nx.example.com", nil).Verified)
}

type fakeResolver map[string][]string

func (f fakeResolver) LookupHost(_ context.Context, h string) ([]string, error) {
	if v, ok := f[h]; ok {
		return v, nil
	}
	return nil, &fakeErr{}
}

type fakeErr struct{}

func (*fakeErr) Error() string { return "no such host" }

func TestControllerPushesOnlyOnChange(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	seed(t, st)
	require.NoError(t, store.PutDomain(ctx, st, &v1.Domain{Id: "dom", ServiceId: "s1", Hostname: "app.example.com"}))
	fc := newFakeCaddy(t)
	c := &Controller{Store: st, Self: "m1", Admin: NewAdminClient(fc.srv.URL)}

	pushed, err := c.Reconcile(ctx)
	require.NoError(t, err)
	require.True(t, pushed)
	pushed, _ = c.Reconcile(ctx)
	require.False(t, pushed)
	require.Equal(t, 1, fc.count())

	// a replica becomes healthy -> new push
	r, _ := store.GetReplica(ctx, st, "r3")
	r.State = v1.ReplicaState_REPLICA_STATE_RUNNING
	require.NoError(t, store.PutReplica(ctx, st, r))
	pushed, _ = c.Reconcile(ctx)
	require.True(t, pushed)
	require.Equal(t, 2, fc.count())

	// failed push is retried
	fc.mu.Lock()
	fc.fail = true
	fc.mu.Unlock()
	_, err = MarkDraining(ctx, st, "r1", time.Now())
	require.NoError(t, err)
	_, err = c.Reconcile(ctx)
	require.Error(t, err)
	fc.mu.Lock()
	fc.fail = false
	fc.mu.Unlock()
	pushed, err = c.Reconcile(ctx)
	require.NoError(t, err)
	require.True(t, pushed)
	require.NotContains(t, c.Routes()[0].Upstreams, "10.210.1.130:3000")

	// Caddy restart
	c.Invalidate()
	pushed, _ = c.Reconcile(ctx)
	require.True(t, pushed)
}

func TestNonIngressMachineDoesNotPush(t *testing.T) {
	st := store.NewMemory()
	seed(t, st)
	fc := newFakeCaddy(t)
	c := &Controller{Store: st, Self: "m2", Admin: NewAdminClient(fc.srv.URL)}
	pushed, err := c.Reconcile(context.Background())
	require.NoError(t, err)
	require.False(t, pushed)
	require.Zero(t, fc.count())
}

func TestDrainAckFlow(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	seed(t, st)
	require.NoError(t, store.PutDomain(ctx, st, &v1.Domain{Id: "dom", ServiceId: "s1", Hostname: "app.example.com"}))
	fc := newFakeCaddy(t)
	c := &Controller{Store: st, Self: "m1", Admin: NewAdminClient(fc.srv.URL)}
	_, err := c.Reconcile(ctx)
	require.NoError(t, err)

	mark, err := MarkDraining(ctx, st, "r1", time.Now())
	require.NoError(t, err)
	ok, err := DrainApplied(ctx, st, mark)
	require.NoError(t, err)
	require.False(t, ok, "ingress has not applied the drain yet")
	pushed, err := c.Reconcile(ctx)
	require.NoError(t, err)
	require.True(t, pushed)
	ok, _ = DrainApplied(ctx, st, mark)
	require.True(t, ok)

	// a drain that does not change the config is still acknowledged
	m3, _ := MarkDraining(ctx, st, "r3", time.Now())
	pushed, err = c.Reconcile(ctx)
	require.NoError(t, err)
	require.False(t, pushed)
	ok, _ = DrainApplied(ctx, st, m3)
	require.True(t, ok)

	// idempotent mark
	m2, _ := MarkDraining(ctx, st, "r1", time.Now().Add(time.Hour))
	require.Equal(t, mark.At, m2.At)
}

func TestDefaultDomainsAndCertState(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	seed(t, st)
	key, _ := secrets.NewKey()
	stg := &Storage{Store: st, Key: key, Leases: &lease.Manager{Store: st, Self: "m1"}}
	c := &Controller{Store: st, Self: "m1", Storage: stg}
	require.NoError(t, c.MaintainDomains(ctx))
	ds, _ := store.ListDomains(ctx, st, "s1")
	require.Len(t, ds, 1)
	require.Equal(t, "web-my-app.203.0.113.7.sslip.io", ds[0].Hostname)
	require.Equal(t, "pending", ds[0].TlsState)

	require.NoError(t, stg.Put(ctx, "certificates/acme-v02/"+ds[0].Hostname+"/"+ds[0].Hostname+".crt", selfSigned(t, ds[0].Hostname, time.Now().Add(48*time.Hour))))
	require.NoError(t, c.MaintainDomains(ctx))
	ds, _ = store.ListDomains(ctx, st, "s1")
	require.Equal(t, "active", ds[0].TlsState)
	require.NotNil(t, ds[0].TlsExpiresAt)
}

func newStorage(t *testing.T, self string, st store.Store) *Storage {
	key, err := secrets.ParseKey(testKey)
	require.NoError(t, err)
	return &Storage{Store: st, Key: key, Leases: &lease.Manager{Store: st, Self: self}}
}

var testKey = func() string { k, _ := secrets.NewKey(); return k.String() }()

func TestStorageSemantics(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	s := newStorage(t, "m1", st)
	_, err := s.Get(ctx, "a/b")
	require.ErrorIs(t, err, ErrKeyNotFound)
	require.NoError(t, s.Put(ctx, "certificates/le/x.com/x.com.crt", []byte("CERT")))
	require.NoError(t, s.Put(ctx, "certificates/le/x.com/x.com.key", []byte("KEY")))
	require.NoError(t, s.Put(ctx, "certificates/le/y.com/y.com.crt", []byte("CERT2")))
	v, err := s.Get(ctx, "certificates/le/x.com/x.com.key")
	require.NoError(t, err)
	require.Equal(t, "KEY", string(v))
	// sealed at rest
	rec, _ := st.Get(ctx, store.TableSettings, "caddy/certificates/le/x.com/x.com.key")
	require.NotContains(t, string(rec.Data), "KEY\"")
	require.NotContains(t, string(rec.Data), `"value":"KEY"`)

	kids, _ := s.List(ctx, "certificates/le", false)
	require.Equal(t, []string{"certificates/le/x.com", "certificates/le/y.com"}, kids)
	all, _ := s.List(ctx, "certificates/le/x.com", true)
	require.Len(t, all, 2)
	ok, _ := s.Exists(ctx, "certificates/le/x.com/x.com.crt")
	require.True(t, ok)
	ki, err := s.Stat(ctx, "certificates/le/x.com/x.com.crt")
	require.NoError(t, err)
	require.True(t, ki.IsTerminal)
	require.EqualValues(t, 4, ki.Size)
	ki, err = s.Stat(ctx, "certificates/le/x.com")
	require.NoError(t, err)
	require.False(t, ki.IsTerminal)

	require.NoError(t, s.Delete(ctx, "certificates/le/x.com")) // recursive
	ok, _ = s.Exists(ctx, "certificates/le/x.com/x.com.crt")
	require.False(t, ok)
	ok, _ = s.Exists(ctx, "certificates/le/y.com/y.com.crt")
	require.True(t, ok)
	require.ErrorIs(t, s.Delete(ctx, "nope"), ErrKeyNotFound)
}

func TestStorageLocksAcrossNodes(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory() // shared replicated store
	a, b := newStorage(t, "m1", st), newStorage(t, "m2", st)
	ok, err := a.Lock(ctx, "issue_cert_x.com", "o1")
	require.NoError(t, err)
	require.True(t, ok)
	ok, _ = a.Lock(ctx, "issue_cert_x.com", "o2") // same node, other owner
	require.False(t, ok)
	ok, _ = b.Lock(ctx, "issue_cert_x.com", "o3") // other node
	require.False(t, ok)
	ok, _ = a.Lock(ctx, "issue_cert_x.com", "o1") // renew
	require.True(t, ok)
	require.NoError(t, a.Unlock(ctx, "issue_cert_x.com", "o2")) // wrong owner: no-op
	ok, _ = b.Lock(ctx, "issue_cert_x.com", "o3")
	require.False(t, ok)
	require.NoError(t, a.Unlock(ctx, "issue_cert_x.com", "o1"))
	ok, _ = b.Lock(ctx, "issue_cert_x.com", "o3")
	require.True(t, ok)
}

func TestStorageHTTP(t *testing.T) {
	st := store.NewMemory()
	s := newStorage(t, "m1", st)
	h := &StorageHandler{}
	srv := httptest.NewServer(h)
	defer srv.Close()
	call := func(op, token string, body any) *http.Response {
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest(http.MethodPost, srv.URL+StoragePath+op, bytes.NewReader(b))
		req.URL.Path = StoragePath + op
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		return resp
	}
	require.Equal(t, 503, call("load", "", storageReq{Key: "k"}).StatusCode)
	h.Bind(s)
	require.Equal(t, 401, call("load", "wrong", storageReq{Key: "k"}).StatusCode)
	tok := StorageToken(s.Key)
	require.Equal(t, 404, call("load", tok, storageReq{Key: "k"}).StatusCode)
	require.Equal(t, 200, call("store", tok, storageReq{Key: "k", Value: []byte("v")}).StatusCode)
	resp := call("load", tok, storageReq{Key: "k"})
	var out struct{ Value []byte }
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	require.Equal(t, "v", string(out.Value))
	require.Equal(t, 200, call("lock", tok, storageReq{Name: "n", Owner: "a"}).StatusCode)
	require.Equal(t, 409, call("lock", tok, storageReq{Name: "n", Owner: "b"}).StatusCode)
	require.Equal(t, 200, call("unlock", tok, storageReq{Name: "n", Owner: "a"}).StatusCode)
	require.Equal(t, 200, call("lock", tok, storageReq{Name: "n", Owner: "b"}).StatusCode)
	require.True(t, strings.HasPrefix(StoragePath, "/"))
}

func TestOptionsFromEnv(t *testing.T) {
	env := map[string]string{"AX_DEV": "1", "AX_DNS_CLOUDFLARE_TOKEN": "t", "AX_ACME_EMAIL": "a@b.c"}
	o := OptionsFromEnv(func(k string) string { return env[k] }, "apps.test")
	require.True(t, o.Dev)
	require.Equal(t, []string{"apps.test"}, o.DNS01.Wildcards)
	require.Equal(t, "a@b.c", o.ACMEEmail)
}
