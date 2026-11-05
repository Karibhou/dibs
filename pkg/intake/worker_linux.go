package intake

import (
	"bufio"
	"errors"
	"io"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// limitDataSegment caps the process's writable private memory (RLIMIT_DATA)
// at what it has already mapped plus budget, so heap growth past the budget
// fails and the Go runtime aborts. The budget is relative because some
// runtimes (e.g. the race detector's shadow memory) map large regions up
// front.
func limitDataSegment(budget int64) error {
	f, err := os.Open("/proc/self/status")
	if err != nil {
		return err
	}
	defer f.Close()
	used, err := parseVmData(f)
	if err != nil {
		return err
	}
	var lim syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_DATA, &lim); err != nil {
		return err
	}
	lim.Cur = min(uint64(used+budget), lim.Max)
	return syscall.Setrlimit(syscall.RLIMIT_DATA, &lim)
}

// parseVmData returns the VmData line of /proc/<pid>/status in bytes.
func parseVmData(status io.Reader) (int64, error) {
	sc := bufio.NewScanner(status)
	for sc.Scan() {
		rest, ok := strings.CutPrefix(sc.Text(), "VmData:")
		if !ok {
			continue
		}
		kb, err := strconv.ParseInt(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(rest), "kB")), 10, 64)
		if err != nil {
			return 0, err
		}
		return kb << 10, nil
	}
	if err := sc.Err(); err != nil {
		return 0, err
	}
	return 0, errors.New("VmData not found")
}
