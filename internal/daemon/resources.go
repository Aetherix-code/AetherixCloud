package daemon

import (
	"bufio"
	"os"
	goruntime "runtime"
	"strconv"
	"strings"
	"sync"

	v1 "github.com/aetherix/aetherixcloud/gen/aetherix/v1"
	"golang.org/x/sys/unix"
)

// capacity reports this machine's CPU/memory/disk capacity (best effort).
func capacity() *v1.Resources {
	r := &v1.Resources{CpuMillis: int64(goruntime.NumCPU()) * 1000}
	if mem := memTotal(); mem > 0 {
		r.MemoryBytes = mem
	}
	if total, _ := diskUsage("/"); total > 0 {
		r.DiskBytes = total
	}
	return r
}

// usage reports current CPU/memory/disk use (best effort). CPU is a delta over
// successive samples of /proc/stat (or load average when /proc is absent).
func usage() *v1.Resources {
	r := &v1.Resources{}
	r.CpuMillis = cpuUsedMillis()
	if used := memUsed(); used > 0 {
		r.MemoryBytes = used
	}
	if total, free := diskUsage("/"); total > 0 {
		r.DiskBytes = total - free
	}
	return r
}

func memTotalFromProc() int64 {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "MemTotal:") {
			return parseMemKB(sc.Text())
		}
	}
	return 0
}

func memUsed() int64 {
	total, avail := int64(0), int64(-1)
	if f, err := os.Open("/proc/meminfo"); err == nil {
		defer f.Close()
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "MemTotal:"):
				total = parseMemKB(line)
			case strings.HasPrefix(line, "MemAvailable:"):
				avail = parseMemKB(line)
			}
		}
		if total > 0 && avail >= 0 {
			return total - avail
		}
	}
	return 0
}

func parseMemKB(line string) int64 {
	fs := strings.Fields(line)
	if len(fs) < 2 {
		return 0
	}
	kb, err := strconv.ParseInt(fs[1], 10, 64)
	if err != nil {
		return 0
	}
	return kb * 1024
}

func diskUsage(path string) (total, free int64) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, 0
	}
	bs := int64(st.Bsize)
	if bs <= 0 {
		return 0, 0
	}
	return int64(st.Blocks) * bs, int64(st.Bavail) * bs
}

var (
	cpuMu       sync.Mutex
	cpuPrevBusy uint64
	cpuPrevTot  uint64
	cpuLast     int64
)

func cpuUsedMillis() int64 {
	busy, total, ok := readProcStat()
	if ok {
		cpuMu.Lock()
		defer cpuMu.Unlock()
		if cpuPrevTot > 0 && total > cpuPrevTot {
			db, dt := busy-cpuPrevBusy, total-cpuPrevTot
			if dt > 0 {
				n := int64(goruntime.NumCPU()) * 1000
				cpuLast = int64(float64(n) * float64(db) / float64(dt))
				if cpuLast > n {
					cpuLast = n
				}
				if cpuLast < 0 {
					cpuLast = 0
				}
			}
		}
		cpuPrevBusy, cpuPrevTot = busy, total
		return cpuLast
	}
	// No /proc/stat (e.g. macOS host daemon): leave CPU use at 0.
	return 0
}

func readProcStat() (busy, total uint64, ok bool) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return 0, 0, false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	if !sc.Scan() {
		return 0, 0, false
	}
	fs := strings.Fields(sc.Text())
	if len(fs) < 5 || fs[0] != "cpu" {
		return 0, 0, false
	}
	var vals [8]uint64
	for i := 0; i < len(vals) && i+1 < len(fs); i++ {
		vals[i], _ = strconv.ParseUint(fs[i+1], 10, 64)
	}
	// user nice system idle iowait irq softirq steal
	idle := vals[3] + vals[4]
	for _, v := range vals {
		total += v
	}
	busy = total - idle
	return busy, total, true
}
