package cluster

import (
	"context"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"time"

	v1 "github.com/aetherix/aetherixcloud/gen/aetherix/v1"
	"github.com/aetherix/aetherixcloud/internal/auth"
	"github.com/aetherix/aetherixcloud/internal/ids"
	"github.com/aetherix/aetherixcloud/internal/mesh"
	"github.com/aetherix/aetherixcloud/internal/pki"
	"github.com/aetherix/aetherixcloud/internal/secrets"
	"github.com/aetherix/aetherixcloud/internal/snapshot"
	"github.com/aetherix/aetherixcloud/internal/store"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// InitOptions configures a new cluster.
type InitOptions struct {
	ClusterName string
	MachineName string
	// Advertise are public hosts/IPs of this machine (SANs + WG endpoints).
	Advertise []string
	WGPort    int
	APIPort   int
	Roles     []v1.MachineRole
	Labels    map[string]string
	Network   string // default mesh.NetworkCIDR
	Domain    string
}

// Init creates a brand-new cluster identity for this machine: CA, cluster
// key, WireGuard keys, first subnet and the initial admin API token. The
// returned plaintext token is shown once. Rows are written to the store on
// first daemon start (Bootstrap.Apply).
func Init(opts InitOptions) (*NodeState, string, error) {
	if opts.ClusterName == "" {
		opts.ClusterName = "aetherix"
	}
	if opts.MachineName == "" {
		opts.MachineName = "node-1"
	}
	if opts.WGPort == 0 {
		opts.WGPort = mesh.DefaultPort
	}
	if opts.APIPort == 0 {
		opts.APIPort = DefaultAPIPort
	}
	network := mesh.Network()
	if opts.Network != "" {
		p, err := netip.ParsePrefix(opts.Network)
		if err != nil {
			return nil, "", fmt.Errorf("network: %w", err)
		}
		network = p
	}
	ca, err := pki.NewCA(opts.ClusterName)
	if err != nil {
		return nil, "", err
	}
	ck, err := secrets.NewKey()
	if err != nil {
		return nil, "", err
	}
	kp, err := mesh.GenerateKeyPair()
	if err != nil {
		return nil, "", err
	}
	subnet, err := mesh.AllocateSubnet(network, nil)
	if err != nil {
		return nil, "", err
	}
	st := &NodeState{
		ClusterID: ids.New(), ClusterName: opts.ClusterName,
		MachineID: ids.New(), MachineName: opts.MachineName,
		Subnet: subnet.String(), MeshIP: mesh.MeshIP(subnet).String(), Network: network.String(),
		WGPrivateKey: kp.Private, WGPublicKey: kp.Public, WGPort: opts.WGPort,
		ClusterKey: ck.String(), CACertPEM: string(ca.CertPEM), CAKeyPEM: string(ca.KeyPEM),
		Advertise: opts.Advertise, APIPort: opts.APIPort, Labels: opts.Labels,
	}
	roles := opts.Roles
	if len(roles) == 0 {
		roles = []v1.MachineRole{v1.MachineRole_MACHINE_ROLE_WORKER, v1.MachineRole_MACHINE_ROLE_INGRESS, v1.MachineRole_MACHINE_ROLE_BUILDER}
	}
	for _, r := range roles {
		st.Roles = append(st.Roles, r.String())
	}
	now := timestamppb.Now()
	cl := &v1.Cluster{Id: st.ClusterID, Name: opts.ClusterName, CaPem: string(ca.CertPEM), Network: network.String(), Domain: opts.Domain, CreatedAt: now}
	m := st.Machine(now)
	plain, tok, hash := auth.NewTokenRecord("initial admin", []string{auth.ScopeAdmin}, 0)
	b := &Bootstrap{AdminTokenHash: hash}
	mo := protojson.MarshalOptions{UseProtoNames: true}
	cj, _ := mo.Marshal(cl)
	mj, _ := mo.Marshal(m)
	tj, _ := mo.Marshal(tok)
	b.ClusterJSON, b.MachineJSON, b.AdminTokenRow = string(cj), string(mj), string(tj)
	st.Bootstrap = b
	return st, plain, nil
}

// Endpoints returns the WireGuard endpoint candidates of this machine.
func (s *NodeState) Endpoints() []string {
	var out []string
	for _, h := range s.Advertise {
		out = append(out, fmt.Sprintf("%s:%d", hostPort(h), s.WGPort))
	}
	return out
}

func hostPort(h string) string {
	if a, err := netip.ParseAddr(h); err == nil && a.Is6() {
		return "[" + h + "]"
	}
	return h
}

// Machine builds the machine row for this node.
func (s *NodeState) Machine(now *timestamppb.Timestamp) *v1.Machine {
	m := &v1.Machine{
		Id: s.MachineID, Name: s.MachineName, PublicKey: s.WGPublicKey, Endpoints: s.Endpoints(),
		Subnet: s.Subnet, MeshIp: s.MeshIP, Labels: s.Labels, State: v1.MachineState_MACHINE_STATE_UP,
		ApiPort: int32(s.APIPort), CreatedAt: now, LastSeen: now,
	}
	for _, r := range s.Roles {
		if v, ok := v1.MachineRole_value[r]; ok {
			m.Roles = append(m.Roles, v1.MachineRole(v))
		}
	}
	return m
}

// ApplyBootstrap writes the pending init rows to the store (idempotent) and
// clears them from the state. The caller must Save the state afterwards.
func (s *NodeState) ApplyBootstrap(ctx context.Context, st store.Store) error {
	b := s.Bootstrap
	if b == nil {
		return nil
	}
	var cl v1.Cluster
	var m v1.Machine
	var tok v1.ApiToken
	uo := protojson.UnmarshalOptions{DiscardUnknown: true}
	if err := uo.Unmarshal([]byte(b.ClusterJSON), &cl); err != nil {
		return err
	}
	if err := uo.Unmarshal([]byte(b.MachineJSON), &m); err != nil {
		return err
	}
	if err := uo.Unmarshal([]byte(b.AdminTokenRow), &tok); err != nil {
		return err
	}
	if err := store.PutCluster(ctx, st, &cl); err != nil {
		return err
	}
	m.LastSeen = timestamppb.New(time.Now())
	if err := store.PutMachine(ctx, st, &m); err != nil {
		return err
	}
	if err := store.PutProto(ctx, st, store.TableAPITokens, tok.Id,
		map[string]string{"user_id": tok.UserId, "token_hash": b.AdminTokenHash}, &tok); err != nil {
		return err
	}
	s.Bootstrap = nil
	return nil
}

// InitFromBackup initialises a recovery node from a version-2 snapshot that
// embeds identity secrets. The snapshot is copied to dataDir/pending-restore.snapshot
// and applied on first daemon start. A new machine identity (WireGuard keys,
// subnet) is minted; peer machines from the snapshot are cleared on restore
// and must rejoin.
func InitFromBackup(dataDir, backupPath string, opts InitOptions) (*NodeState, string, error) {
	raw, err := os.ReadFile(backupPath)
	if err != nil {
		return nil, "", fmt.Errorf("read backup: %w", err)
	}
	snap, err := snapshot.Decode(raw)
	if err != nil {
		return nil, "", err
	}
	if snap.Identity == nil || snap.Identity.ClusterKey == "" || snap.Identity.CAKeyPEM == "" || snap.Identity.CACertPEM == "" {
		return nil, "", fmt.Errorf("snapshot lacks identity secrets (re-download with aetherixd/ax that embeds CA + cluster key, version ≥ 2)")
	}
	uo := protojson.UnmarshalOptions{DiscardUnknown: true}
	mo := protojson.MarshalOptions{UseProtoNames: true}
	if opts.ClusterName == "" {
		if rows := snap.Tables[store.TableCluster]; len(rows) > 0 {
			var cl v1.Cluster
			if uo.Unmarshal([]byte(rows[0].Data), &cl) == nil && cl.Name != "" {
				opts.ClusterName = cl.Name
			}
		}
	}
	st, token, err := Init(opts)
	if err != nil {
		return nil, "", err
	}
	st.ClusterID = snap.Identity.ClusterID
	if st.ClusterID == "" {
		st.ClusterID = ids.New()
	}
	st.ClusterKey = snap.Identity.ClusterKey
	st.CACertPEM = snap.Identity.CACertPEM
	st.CAKeyPEM = snap.Identity.CAKeyPEM
	// Refresh bootstrap cluster row so CaPem / id match recovered identity.
	if st.Bootstrap != nil {
		var cl v1.Cluster
		_ = uo.Unmarshal([]byte(st.Bootstrap.ClusterJSON), &cl)
		cl.Id = st.ClusterID
		cl.CaPem = st.CACertPEM
		if b, err := mo.Marshal(&cl); err == nil {
			st.Bootstrap.ClusterJSON = string(b)
		}
		var m v1.Machine
		_ = uo.Unmarshal([]byte(st.Bootstrap.MachineJSON), &m)
		m.Id = st.MachineID
		if b, err := mo.Marshal(&m); err == nil {
			st.Bootstrap.MachineJSON = string(b)
		}
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, "", err
	}
	pending := filepath.Join(dataDir, snapshot.PendingFile)
	if err := os.WriteFile(pending, raw, 0o600); err != nil {
		return nil, "", fmt.Errorf("write pending restore: %w", err)
	}
	return st, token, nil
}

// ApplyPendingRestore loads dataDir/pending-restore.snapshot into the store
// (if present), keeps this machine as the sole member, re-injects the
// bootstrap admin token, and removes the pending file. Returns whether a
// restore ran.
func (s *NodeState) ApplyPendingRestore(ctx context.Context, st store.Store, dataDir string) (bool, error) {
	path := filepath.Join(dataDir, snapshot.PendingFile)
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	snap, err := snapshot.Decode(raw)
	if err != nil {
		return false, err
	}

	var tok v1.ApiToken
	var tokHash string
	if s.Bootstrap != nil {
		uo := protojson.UnmarshalOptions{DiscardUnknown: true}
		if err := uo.Unmarshal([]byte(s.Bootstrap.AdminTokenRow), &tok); err != nil {
			return false, err
		}
		tokHash = s.Bootstrap.AdminTokenHash
	}

	_, err = snapshot.Apply(ctx, st, snap, snapshot.ApplyOptions{
		Force:         true,
		SelfMachineID: s.MachineID,
		SelfMachine:   s.Machine(timestamppb.Now()),
	})
	if err != nil {
		return false, err
	}

	// Ensure cluster row matches this node's recovered CA / id.
	cl, err := store.GetCluster(ctx, st)
	if err != nil {
		cl = &v1.Cluster{Id: s.ClusterID, Name: s.ClusterName, Network: s.Network, CreatedAt: timestamppb.Now()}
	}
	cl.Id = s.ClusterID
	cl.CaPem = s.CACertPEM
	if cl.Name == "" {
		cl.Name = s.ClusterName
	}
	if err := store.PutCluster(ctx, st, cl); err != nil {
		return false, err
	}

	if tok.Id != "" && tokHash != "" {
		if err := store.PutProto(ctx, st, store.TableAPITokens, tok.Id,
			map[string]string{"user_id": tok.UserId, "token_hash": tokHash}, &tok); err != nil {
			return false, err
		}
	}
	s.Bootstrap = nil
	if err := os.Remove(path); err != nil {
		return true, err
	}
	return true, nil
}
