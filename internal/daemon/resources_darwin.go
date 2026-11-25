//go:build darwin

package daemon

import "golang.org/x/sys/unix"

func memTotal() int64 {
	if n := memTotalFromProc(); n > 0 {
		return n
	}
	// Darwin: hw.memsize via sysctl (SysctlUint64 is BSD/Darwin-only).
	if n, err := unix.SysctlUint64("hw.memsize"); err == nil {
		return int64(n)
	}
	return 0
}
