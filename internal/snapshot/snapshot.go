// Package snapshot encodes and restores full replicated-store dumps
// (`.snapshot` files from ClusterService.Backup / `ax cluster backup`).
//
// A snapshot holds every store table as JSON. Version 2 also embeds the
// cluster identity secrets (cluster key + CA) so a node can be rebuilt after
// total loss via `aetherixd init --from-backup`. Local Docker volume data is
// never included — restore those from S3 volume backups separately.
package snapshot

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"

	v1 "github.com/aetherix/aetherixcloud/gen/aetherix/v1"
	"github.com/aetherix/aetherixcloud/internal/store"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	// Version of the on-disk / streamed format.
	Version = 2
	// PendingFile is written under the data dir by init --from-backup and
	// applied on first daemon start.
	PendingFile = "pending-restore.snapshot"
)

// Row is one store record in a snapshot.
type Row struct {
	ID   string            `json:"id"`
	Idx  map[string]string `json:"idx,omitempty"`
	Data string            `json:"data"`
}

// Identity holds secrets that live in node.json (not in the store). Present
// in version ≥ 2 backups so disaster recovery can rebuild a first node.
type Identity struct {
	ClusterID  string `json:"cluster_id"`
	ClusterKey string `json:"cluster_key"`
	CACertPEM  string `json:"ca_cert"`
	CAKeyPEM   string `json:"ca_key"`
}

// Snapshot is the JSON envelope written by Backup.
type Snapshot struct {
	Version  int                  `json:"version"`
	Created  time.Time            `json:"created"`
	Tables   map[string][]Row     `json:"tables"`
	Identity *Identity            `json:"identity,omitempty"`
}

// Encode builds a snapshot from the store. identity may be nil (version 1
// compatible body); when set it is embedded for DR.
func Encode(ctx context.Context, st store.Store, identity *Identity) ([]byte, error) {
	snap := Snapshot{
		Version:  Version,
		Created:  time.Now().UTC(),
		Tables:   map[string][]Row{},
		Identity: identity,
	}
	if identity == nil {
		snap.Version = 1
	}
	for _, t := range store.Tables {
		recs, err := st.List(ctx, t.Name, store.Query{})
		if err != nil {
			return nil, err
		}
		for _, r := range recs {
			snap.Tables[t.Name] = append(snap.Tables[t.Name], Row{ID: r.ID, Idx: r.Idx, Data: string(r.Data)})
		}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", " ")
	if err := enc.Encode(snap); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Decode parses a snapshot JSON blob.
func Decode(data []byte) (*Snapshot, error) {
	var snap Snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return nil, fmt.Errorf("snapshot: invalid JSON: %w", err)
	}
	if snap.Version < 1 || snap.Version > Version {
		return nil, fmt.Errorf("snapshot: unsupported version %d", snap.Version)
	}
	if snap.Tables == nil {
		return nil, fmt.Errorf("snapshot: missing tables")
	}
	return &snap, nil
}

// ApplyOptions controls safety checks and post-restore machine cleanup.
type ApplyOptions struct {
	// Force skips the "fresh single-node" safety check.
	Force bool
	// SelfMachineID is the recovering node's id. Other machines are deleted
	// and their replicas cleared so they must rejoin.
	SelfMachineID string
	// SelfMachine, when set, is upserted after restore (replaces any stale
	// row for SelfMachineID).
	SelfMachine *v1.Machine
}

// Result summarises what Apply did.
type Result struct {
	Tables           int
	Rows             int
	MachinesRemoved  int
	ReplicasCleared  int
	LeasesCleared    int
	JoinTokensCleared int
}

// IsFresh reports whether the store looks like a newly initialised single-node
// cluster (safe to restore into without --force): at most one machine and no
// projects/services/deployments.
func IsFresh(ctx context.Context, st store.Store) (bool, error) {
	ms, err := st.List(ctx, store.TableMachines, store.Query{})
	if err != nil {
		return false, err
	}
	if len(ms) > 1 {
		return false, nil
	}
	for _, t := range []string{store.TableProjects, store.TableServices, store.TableDeployments} {
		recs, err := st.List(ctx, t, store.Query{Limit: 1})
		if err != nil {
			return false, err
		}
		if len(recs) > 0 {
			return false, nil
		}
	}
	return true, nil
}

// Apply replaces store contents with the snapshot. Safety: refuses unless the
// store is fresh (see IsFresh) or opts.Force is set.
//
// After loading tables, machines other than SelfMachineID are deleted, and
// replicas / leases / join tokens tied to the old topology are cleared so the
// controller can reschedule onto the recovering node. Peer machines must
// rejoin with a new join token.
func Apply(ctx context.Context, st store.Store, snap *Snapshot, opts ApplyOptions) (*Result, error) {
	if snap == nil {
		return nil, fmt.Errorf("snapshot: nil")
	}
	if !opts.Force {
		ok, err := IsFresh(ctx, st)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("snapshot: refuse restore into a non-fresh cluster (pass force=true to override; destroys current cluster state)")
		}
	}
	res := &Result{}

	// Delete rows that would otherwise survive (snapshot is a full replace).
	for _, t := range store.Tables {
		existing, err := st.List(ctx, t.Name, store.Query{})
		if err != nil {
			return nil, err
		}
		keep := map[string]bool{}
		for _, r := range snap.Tables[t.Name] {
			keep[r.ID] = true
		}
		var ops []store.TableRecord
		for _, r := range existing {
			if !keep[r.ID] {
				ops = append(ops, store.TableRecord{Table: t.Name, Record: store.Record{ID: r.ID}, Delete: true})
			}
		}
		if len(ops) > 0 {
			if err := st.Apply(ctx, ops); err != nil {
				return nil, err
			}
		}
	}

	for _, t := range store.Tables {
		rows := snap.Tables[t.Name]
		if len(rows) == 0 {
			continue
		}
		res.Tables++
		var ops []store.TableRecord
		for _, r := range rows {
			ops = append(ops, store.TableRecord{Table: t.Name, Record: store.Record{ID: r.ID, Idx: r.Idx, Data: []byte(r.Data)}})
			res.Rows++
		}
		if err := st.Apply(ctx, ops); err != nil {
			return nil, fmt.Errorf("snapshot: apply %s: %w", t.Name, err)
		}
	}

	if err := scrubTopology(ctx, st, opts, res); err != nil {
		return nil, err
	}
	return res, nil
}

