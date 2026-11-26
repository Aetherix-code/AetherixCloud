package cluster

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"connectrpc.com/connect"
	v1 "github.com/aetherix/aetherixcloud/gen/aetherix/v1"
	"github.com/aetherix/aetherixcloud/gen/aetherix/v1/aetherixv1connect"
	"github.com/aetherix/aetherixcloud/internal/ids"
	"github.com/aetherix/aetherixcloud/internal/lease"
	"github.com/aetherix/aetherixcloud/internal/mesh"
	"github.com/aetherix/aetherixcloud/internal/pki"
	"github.com/aetherix/aetherixcloud/internal/store"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// TokenPrefix identifies join tokens.
const TokenPrefix = "axjoin1."

// DefaultTokenTTL is how long join tokens stay valid.
const DefaultTokenTTL = time.Hour

// JoinToken is the decoded token a new machine presents.
type JoinToken struct {
	V         int      `json:"v"`
	ID        string   `json:"id"`
	Secret    string   `json:"s"`
	ClusterID string   `json:"c"`
	CAPEM     string   `json:"ca"` // pinned CA: the joiner trusts only this
	Endpoints []string `json:"e"`  // API base URLs of existing machines
	Expires   int64    `json:"x"`
}

// Encode returns the opaque token string.
func (t JoinToken) Encode() string {
	b, _ := json.Marshal(t)
	return TokenPrefix + base64.RawURLEncoding.EncodeToString(b)
}

// ParseJoinToken decodes a token string.
func ParseJoinToken(s string) (JoinToken, error) {
	var t JoinToken
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, TokenPrefix) {
		return t, errors.New("join: malformed token (missing prefix)")
	}
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(s, TokenPrefix))
	if err != nil {
		return t, errors.New("join: malformed token")
	}
	if err := json.Unmarshal(b, &t); err != nil || t.V != 1 || t.ID == "" || t.Secret == "" {
		return t, errors.New("join: malformed token")
	}
	return t, nil
}

// tokenRow is the persisted token record (join_tokens.data); the secret is
// stored only as a hash.
type tokenRow struct {
	ID          string   `json:"id"`
	Expires     int64    `json:"expires"`
	Roles       []string `json:"roles,omitempty"`
	MachineName string   `json:"machine_name,omitempty"`
	UsedBy      string   `json:"used_by,omitempty"`
	UsedAt      int64    `json:"used_at,omitempty"`
}

