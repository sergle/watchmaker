//go:build linux && (amd64 || arm64)

package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/busybox-org/watchmaker"
)

var (
	pid           uint64
	fakeTime      string
	clockIdsSlice string
)

func init() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	log.SetOutput(os.Stdout)
}

func main() {
	var clockIdsSliceDefault string
	if runtime.GOARCH == "arm64" {
		// on modern arm64 there is no __NR_time syscall;
		// glibc is using clock_gettime() wrapper [1] with CLOCK_REALTIME_COARSE clockid [2]
		//
		// [1] https://sourceware.org/git/?p=glibc.git;a=blob;f=time/time.c;h=d5dcb2e7ed83bc491ed026caf914caf4f1ae9202;hb=c804cd1c00adde061ca51711f63068c103e94eef
		// [2] https://sourceware.org/git/?p=glibc.git;a=blob;f=sysdeps/unix/sysv/linux/time-clockid.h;h=91543b69e47ce2828316ff0b3361ec435159690e;hb=c804cd1c00adde061ca51711f63068c103e94eef
		clockIdsSliceDefault = "CLOCK_REALTIME,CLOCK_REALTIME_COARSE"
	} else {
		clockIdsSliceDefault = "CLOCK_REALTIME"
	}

	flag.Uint64Var(&pid, "pid", 0, "pid of target program")
	flag.StringVar(&fakeTime, "faketime", "", "fake time (incremental/absolute value)")
	flag.StringVar(&clockIdsSlice, "clockids", "", "clockids to modify, default is "+clockIdsSliceDefault)
	flag.Parse()

	if pid <= 0 {
		log.Fatalln("pid can't is zero")
	}
	if fakeTime == "" {
		log.Fatalln("faketime can't is empty")
	}
	if clockIdsSlice == "" {
		clockIdsSlice = clockIdsSliceDefault
	}
	log.Println("pid:", pid, "faketime:", fakeTime, "clockids:", clockIdsSlice)

	offsetTime, err := watchmaker.CalculateOffset(fakeTime)
	if err != nil {
		log.Fatalln(err)
	}

	clkIds, err := watchmaker.EncodeClkIds(strings.Split(clockIdsSlice, ","))
	if err != nil {
		log.Fatalln(err)
	}

	// Split the offset into whole seconds + sub-second nanoseconds instead of
	// passing the entire offset as nanoseconds.
	//
	// Why this matters: the injected fake_clock_gettime.c / fake_gettimeofday.c
	// normalize the nanosecond delta with a `while` loop that subtracts 1e9 one
	// iteration at a time. If the whole offset is handed over as nanoseconds,
	// that loop runs (offset_in_seconds) times on EVERY clock_gettime/
	// gettimeofday call the target makes — e.g. ~31.5M iterations for a +1y
	// skew — which makes the target process visibly slow.
	//
	// By keeping the nanosecond delta sub-second here, |nsec_delta| < 1e9, so the
	// normalize loop runs at most once per call regardless of offset size.
	//
	// NOTE: this fixes it at the call site only. The C payload itself is still
	// O(offset) if some other caller passes a >= 1e9 nanosecond delta (e.g. via
	// Config.Merge stacking deltas, or a direct NewConfig). The robust fix would
	// be to replace those `while` loops with div/mod (as fake_time.c already
	// does); we rely on the split below instead since watchmaker is only used as
	// a one-shot CLI.
	deltaSeconds := int64(offsetTime / time.Second)
	deltaNanoSeconds := int64(offsetTime % time.Second)

	skew, err := watchmaker.GetSkew(watchmaker.NewConfig(deltaSeconds, deltaNanoSeconds, clkIds))
	if err != nil {
		log.Fatalln(err)
	}
	log.Printf("modifying time, pid: %v", pid)
	err = skew.Inject(pid)
	if err != nil {
		log.Fatalln(err)
	}
	log.Println("modifying time success")

	childPIDs, err := getChildProcesses(pid)
	if err != nil {
		log.Fatalln(err)
	}
	if len(childPIDs) == 0 {
		return
	}
	log.Printf("modifying child time, pids: %v", childPIDs)
	for _, _childPid := range childPIDs {
		var skewFork *watchmaker.Skew
		skewFork, err = skew.Fork()
		if err != nil {
			log.Println(err)
			continue
		}
		err = skewFork.Inject(_childPid)
		if err != nil {
			log.Println(err)
		}
	}
	log.Println("modifying child time success")
}

const DefaultProcPrefix = "/proc"

// GetChildProcesses will return all child processes's pid. Include all generations.
// only return error when /proc/pid/tasks cannot be read
func getChildProcesses(ppid uint64) ([]uint64, error) {
	procs, err := os.ReadDir(DefaultProcPrefix)
	if err != nil {
		return nil, fmt.Errorf("%T read /proc/pid/tasks , ppid : %d", err, ppid)
	}

	pidMap := make(map[uint64][]uint64) // Map of parent PID to child PIDs
	var mu sync.Mutex                   // Mutex for synchronizing map writes
	var wg sync.WaitGroup               // WaitGroup to manage goroutines

	for _, proc := range procs {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			processStat(&mu, name, pidMap)
		}(proc.Name())
	}

	wg.Wait()

	// Collect all child PIDs recursively starting from the given ppid.
	result := collectAllChildren(ppid, pidMap)
	return result, nil
}

func collectAllChildren(ppid uint64, pidMap map[uint64][]uint64) []uint64 {
	var result []uint64
	for _, child := range pidMap[ppid] {
		result = append(result, child)
		result = append(result, collectAllChildren(child, pidMap)...)
	}
	return result
}

// processStat parses a process's stat file and updates the pidMap with parent-child relationships.
func processStat(mu *sync.Mutex, name string, pidMap map[uint64][]uint64) {
	_pid, err := strconv.ParseUint(name, 10, 64)
	if err != nil {
		return
	}

	statusPath := filepath.Join(DefaultProcPrefix, name, "stat")
	reader, err := os.Open(statusPath)
	if err != nil {
		return
	}
	defer reader.Close()

	var (
		ppid  uint64
		comm  string
		state string
	)
	// according to procfs's man page
	_, err = fmt.Fscanf(reader, "%d %s %s %d", &_pid, &comm, &state, &ppid)
	if err != nil {
		return
	}
	mu.Lock()
	defer mu.Unlock()
	pidMap[ppid] = append(pidMap[ppid], _pid)
}
