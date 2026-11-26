package build

import (
	"testing"

	"github.com/aetherix/aetherixcloud/internal/backup"
	"github.com/stretchr/testify/require"
)

func TestArchiveObjectName(t *testing.T) {
	require.Equal(t, "abcdef012345.tar.gz", archiveObjectName("sha256:abcdef0123456789", "127.0.0.1:5000/p/s:tag"))
	require.Equal(t, "abc1234-deadbeef.tar.gz", archiveObjectName("", "127.0.0.1:5000/p/s:abc1234-deadbeef"))
}

func TestS3ImageKey(t *testing.T) {
	s3 := backup.S3Config{Prefix: "aetherix/"}
	require.Equal(t, "aetherix/images/my-app/api/abc.tar.gz", s3ImageKey(s3, "My App", "api", "abc.tar.gz"))
	require.Equal(t, "aetherix/images/my-app/api/", s3ImageKey(s3, "My App", "api", ""))
	s3.Prefix = ""
	require.Equal(t, "images/x/y/z.tar.gz", s3ImageKey(s3, "x", "y", "z.tar.gz"))
}