func scrubTopology(ctx context.Context, st store.Store, opts ApplyOptions, res *Result) error {
	self := opts.SelfMachineID
	ms, err := store.ListMachines(ctx, st)
	if err != nil {
		return err
	}
	for _, m := range ms {
		if self != "" && m.Id == self {
			continue
		}
		if err := st.Delete(ctx, store.TableMachines, m.Id); err != nil {
			return err
		}
		res.MachinesRemoved++
	}

	reps, err := store.ListReplicas(ctx, st, nil)
	if err != nil {
		return err
	}
	for _, r := range reps {
		// Drop every replica: containers are gone after total loss, and
		// assignments to deleted machines are invalid. The controller
		// recreates desired replicas on the recovering node.
		if err := st.Delete(ctx, store.TableReplicas, r.Id); err != nil {
			return err
		}
		res.ReplicasCleared++
	}

	leases, err := st.List(ctx, store.TableLeases, store.Query{})
	if err != nil {
		return err
	}
	for _, r := range leases {
		if err := st.Delete(ctx, store.TableLeases, r.ID); err != nil {
			return err
		}
		res.LeasesCleared++
	}

	tokens, err := st.List(ctx, store.TableJoinTokens, store.Query{})
	if err != nil {
		return err
	}
	for _, r := range tokens {
		if err := st.Delete(ctx, store.TableJoinTokens, r.ID); err != nil {
			return err
		}
		res.JoinTokensCleared++
	}

	if opts.SelfMachine != nil {
		m := opts.SelfMachine
		if m.LastSeen == nil {
			m.LastSeen = timestamppb.Now()
		}
		if m.State == v1.MachineState_MACHINE_STATE_UNSPECIFIED {
			m.State = v1.MachineState_MACHINE_STATE_UP
		}
		if err := store.PutMachine(ctx, st, m); err != nil {
			return err
		}
	}
	return nil
}
