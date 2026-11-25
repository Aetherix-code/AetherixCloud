package secrets

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSealOpen(t *testing.T) {
	k, err := NewKey()
	require.NoError(t, err)
	s, err := k.Seal([]byte("hunter2"))
	require.NoError(t, err)
	got, err := k.Open(s)
	require.NoError(t, err)
	require.Equal(t, "hunter2", string(got))

	k2, _ := NewKey()
	_, err = k2.Open(s)
	require.Error(t, err)
	_, err = k.Open("plain")
	require.Error(t, err)

	k3, err := ParseKey(k.String())
	require.NoError(t, err)
	require.Equal(t, k, k3)
}
