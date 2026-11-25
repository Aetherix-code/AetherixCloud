// Package cluster implements cluster bootstrap (init), the join protocol
// (tokens, server and client halves) and the node's local identity file.
package cluster

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// Ports and well-known values.
const (
	DefaultAPIPort         = 8443 // public TLS API (token/bearer authenticated)
	DefaultMeshAPIPort     = 8080 // plain HTTP, bound to loopback + mesh IP only
	DefaultCorrosionGossip = 8787
	DefaultCorrosionAPI    = "127.0.0.1:8081"
	StateFile              = "node.json"
)

// PeerSeed is a peer known at join time (before the store has synced).
type PeerSeed struct {
	MachineID string   `json:"machine_id"`
	Name      string   `json:"name"`
	PublicKey string   `json:"public_key"`
	Endpoints []string `json:"endpoints"`
	Subnet    string   `json:"subnet"`
	MeshIP    string   `json:"mesh_ip"`
}

// Bootstrap holds protojson rows an `init` node writes to the store on its
// first start (the store only exists once the daemon runs).
type Bootstrap struct {
	ClusterJSON    string `json:"cluster"`
	MachineJSON    string `json:"machine"`
	AdminTokenRow  string `json:"admin_token"` // protojson ApiToken
	AdminTokenHash string `json:"admin_token_hash"`
}

// NodeState is the node's local, private identity (data dir/node.json,
// mode 0600). It contains secrets: the WireGuard private key, the cluster
// key and the CA key.
type NodeState struct {
	ClusterID   string `json:"cluster_id"`
	ClusterName string `json:"cluster_name"`
	MachineID   string `json:"machine_id"`
	MachineName string `json:"machine_name"`
	Subnet      string `json:"subnet"`
	MeshIP      string `json:"mesh_ip"`
	Network     string `json:"network"`

	WGPrivateKey string `json:"wg_private_key"`
	WGPublicKey  string `json:"wg_public_key"`
	WGPort       int    `json:"wg_port"`

	ClusterKey string `json:"cluster_key"`
	CACertPEM  string `json:"ca_cert"`
	CAKeyPEM   string `json:"ca_key"`

	// Advertise are the public hosts/IPs of this machine (cert SANs and
	// WireGuard endpoint candidates).
	Advertise []string          `json:"advertise"`
	APIPort   int               `json:"api_port"`
	Roles     []string          `json:"roles,omitempty"`
	Labels    map[string]string `json:"labels,omitempty"`

	Peers          []PeerSeed `json:"peers,omitempty"`
	StoreBootstrap []string   `json:"store_bootstrap,omitempty"`
	Bootstrap      *Bootstrap `json:"bootstrap,omitempty"`
}

// LoadState reads dir/node.json. os.ErrNotExist => node not initialised.
func LoadState(dir string) (*NodeState, error) {
	b, err := os.ReadFile(filepath.Join(dir, StateFile))
	if err != nil {
		return nil, err
	}
	var s NodeState
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// Exists reports whether dir has a node state.
func Exists(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, StateFile))
	return !errors.Is(err, os.ErrNotExist)
}

// Save writes the state atomically with mode 0600.
func (s *NodeState) Save(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, StateFile+".tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, StateFile))
}
