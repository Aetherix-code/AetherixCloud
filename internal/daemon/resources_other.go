//go:build !darwin

package daemon

// memTotal reads MemTotal from /proc/meminfo (Linux, including DinD nodes).
func memTotal() int64 {
	return memTotalFromProc()
}
