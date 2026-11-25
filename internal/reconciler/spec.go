package reconciler

import (
	"fmt"
	"strings"
	"time"

	v1 "github.com/aetherix/aetherixcloud/gen/aetherix/v1"
	"github.com/aetherix/aetherixcloud/internal/runtime"
)

// ContainerSpecFor converts a deployment spec into a runtime ContainerSpec.
func ContainerSpecFor(svc *v1.Service, dep *v1.Deployment, rep *v1.Replica, clusterID, machineID string, env map[string]string, dns []string) runtime.ContainerSpec {
	spec := dep.Spec
	labels := map[string]string{}
	for k, v := range spec.Labels {
		labels[k] = v
	}
	labels[runtime.LabelManaged] = "true"
	labels[runtime.LabelCluster] = clusterID
	labels[runtime.LabelMachine] = machineID
	labels[runtime.LabelProject] = svc.ProjectId
	labels[runtime.LabelService] = svc.Id
	labels[runtime.LabelServiceName] = svc.Name
	labels[runtime.LabelDeployment] = dep.Id
	labels[runtime.LabelReplica] = rep.Id
	labels[runtime.LabelFence] = fmt.Sprint(rep.Fence)

	cs := runtime.ContainerSpec{
		Name:          ContainerName(svc.Name, rep.Id),
		Image:         spec.Image,
		Command:       spec.Command,
		Entrypoint:    spec.Entrypoint,
		Env:           env,
		Labels:        labels,
		Network:       runtime.NetworkName,
		Hostname:      svc.Name,
		DNS:           dns,
		CPUMillis:     spec.GetResources().GetCpuMillis(),
		MemoryBytes:   spec.GetResources().GetMemoryBytes(),
		RestartPolicy: spec.RestartPolicy,
		User:          spec.User,
		WorkingDir:    spec.WorkingDir,
	}
	if spec.StopGraceSeconds > 0 {
		cs.StopTimeout = time.Duration(spec.StopGraceSeconds) * time.Second
	}
	for _, p := range spec.Ports {
		proto := "tcp"
		if p.Protocol == v1.PortProtocol_PORT_PROTOCOL_UDP {
			proto = "udp"
		}
		pb := runtime.PortBinding{ContainerPort: int(p.ContainerPort), Protocol: proto}
		if p.Mode == "host" {
			pb.HostPort = int(p.PublishedPort)
			if pb.HostPort == 0 {
				pb.HostPort = int(p.ContainerPort)
			}
		}
		cs.Ports = append(cs.Ports, pb)
	}
	for _, v := range spec.Volumes {
		cs.Mounts = append(cs.Mounts, runtime.Mount{
			Source: VolumeName(svc, v), Target: v.Target, ReadOnly: v.ReadOnly, SizeBytes: v.SizeBytes,
		})
	}
	cs.Health = healthSpec(spec)
	return cs
}

// VolumeName namespaces named volumes by service; absolute paths are bind mounts.
func VolumeName(svc *v1.Service, v *v1.VolumeMount) string {
	if strings.HasPrefix(v.Source, "/") {
		return v.Source
	}
	return "ax-" + svc.Id[:min(8, len(svc.Id))] + "-" + v.Source
}

// ContainerName is "<service>-<replica suffix>".
func ContainerName(service, replicaID string) string {
	suffix := replicaID
	if len(suffix) > 8 {
		suffix = suffix[len(suffix)-8:]
	}
	return service + "-" + suffix
}

func healthSpec(spec *v1.ServiceSpec) *runtime.HealthSpec {
	hc := spec.GetHealthCheck()
	if hc == nil {
		return nil
	}
	h := &runtime.HealthSpec{
		Interval:    time.Duration(hc.IntervalSeconds) * time.Second,
		Timeout:     time.Duration(hc.TimeoutSeconds) * time.Second,
		Retries:     int(hc.Retries),
		StartPeriod: time.Duration(hc.StartPeriodSeconds) * time.Second,
	}
	if h.Interval == 0 {
		h.Interval = 5 * time.Second
	}
	if h.Timeout == 0 {
		h.Timeout = 3 * time.Second
	}
	if h.Retries == 0 {
		h.Retries = 3
	}
	switch {
	case len(hc.Command) > 0:
		h.Command = hc.Command
	case hc.HttpPath != "":
		port := hc.HttpPort
		if port == 0 && len(spec.Ports) > 0 {
			port = spec.Ports[0].ContainerPort
		}
		url := fmt.Sprintf("http://127.0.0.1:%d%s", port, hc.HttpPath)
		h.Command = []string{"sh", "-c", fmt.Sprintf("wget -q -O /dev/null %s || curl -fsS -o /dev/null %s", url, url)}
	case hc.TcpPort != 0:
		h.Command = []string{"sh", "-c", fmt.Sprintf("nc -z 127.0.0.1 %d", hc.TcpPort)}
	default:
		return nil
	}
	return h
}
