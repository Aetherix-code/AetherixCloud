package api

import (
	"context"
	"testing"

	v1 "github.com/aetherix/aetherixcloud/gen/aetherix/v1"
	"github.com/aetherix/aetherixcloud/internal/store"
	"github.com/stretchr/testify/require"
)

func TestAssignTCPPorts(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	require.NoError(t, store.PutService(ctx, st, &v1.Service{
		Id: "other", Spec: &v1.ServiceSpec{Ports: []*v1.ServicePort{{Mode: "tcp", PublishedPort: 20042}}},
	}))

	spec := &v1.ServiceSpec{Ports: []*v1.ServicePort{
		{Mode: "tcp", ContainerPort: 5432},
		{Mode: "https", ContainerPort: 8080},
		{Mode: "tcp", ContainerPort: 6379, PublishedPort: 30001}, // keep explicit
	}}
	require.NoError(t, AssignTCPPorts(ctx, st, spec, "new"))
	require.NotZero(t, spec.Ports[0].PublishedPort)
	require.NotEqual(t, int32(20042), spec.Ports[0].PublishedPort)
	require.GreaterOrEqual(t, spec.Ports[0].PublishedPort, int32(tcpProxyPortMin))
	require.LessOrEqual(t, spec.Ports[0].PublishedPort, int32(tcpProxyPortMax))
	require.Equal(t, int32(0), spec.Ports[1].PublishedPort)
	require.Equal(t, int32(30001), spec.Ports[2].PublishedPort)
	require.NotEqual(t, spec.Ports[0].PublishedPort, spec.Ports[2].PublishedPort)
}
