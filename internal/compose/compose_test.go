package compose

import (
	"testing"

	v1 "github.com/aetherix/aetherixcloud/gen/aetherix/v1"
	"github.com/stretchr/testify/require"
)

const sample = `
name: shop
services:
  web:
    image: ghcr.io/acme/web:${TAG:-latest}
    command: ["node", "server.js"]
    environment:
      DB_URL: postgres://db/app
      DEBUG:
      PORT: 3000
    ports:
      - "8080:3000"
      - "127.0.0.1:9090:9090/udp"
    x-ports:
      - app.example.com:3000/https
      - 3001/http
    x-machines: [node-1, node-2]
    x-labels: {disk: ssd}
    x-reschedulable: true
    x-deploy:
      strategy: stop-first
      max_surge: 2
    depends_on: [db]
    healthcheck:
      test: ["CMD", "curl", "-f", "http://localhost:3000/up"]
      interval: 10s
      timeout: 2s
      retries: 5
      start_period: 30s
    deploy:
      replicas: 3
      resources:
        limits: {cpus: "0.5", memory: 512M}
    restart: on-failure:3
    stop_grace_period: 20s
  db:
    image: postgres:16
    command: postgres -c "shared_buffers=128MB"
    volumes:
      - pgdata:/var/lib/postgresql/data
      - /etc/hosts:/etc/hosts:ro
    links: [web]
  agent:
    build:
      context: ./agent
      dockerfile: Dockerfile.dev
      args: {A: "1"}
    x-build: {builder: railpack, watch_paths: ["agent/**"]}
    deploy: {mode: global}
volumes:
  pgdata:
    x-persistence: backup-restore
    x-size: 10G
`

func TestParseFull(t *testing.T) {
	p, err := Parse([]byte(sample), map[string]string{"DEBUG": "1"})
	require.NoError(t, err)
	require.Equal(t, "shop", p.Name)
	var names []string
	for _, s := range p.Services {
		names = append(names, s.Name)
	}
	require.Equal(t, []string{"agent", "db", "web"}, names) // topological, db before web

	by := map[string]*Service{}
	for _, s := range p.Services {
		by[s.Name] = s
	}
	web := by["web"].Spec
	require.Equal(t, "ghcr.io/acme/web:latest", web.Image)
	require.Equal(t, []string{"node", "server.js"}, web.Command)
	require.Equal(t, map[string]string{"DB_URL": "postgres://db/app", "DEBUG": "1", "PORT": "3000"}, web.Env)
	require.Equal(t, int32(3), web.Replicas)
	require.True(t, web.Reschedulable)
	require.Equal(t, v1.DeployStrategy_DEPLOY_STRATEGY_STOP_FIRST, web.Strategy)
	require.Equal(t, int32(2), web.MaxSurge)
	require.Equal(t, []string{"node-1", "node-2"}, web.Placement.Machines)
	require.Equal(t, "ssd", web.Placement.Labels["disk"])
	require.Equal(t, int64(500), web.Resources.CpuMillis)
	require.Equal(t, int64(512<<20), web.Resources.MemoryBytes)
	require.Equal(t, "on-failure", web.RestartPolicy)
	require.Equal(t, int32(20), web.StopGraceSeconds)
	require.Equal(t, []string{"db"}, web.DependsOn)

	require.Equal(t, []string{"curl", "-f", "http://localhost:3000/up"}, web.HealthCheck.Command)
	require.Equal(t, int32(10), web.HealthCheck.IntervalSeconds)
	require.Equal(t, int32(30), web.HealthCheck.StartPeriodSeconds)

	require.Len(t, web.Ports, 4)
	require.Equal(t, "host", web.Ports[0].Mode)
	require.Equal(t, int32(8080), web.Ports[0].PublishedPort)
	require.Equal(t, int32(3000), web.Ports[0].ContainerPort)
	require.Equal(t, v1.PortProtocol_PORT_PROTOCOL_UDP, web.Ports[1].Protocol)
	require.Equal(t, "https", web.Ports[2].Mode)
	require.Equal(t, "app.example.com", web.Ports[2].Hostname)
	require.Equal(t, "http", web.Ports[3].Mode)

	db := by["db"].Spec
	require.Equal(t, []string{"postgres", "-c", "shared_buffers=128MB"}, db.Command)
	require.Equal(t, "backup-restore", db.Volumes[0].Persistence)
	require.Equal(t, int64(10<<30), db.Volumes[0].SizeBytes)
	require.True(t, db.Volumes[1].ReadOnly)
	require.Equal(t, v1.DeployStrategy_DEPLOY_STRATEGY_STOP_FIRST, db.Strategy) // exclusive volume default
	require.Contains(t, p.Warnings[0], "links")

	ag := by["agent"].Spec
	require.Equal(t, v1.ServiceMode_SERVICE_MODE_GLOBAL, ag.Mode)
	require.Equal(t, "railpack", ag.Build.Builder)
	require.Equal(t, "./agent", ag.Build.Context)
	require.Equal(t, "Dockerfile.dev", ag.Build.Dockerfile)
	require.Equal(t, map[string]string{"A": "1"}, ag.Build.Args)
	require.Equal(t, []string{"agent/**"}, ag.Build.WatchPaths)
}

