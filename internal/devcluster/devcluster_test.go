package devcluster

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestNaming(t *testing.T) {
	require.Equal(t, "node-3", NodeName(3))
	require.Equal(t, "ax-node-3", ContainerName(3))
	require.Equal(t, "172.28.0.13", NodeIP(3))
	for in, want := range map[string]int{"node-2": 2, "ax-node-2": 2, "2": 2} {
		n, err := ParseNode(in)
		require.NoError(t, err)
		require.Equal(t, want, n)
	}
	for _, bad := range []string{"", "node-", "node-x", "0", "999"} {
		_, err := ParseNode(bad)
		require.Error(t, err, bad)
	}
}

func TestScripts(t *testing.T) {
	s := PartitionScript("172.28.0.13", "172.28.0.14")
	require.Equal(t, 4, strings.Count(s, "-j DROP"))
	require.Contains(t, s, "-s 172.28.0.13")
	require.Contains(t, s, "-d 172.28.0.14")
	require.Contains(t, HealScript, "iptables -F AX_PARTITION")
	require.Contains(t, LatencyScript(200*time.Millisecond), "delay 200ms")
	require.Contains(t, LatencyScript(0), "qdisc del")
	require.Equal(t, []string{"exec", "-it", "ax-node-2", "bash"}, SSHArgs(2))
}

func TestOptionsDefaults(t *testing.T) {
	o := Options{}
	o.defaults()
	require.Equal(t, 3, o.Nodes)
	require.Equal(t, DefaultImage, o.Image)
	require.Equal(t, 8443, o.APIHostPort)
	require.NotEmpty(t, o.StateFile)
}