func hashSecret(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// Errors returned by Join.
var (
	ErrTokenInvalid = errors.New("join: invalid or unknown token")
	ErrTokenExpired = errors.New("join: token expired")
	ErrTokenUsed    = errors.New("join: token already used")
)

// Server implements the introducer side of the join protocol.
type Server struct {
	Store  store.Store
	Leases *lease.Manager
	State  *NodeState
	Now    func() time.Time
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// CreateToken mints a one-time join token.
func (s *Server) CreateToken(ctx context.Context, ttl time.Duration, roles []v1.MachineRole, machineName string) (string, time.Time, error) {
	if ttl <= 0 {
		ttl = DefaultTokenTTL
	}
	sec := make([]byte, 24)
	_, _ = rand.Read(sec)
	exp := s.now().Add(ttl)
	t := JoinToken{V: 1, ID: ids.New(), Secret: base64.RawURLEncoding.EncodeToString(sec), ClusterID: s.State.ClusterID,
		CAPEM: s.State.CACertPEM, Expires: exp.Unix()}
	for _, h := range s.State.Advertise {
		t.Endpoints = append(t.Endpoints, fmt.Sprintf("https://%s", net.JoinHostPort(h, fmt.Sprint(s.State.APIPort))))
	}
	row := tokenRow{ID: t.ID, Expires: exp.Unix(), MachineName: machineName}
	for _, r := range roles {
		row.Roles = append(row.Roles, r.String())
	}
	if err := store.PutJSON(ctx, s.Store, store.TableJoinTokens, t.ID, map[string]string{"token_hash": hashSecret(t.Secret)}, row); err != nil {
		return "", time.Time{}, err
	}
	return t.Encode(), exp, nil
}

// Join validates the token and registers the machine. remoteIP is the
// caller's address, used as the endpoint when none is supplied.
func (s *Server) Join(ctx context.Context, req *v1.JoinMachineRequest, remoteIP string) (*v1.JoinMachineResponse, error) {
	tok, err := ParseJoinToken(req.Token)
	if err != nil {
		return nil, ErrTokenInvalid
	}
	if req.PublicKey == "" {
		return nil, errors.New("join: public_key is required")
	}
	rec, err := s.Store.Get(ctx, store.TableJoinTokens, tok.ID)
	if err != nil {
		return nil, ErrTokenInvalid
	}
	if rec.Idx["token_hash"] != hashSecret(tok.Secret) {
		return nil, ErrTokenInvalid
	}
	var row tokenRow
	if err := json.Unmarshal(rec.Data, &row); err != nil {
		return nil, err
	}
	if s.now().Unix() > row.Expires {
		return nil, ErrTokenExpired
	}
	if row.UsedBy != "" {
		return nil, ErrTokenUsed
	}

	// Serialise subnet allocation/token consumption across the cluster.
	l, err := s.Leases.Acquire(ctx, "membership", 30*time.Second)
	if err != nil {
		return nil, fmt.Errorf("join: another join is in progress, retry: %w", err)
	}
	defer l.Release(context.WithoutCancel(ctx))

	// Re-check the token under the lease.
	if rec, err = s.Store.Get(ctx, store.TableJoinTokens, tok.ID); err != nil {
		return nil, ErrTokenInvalid
	}
	_ = json.Unmarshal(rec.Data, &row)
	if row.UsedBy != "" {
		return nil, ErrTokenUsed
	}

	machines, err := store.ListMachines(ctx, s.Store)
	if err != nil {
		return nil, err
	}
	var used []netip.Prefix
	names := map[string]bool{}
	for _, m := range machines {
		if p, err := netip.ParsePrefix(m.Subnet); err == nil {
			used = append(used, p)
		}
		names[m.Name] = true
		if m.PublicKey == req.PublicKey {
			return nil, errors.New("join: machine with this public key already exists")
		}
	}
	network, _ := netip.ParsePrefix(s.State.Network)
	subnet, err := mesh.AllocateSubnet(network, used)
	if err != nil {
		return nil, err
	}
	name := req.Name
	if name == "" {
		name = row.MachineName
	}
	if name == "" {
		for i := len(machines) + 1; ; i++ {
			if n := fmt.Sprintf("node-%d", i); !names[n] {
				name = n
				break
			}
		}
	}
	if names[name] {
		return nil, fmt.Errorf("join: machine name %q already in use", name)
	}
	endpoints := req.Endpoints
	if len(endpoints) == 0 && remoteIP != "" {
		endpoints = []string{net.JoinHostPort(remoteIP, fmt.Sprint(mesh.DefaultPort))}
	}
	apiPort := req.ApiPort
	if apiPort == 0 {
		apiPort = int32(DefaultAPIPort)
	}
	now := timestamppb.New(s.now())
	m := &v1.Machine{
		Id: ids.New(), Name: name, PublicKey: req.PublicKey, Endpoints: endpoints,
		Subnet: subnet.String(), MeshIp: mesh.MeshIP(subnet).String(), Labels: req.Labels,
		Capacity: req.Capacity, State: v1.MachineState_MACHINE_STATE_UP, ApiPort: apiPort,
		CreatedAt: now, LastSeen: now,
	}
	for _, r := range row.Roles {
		if v, ok := v1.MachineRole_value[r]; ok {
			m.Roles = append(m.Roles, v1.MachineRole(v))
		}
	}
	if len(m.Roles) == 0 {
		m.Roles = []v1.MachineRole{v1.MachineRole_MACHINE_ROLE_WORKER, v1.MachineRole_MACHINE_ROLE_INGRESS, v1.MachineRole_MACHINE_ROLE_BUILDER}
	}
	if err := store.PutMachine(ctx, s.Store, m); err != nil {
		return nil, err
	}
	row.UsedBy, row.UsedAt = m.Id, s.now().Unix()
	if err := store.PutJSON(ctx, s.Store, store.TableJoinTokens, tok.ID, map[string]string{"token_hash": rec.Idx["token_hash"]}, row); err != nil {
		return nil, err
	}
	_ = store.PutEvent(ctx, s.Store, &v1.Event{Id: ids.New(), Type: "machine.joined", SubjectId: m.Id,
		Message: fmt.Sprintf("machine %s joined the cluster", m.Name), Time: now})

	cl, err := store.GetCluster(ctx, s.Store)
	if err != nil {
		return nil, err
	}
	resp := &v1.JoinMachineResponse{Machine: m, Cluster: cl}
	for _, p := range machines {
		resp.Peers = append(resp.Peers, &v1.PeerInfo{MachineId: p.Id, Name: p.Name, PublicKey: p.PublicKey,
			Endpoints: p.Endpoints, Subnet: p.Subnet, MeshIp: p.MeshIp})
		resp.StoreBootstrap = append(resp.StoreBootstrap, fmt.Sprintf("%s:%d", p.MeshIp, DefaultCorrosionGossip))
	}
	secretsBlob, _ := json.Marshal(sealedSecrets{ClusterKey: s.State.ClusterKey, CAKeyPEM: s.State.CAKeyPEM})
	resp.ClusterKey = base64.StdEncoding.EncodeToString(secretsBlob)
	return resp, nil
}

// sealedSecrets travels inside JoinMachineResponse.cluster_key (over the
// CA-pinned TLS channel).
type sealedSecrets struct {
	ClusterKey string `json:"cluster_key"`
	CAKeyPEM   string `json:"ca_key"`
}

// JoinOptions configures the joiner side.
type JoinOptions struct {
	Token     string
	Name      string
	Advertise []string // public hosts/IPs of this machine
	WGPort    int
	APIPort   int
	Labels    map[string]string
	Capacity  *v1.Resources
	// HTTPClient overrides the pinned client (tests).
	HTTPClient *http.Client
}

// Join registers this machine with the cluster using a token and returns the
// node state to persist.
func Join(ctx context.Context, opts JoinOptions) (*NodeState, error) {
	tok, err := ParseJoinToken(opts.Token)
	if err != nil {
		return nil, err
	}
	if time.Now().Unix() > tok.Expires {
		return nil, ErrTokenExpired
	}
	if opts.WGPort == 0 {
		opts.WGPort = mesh.DefaultPort
	}
	if opts.APIPort == 0 {
		opts.APIPort = DefaultAPIPort
	}
	kp, err := mesh.GenerateKeyPair()
	if err != nil {
		return nil, err
	}
	hc := opts.HTTPClient
	if hc == nil {
		tlsCfg, err := pki.ClientTLS([]byte(tok.CAPEM))
		if err != nil {
			return nil, err
		}
		hc = &http.Client{Timeout: 60 * time.Second, Transport: &http.Transport{TLSClientConfig: tlsCfg}}
	}
	req := &v1.JoinMachineRequest{Token: opts.Token, Name: opts.Name, PublicKey: kp.Public,
		Capacity: opts.Capacity, Labels: opts.Labels, ApiPort: int32(opts.APIPort)}
	for _, h := range opts.Advertise {
		req.Endpoints = append(req.Endpoints, fmt.Sprintf("%s:%d", hostPort(h), opts.WGPort))
	}
	var resp *v1.JoinMachineResponse
	var lastErr error
	for _, ep := range tok.Endpoints {
		c := aetherixv1connect.NewMachineServiceClient(hc, ep)
		r, err := c.JoinMachine(ctx, connect.NewRequest(req))
		if err == nil {
			resp = r.Msg
			break
		}
		lastErr = fmt.Errorf("%s: %w", ep, err)
		if connect.CodeOf(err) != connect.CodeUnavailable && connect.CodeOf(err) != connect.CodeUnknown {
			break // definitive answer (bad token etc.)
		}
	}
	if resp == nil {
		return nil, fmt.Errorf("join failed: %w", lastErr)
	}
	blob, err := base64.StdEncoding.DecodeString(resp.ClusterKey)
	if err != nil {
		return nil, err
	}
	var sec sealedSecrets
	if err := json.Unmarshal(blob, &sec); err != nil {
		return nil, err
	}
	m := resp.Machine
	st := &NodeState{
		ClusterID: resp.Cluster.Id, ClusterName: resp.Cluster.Name, MachineID: m.Id, MachineName: m.Name,
		Subnet: m.Subnet, MeshIP: m.MeshIp, Network: resp.Cluster.Network,
		WGPrivateKey: kp.Private, WGPublicKey: kp.Public, WGPort: opts.WGPort,
		ClusterKey: sec.ClusterKey, CACertPEM: resp.Cluster.CaPem, CAKeyPEM: sec.CAKeyPEM,
		Advertise: opts.Advertise, APIPort: opts.APIPort, Labels: opts.Labels, StoreBootstrap: resp.StoreBootstrap,
	}
	for _, r := range m.Roles {
		st.Roles = append(st.Roles, r.String())
	}
	for _, p := range resp.Peers {
		st.Peers = append(st.Peers, PeerSeed{MachineID: p.MachineId, Name: p.Name, PublicKey: p.PublicKey,
			Endpoints: p.Endpoints, Subnet: p.Subnet, MeshIP: p.MeshIp})
	}
	return st, nil
}
