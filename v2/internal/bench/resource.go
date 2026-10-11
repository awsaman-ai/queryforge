package bench

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Resources is one point-in-time reading of what the architecture costs to
// keep warm. Section 17 of the brief asks for CPU usage and resident memory;
// both are split between the two processes involved, because the Go process
// alone would understate the architecture by the size of the model.
type Resources struct {
	// GoHeapBytes is live heap in the Go process (runtime.MemStats.HeapAlloc):
	// the index, the corpus and the config.
	GoHeapBytes uint64 `json:"go_heap_bytes"`
	// GoSysBytes is total memory obtained from the OS by the Go runtime.
	GoSysBytes uint64 `json:"go_sys_bytes"`
	// GoPeakRSSBytes is the peak resident set of the Go process, from
	// getrusage. Peak, not current -- the kernel does not hand back a current
	// RSS through rusage.
	GoPeakRSSBytes int64 `json:"go_peak_rss_bytes"`
	// GoCPU is user+system CPU consumed by the Go process so far.
	GoCPU time.Duration `json:"go_cpu"`

	// WorkerPID is the embedding worker's process id, or 0 when no worker is
	// running (the deterministic provider, or a run that never started one).
	WorkerPID int `json:"worker_pid"`
	// WorkerRSSBytes is the worker's CURRENT resident set, read from ps. This
	// is where the model's weights live, so it dominates the total.
	WorkerRSSBytes int64 `json:"worker_rss_bytes"`
	// WorkerCPU is cumulative CPU consumed by the worker process.
	WorkerCPU time.Duration `json:"worker_cpu"`
	// WorkerErr records why a worker reading is missing, rather than
	// reporting a silent zero.
	WorkerErr string `json:"worker_error,omitempty"`
}

// TotalRSSBytes is the architecture's resident cost: the Go process's peak
// plus the worker's current. The two come from different sources (rusage vs
// ps), which is why they are also reported separately.
func (r Resources) TotalRSSBytes() int64 { return r.GoPeakRSSBytes + r.WorkerRSSBytes }

// TotalCPU is CPU across both processes.
func (r Resources) TotalCPU() time.Duration { return r.GoCPU + r.WorkerCPU }

// Sample reads the current resource usage. workerPID may be 0.
//
// It never fails: a worker reading that cannot be taken is recorded in
// WorkerErr, because a benchmark that aborts at the last measurement has
// thrown away the whole run.
func Sample(ctx context.Context, workerPID int) Resources {
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)

	res := Resources{
		GoHeapBytes: mem.HeapAlloc,
		GoSysBytes:  mem.Sys,
		WorkerPID:   workerPID,
	}

	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err == nil {
		res.GoPeakRSSBytes = maxRSSBytes(ru.Maxrss)
		res.GoCPU = timevalDuration(ru.Utime) + timevalDuration(ru.Stime)
	}

	if workerPID > 0 {
		rss, cpu, err := ProcessUsage(ctx, workerPID)
		if err != nil {
			res.WorkerErr = err.Error()
		} else {
			res.WorkerRSSBytes, res.WorkerCPU = rss, cpu
		}
	}
	return res
}

// maxRSSBytes normalises getrusage's ru_maxrss, whose UNIT DIFFERS BY
// PLATFORM: bytes on darwin and the BSDs, kilobytes on Linux. Getting this
// wrong misreports memory by 1024x in either direction, which is the kind of
// error a reader cannot spot in a finished table.
func maxRSSBytes(maxrss int64) int64 {
	if maxrss <= 0 {
		return 0
	}
	if runtime.GOOS == "linux" {
		return maxrss * 1024
	}
	return maxrss
}

func timevalDuration(tv syscall.Timeval) time.Duration {
	return time.Duration(tv.Sec)*time.Second + time.Duration(tv.Usec)*time.Microsecond
}

// ProcessUsage reads one process's resident set and cumulative CPU from ps.
//
// ps is used rather than a /proc read because this module targets darwin as
// well as linux, and because the worker is a CHILD THAT IS STILL RUNNING:
// getrusage(RUSAGE_CHILDREN) only accounts for children that have been
// reaped, so it reports zero for exactly the process whose memory matters
// most here.
func ProcessUsage(ctx context.Context, pid int) (rssBytes int64, cpu time.Duration, err error) {
	if pid <= 0 {
		return 0, 0, fmt.Errorf("no process id")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, "ps", "-o", "rss=,time=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0, 0, fmt.Errorf("ps for pid %d: %w", pid, err)
	}
	fields := strings.Fields(string(out))
	if len(fields) < 2 {
		return 0, 0, fmt.Errorf("ps for pid %d returned %q", pid, strings.TrimSpace(string(out)))
	}
	kb, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("ps rss %q: %w", fields[0], err)
	}
	cpu, err = ParsePSTime(fields[1])
	if err != nil {
		return 0, 0, err
	}
	// ps reports rss in kilobytes on both platforms this runs on.
	return kb * 1024, cpu, nil
}

// ParsePSTime reads ps's cumulative CPU column, which appears as
// "[[dd-]hh:]mm:ss[.ff]" -- "0:00.00" on darwin, "00:00:00" on linux, and
// "2-01:02:03" for a process that has been running for days.
func ParsePSTime(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty cpu time")
	}

	var days int64
	if i := strings.Index(s, "-"); i >= 0 {
		d, err := strconv.ParseInt(s[:i], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("cpu time %q: %w", s, err)
		}
		days, s = d, s[i+1:]
	}

	parts := strings.Split(s, ":")
	if len(parts) > 3 {
		return 0, fmt.Errorf("cpu time %q has too many fields", s)
	}

	// Seconds are last and may carry a fraction; whole fields precede them.
	seconds, err := strconv.ParseFloat(parts[len(parts)-1], 64)
	if err != nil {
		return 0, fmt.Errorf("cpu time %q: %w", s, err)
	}
	total := time.Duration(seconds * float64(time.Second))

	unit := time.Minute
	for i := len(parts) - 2; i >= 0; i-- {
		v, err := strconv.ParseInt(parts[i], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("cpu time %q: %w", s, err)
		}
		total += time.Duration(v) * unit
		unit *= 60
	}
	return total + time.Duration(days)*24*time.Hour, nil
}