func TestParseErrors(t *testing.T) {
	cases := map[string]string{
		"no services":  "name: x",
		"no image":     "services:\n  a:\n    command: x",
		"bad port":     "services:\n  a:\n    image: i\n    x-ports: [\"abc/https\"]",
		"bad proto":    "services:\n  a:\n    image: i\n    x-ports: [\"80/ftp\"]",
		"bad strategy": "services:\n  a:\n    image: i\n    x-deploy: {strategy: yolo}",
		"unknown dep":  "services:\n  a:\n    image: i\n    depends_on: [zzz]",
		"cycle":        "services:\n  a:\n    image: i\n    depends_on: [b]\n  b:\n    image: i\n    depends_on: [a]",
		"anon volume":  "services:\n  a:\n    image: i\n    volumes: [\"/data\"]",
		"required var": "services:\n  a:\n    image: ${IMG:?set IMG}",
		"invalid yaml": "services: [",
		"bad mem":      "services:\n  a:\n    image: i\n    deploy: {resources: {limits: {memory: lots}}}",
	}
	for name, y := range cases {
		_, err := Parse([]byte(y), nil)
		require.Error(t, err, name)
	}
}

func TestInterpolate(t *testing.T) {
	vars := map[string]string{"A": "1", "E": ""}
	for in, want := range map[string]string{
		"${A}": "1", "$A-x": "1-x", "${B:-d}": "d", "${E:-d}": "d", "${E-d}": "", "${A:-d}": "1",
		"$$A": "$A", "cost $5": "cost $5", "${B}": "",
	} {
		got, err := Interpolate(in, vars)
		require.NoError(t, err, in)
		require.Equal(t, want, got, in)
	}
	_, err := Interpolate("${B:?need B}", vars)
	require.Error(t, err)
}

func TestParseXPortAndHelpers(t *testing.T) {
	p, err := ParseXPort("53/udp@host")
	require.NoError(t, err)
	require.Equal(t, "host", p.Mode)
	require.Equal(t, int32(53), p.PublishedPort)
	require.Equal(t, v1.PortProtocol_PORT_PROTOCOL_UDP, p.Protocol)

	p, err = ParseXPort("8000")
	require.NoError(t, err)
	require.Equal(t, "https", p.Mode)

	got, err := SplitCommand(`sh -c 'echo "hi there"' "a b"`)
	require.NoError(t, err)
	require.Equal(t, []string{"sh", "-c", `echo "hi there"`, "a b"}, got)
	_, err = SplitCommand(`echo "oops`)
	require.Error(t, err)

	n, err := ParseBytes("1.5g")
	require.NoError(t, err)
	require.Equal(t, int64(1.5*float64(1<<30)), n)
}

func TestHealthShell(t *testing.T) {
	p, err := Parse([]byte("services:\n  a:\n    image: i\n    healthcheck:\n      test: [\"CMD-SHELL\", \"pg_isready -U x\"]\n"), nil)
	require.NoError(t, err)
	require.Equal(t, []string{"sh", "-c", "pg_isready -U x"}, p.Services[0].Spec.HealthCheck.Command)
}
