package ingress

import (
	"context"
	"io"
	"net"
	"strconv"
	"testing"
	"time"

	v1 "github.com/aetherix/aetherixcloud/gen/aetherix/v1"
	"github.com/aetherix/aetherixcloud/internal/store"
	"github.com/stretchr/testify/require"
)

func TestTCPProxyForwards(t *testing.T) {
	backend, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer backend.Close()
	go func() {
		c, err := backend.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		buf := make([]byte, 4)
		_, _ = io.ReadFull(c, buf)
		_, _ = c.Write([]byte("pong"))
	}()

	st := store.NewMemory()
	ctx := context.Background()
	pub := freePort(t)
	backendPort := backend.Addr().(*net.TCPAddr).Port
	require.NoError(t, store.PutService(ctx, st, &v1.Service{
		Id: "s1", Name: "db",
		Spec: &v1.ServiceSpec{
			Ports: []*v1.ServicePort{{
				ContainerPort: int32(backendPort),
				Mode:          "tcp",
				PublishedPort: int32(pub),
			}},
		},
	}))
	require.NoError(t, store.PutMachine(ctx, st, &v1.Machine{Id: "m1", State: v1.MachineState_MACHINE_STATE_UP}))
	require.NoError(t, store.PutReplica(ctx, st, &v1.Replica{
		Id: "r1", ServiceId: "s1", MachineId: "m1", DesiredRunning: true,
		State: v1.ReplicaState_REPLICA_STATE_RUNNING, ContainerIp: "127.0.0.1",
	}))

	p := &TCPProxy{Store: st, Self: "m1"}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go p.Run(runCtx)

	require.Eventually(t, func() bool {
		c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(pub)), 200*time.Millisecond)
		if err != nil {
			return false
		}
		defer c.Close()
		_, _ = c.Write([]byte("ping"))
		buf := make([]byte, 4)
		_ = c.SetReadDeadline(time.Now().Add(time.Second))
		_, err = io.ReadFull(c, buf)
		return err == nil && string(buf) == "pong"
	}, 5*time.Second, 50*time.Millisecond)
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}
