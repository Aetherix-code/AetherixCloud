// Package scheduler decides which machines run which replicas of a service.
//
// It is a pure function (no I/O): Schedule takes the desired ServiceSpec, the
// known machines and the existing replicas of ONE deployment of the service
// and returns the replicas to create and to remove. The controller in
// internal/reconciler runs it under the cluster scheduling lease.
//
// Modes:
//   - replicated: spec.replicas replicas, spread across eligible machines
//     (fewest replicas of this service first, then lowest resource
//     utilisation, then lowest machine ID for determinism).
//   - global: exactly one replica on every eligible machine.
//
// Eligibility: machine state up (unspecified counts as up), not cordoned or
// draining, has the worker role (no roles => worker), matches
// placement.machines (IDs or names) and placement.labels, and fits the
// per-replica resource request in its remaining capacity.
package scheduler

import (
	"fmt"
	"sort"

	v1 "github.com/aetherix/aetherixcloud/gen/aetherix/v1"
)

// Placement is a replica to create.
type Placement struct {
	MachineID string
}

// Plan is the result of Schedule.
type Plan struct {
	// Place lists replicas to create (one entry each).
	Place []Placement
	// Remove lists existing replicas that must be stopped (surplus, on
	// machines that no longer satisfy constraints, or lost on a down machine
	// of a reschedulable service).
	Remove []*v1.Replica
	// Stranded lists replicas on down machines that cannot be rescheduled
	// (service not reschedulable); they stay assigned.
	Stranded []*v1.Replica
	// Unplaceable counts replicas that wanted a home but none was eligible.
	Unplaceable int
	// Reasons explains Unplaceable (for events / UI).
	Reasons []string
}

// Schedule computes the plan. existing must contain only replicas of the
// target deployment with desired_running = true.
func Schedule(spec *v1.ServiceSpec, machines []*v1.Machine, existing []*v1.Replica) Plan {
	byID := map[string]*v1.Machine{}
	for _, m := range machines {
		byID[m.Id] = m
	}
	var plan Plan

	// Classify existing replicas.
	var live []*v1.Replica
	for _, r := range existing {
		m, ok := byID[r.MachineId]
		switch {
		case !ok || m.State == v1.MachineState_MACHINE_STATE_DOWN:
			if spec.Reschedulable {
				plan.Remove = append(plan.Remove, r)
			} else {
				plan.Stranded = append(plan.Stranded, r)
				live = append(live, r) // still counts: do not double-run
			}
		case !satisfiesPlacement(spec, m):
			plan.Remove = append(plan.Remove, r)
		case m.Draining && spec.Reschedulable:
			// drain: move reschedulable replicas off the machine
			plan.Remove = append(plan.Remove, r)
		default:
			live = append(live, r)
		}
	}

	eligible := eligibleMachines(spec, machines)

	// Load accounting: replicas of this service per machine, plus requested
	// resources already assigned in this plan.
	perMachine := map[string]int{}
	for _, r := range live {
		perMachine[r.MachineId]++
	}
	usedCPU := map[string]int64{}
	usedMem := map[string]int64{}
	for _, m := range machines {
		usedCPU[m.Id] = m.GetUsed().GetCpuMillis()
		usedMem[m.Id] = m.GetUsed().GetMemoryBytes()
	}

	switch spec.Mode {
	case v1.ServiceMode_SERVICE_MODE_GLOBAL:
		have := map[string]bool{}
		for _, r := range live {
			if have[r.MachineId] { // duplicate on one machine: surplus
				plan.Remove = append(plan.Remove, r)
				continue
			}
			have[r.MachineId] = true
		}
		for _, m := range eligible {
			if !have[m.Id] {
				if fits(spec, m, usedCPU, usedMem) {
					reserve(spec, m, usedCPU, usedMem)
					plan.Place = append(plan.Place, Placement{MachineID: m.Id})
				} else {
					plan.Unplaceable++
					plan.Reasons = append(plan.Reasons, fmt.Sprintf("machine %s lacks capacity", m.Name))
				}
			}
		}
	default: // replicated (and unspecified)
		want := int(spec.Replicas)
		if want < 0 {
			want = 0
		}
		if len(live) > want {
			plan.Remove = append(plan.Remove, pickSurplus(live, byID, len(live)-want, perMachine)...)
			break
		}
		for i := len(live); i < want; i++ {
			m := pickMachine(spec, eligible, perMachine, usedCPU, usedMem)
			if m == nil {
				plan.Unplaceable++
				if len(eligible) == 0 {
					plan.Reasons = append(plan.Reasons, "no eligible machine")
				} else {
					plan.Reasons = append(plan.Reasons, "insufficient resources on eligible machines")
				}
				continue
			}
			perMachine[m.Id]++
			reserve(spec, m, usedCPU, usedMem)
			plan.Place = append(plan.Place, Placement{MachineID: m.Id})
		}
	}
	return plan
}

