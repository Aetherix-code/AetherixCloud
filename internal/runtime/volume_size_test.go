package runtime

import "testing"

func TestFormatDockerSize(t *testing.T) {
	cases := map[int64]string{
		0:           "0B",
		512:         "512B",
		1024:        "1K",
		10 << 20:    "10M",
		10 << 30:    "10G",
		1 << 40:     "1T",
		(10 << 30) + 1: "10737418241B",
	}
	for n, want := range cases {
		if got := formatDockerSize(n); got != want {
			t.Errorf("formatDockerSize(%d) = %q, want %q", n, got, want)
		}
	}
}
