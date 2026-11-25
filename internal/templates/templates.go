// Package templates defines one-click service templates (Postgres, Redis,
// MySQL, MinIO). The MCP create_service tool and the dashboard New Service
// dialog share this catalog.
package templates

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"sort"
	"strings"

	v1 "github.com/aetherix/aetherixcloud/gen/aetherix/v1"
)

// Template is a one-click service recipe.
type Template struct {
	ID      string            `json:"id"`
	Label   string            `json:"label"`
	Image   string            `json:"image"`
	Port    int32             `json:"port"`
	Volume  string            `json:"volume,omitempty"`
	Command []string          `json:"command,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	// Secrets are env keys filled with generated values and stored as secret variables.
	Secrets []string `json:"secrets,omitempty"`
}

var catalog = map[string]Template{
	"postgres": {
		ID: "postgres", Label: "Postgres", Image: "postgres:16", Port: 5432,
		Volume: "/var/lib/postgresql/data",
		Env:    map[string]string{"POSTGRES_USER": "app", "POSTGRES_DB": "app"},
		Secrets: []string{"POSTGRES_PASSWORD"},
	},
	"redis": {
		ID: "redis", Label: "Redis", Image: "redis:7", Port: 6379, Volume: "/data",
	},
	"mysql": {
		ID: "mysql", Label: "MySQL", Image: "mysql:8", Port: 3306, Volume: "/var/lib/mysql",
		Env:     map[string]string{"MYSQL_DATABASE": "app"},
		Secrets: []string{"MYSQL_ROOT_PASSWORD"},
	},
	"minio": {
		ID: "minio", Label: "MinIO", Image: "minio/minio:latest", Port: 9000, Volume: "/data",
		Command: []string{"server", "/data", "--console-address", ":9001"},
		Env:     map[string]string{"MINIO_ROOT_USER": "admin"},
		Secrets: []string{"MINIO_ROOT_PASSWORD"},
	},
}

// List returns templates sorted by id.
func List() []Template {
	out := make([]Template, 0, len(catalog))
	for _, t := range catalog {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Get returns a template by id (case-insensitive).
func Get(id string) (Template, bool) {
	t, ok := catalog[strings.ToLower(strings.TrimSpace(id))]
	return t, ok
}

// Names returns available template ids.
func Names() []string {
	ns := make([]string, 0, len(catalog))
	for n := range catalog {
		ns = append(ns, n)
	}
	sort.Strings(ns)
	return ns
}

// Spec builds a ServiceSpec for the template. name is used for the volume source.
func Spec(t Template, name string) *v1.ServiceSpec {
	spec := &v1.ServiceSpec{
		Mode:     v1.ServiceMode_SERVICE_MODE_REPLICATED,
		Strategy: v1.DeployStrategy_DEPLOY_STRATEGY_STOP_FIRST,
		Replicas: 1,
		Source:   &v1.ServiceSource{Image: t.Image},
		Image:    t.Image,
		Command:  t.Command,
		Ports: []*v1.ServicePort{{
			ContainerPort: t.Port, Protocol: v1.PortProtocol_PORT_PROTOCOL_TCP, Mode: "internal",
		}},
		Env: map[string]string{},
	}
	for k, v := range t.Env {
		spec.Env[k] = v
	}
	if t.Volume != "" {
		src := name + "-data"
		if name == "" {
			src = t.ID + "-data"
		}
		spec.Volumes = []*v1.VolumeMount{{
			Source: src, Target: t.Volume, Persistence: "local",
			SizeBytes: 10 << 30, // 10 GiB soft limit at first create
		}}
	}
	return spec
}

// RandomSecret returns a URL-safe random string suitable for DB passwords.
func RandomSecret() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// SecretVariables builds secret Variable rows for the template (values generated).
func SecretVariables(t Template, environmentID, serviceID string) []*v1.Variable {
	out := make([]*v1.Variable, 0, len(t.Secrets))
	for _, k := range t.Secrets {
		out = append(out, &v1.Variable{
			EnvironmentId: environmentID, ServiceId: serviceID,
			Key: k, Value: RandomSecret(), Secret: true,
		})
	}
	return out
}

// ErrUnknown is returned when a template id is not in the catalog.
type ErrUnknown struct{ ID string }

func (e ErrUnknown) Error() string {
	return fmt.Sprintf("unknown template %q (available: %s)", e.ID, strings.Join(Names(), ", "))
}