// pickSurplus chooses n replicas to remove: not-running first, then from the
// machine with the most replicas, then highest ID.
func pickSurplus(live []*v1.Replica, byID map[string]*v1.Machine, n int, perMachine map[string]int) []*v1.Replica {
	cand := append([]*v1.Replica(nil), live...)
	var out []*v1.Replica
	for len(out) < n && len(cand) > 0 {
		sort.SliceStable(cand, func(i, j int) bool {
			ri, rj := cand[i], cand[j]
			si, sj := rank(ri), rank(rj)
			if si != sj {
				return si < sj
			}
			if perMachine[ri.MachineId] != perMachine[rj.MachineId] {
				return perMachine[ri.MachineId] > perMachine[rj.MachineId]
			}
			return ri.Id > rj.Id
		})
		v := cand[0]
		cand = cand[1:]
		perMachine[v.MachineId]--
		out = append(out, v)
	}
	return out
}

// rank: lower = removed first.
func rank(r *v1.Replica) int {
	switch r.State {
	case v1.ReplicaState_REPLICA_STATE_FAILED, v1.ReplicaState_REPLICA_STATE_STOPPED, v1.ReplicaState_REPLICA_STATE_STRANDED:
		return 0
	case v1.ReplicaState_REPLICA_STATE_PENDING, v1.ReplicaState_REPLICA_STATE_STARTING, v1.ReplicaState_REPLICA_STATE_UNSPECIFIED:
		return 1
	default:
		return 2
	}
}

func pickMachine(spec *v1.ServiceSpec, eligible []*v1.Machine, per map[string]int, cpu, mem map[string]int64) *v1.Machine {
	var best *v1.Machine
	var bestLoad float64
	for _, m := range eligible {
		if !fits(spec, m, cpu, mem) {
			continue
		}
		load := utilisation(m, cpu, mem)
		if best == nil ||
			per[m.Id] < per[best.Id] ||
			(per[m.Id] == per[best.Id] && load < bestLoad) ||
			(per[m.Id] == per[best.Id] && load == bestLoad && m.Id < best.Id) {
			best, bestLoad = m, load
		}
	}
	return best
}

func utilisation(m *v1.Machine, cpu, mem map[string]int64) float64 {
	var u float64
	if c := m.GetCapacity().GetCpuMillis(); c > 0 {
		u += float64(cpu[m.Id]) / float64(c)
	}
	if c := m.GetCapacity().GetMemoryBytes(); c > 0 {
		u += float64(mem[m.Id]) / float64(c)
	}
	return u
}

func fits(spec *v1.ServiceSpec, m *v1.Machine, cpu, mem map[string]int64) bool {
	if c := m.GetCapacity().GetCpuMillis(); c > 0 && spec.GetResources().GetCpuMillis() > 0 {
		if cpu[m.Id]+spec.Resources.CpuMillis > c {
			return false
		}
	}
	if c := m.GetCapacity().GetMemoryBytes(); c > 0 && spec.GetResources().GetMemoryBytes() > 0 {
		if mem[m.Id]+spec.Resources.MemoryBytes > c {
			return false
		}
	}
	return true
}

func reserve(spec *v1.ServiceSpec, m *v1.Machine, cpu, mem map[string]int64) {
	cpu[m.Id] += spec.GetResources().GetCpuMillis()
	mem[m.Id] += spec.GetResources().GetMemoryBytes()
}

// Eligible reports whether m may receive new replicas of spec (ignoring
// resource fit). Exported for the API ("why is this stranded?").
func Eligible(spec *v1.ServiceSpec, m *v1.Machine) bool {
	if m.State == v1.MachineState_MACHINE_STATE_DOWN || m.State == v1.MachineState_MACHINE_STATE_SUSPECT {
		return false
	}
	if m.Cordoned || m.Draining {
		return false
	}
	if !hasWorkerRole(m) {
		return false
	}
	return satisfiesPlacement(spec, m)
}

func eligibleMachines(spec *v1.ServiceSpec, machines []*v1.Machine) []*v1.Machine {
	var out []*v1.Machine
	for _, m := range machines {
		if Eligible(spec, m) {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Id < out[j].Id })
	return out
}

func hasWorkerRole(m *v1.Machine) bool {
	if len(m.Roles) == 0 {
		return true
	}
	for _, r := range m.Roles {
		if r == v1.MachineRole_MACHINE_ROLE_WORKER {
			return true
		}
	}
	return false
}

// satisfiesPlacement checks name/ID and label constraints only.
func satisfiesPlacement(spec *v1.ServiceSpec, m *v1.Machine) bool {
	p := spec.GetPlacement()
	if p == nil {
		return true
	}
	if len(p.Machines) > 0 {
		ok := false
		for _, n := range p.Machines {
			if n == m.Id || n == m.Name {
				ok = true
			}
		}
		if !ok {
			return false
		}
	}
	for k, v := range p.Labels {
		if m.Labels[k] != v {
			return false
		}
	}
	return true
}
