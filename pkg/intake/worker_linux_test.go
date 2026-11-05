package intake

import (
	"strings"
	"syscall"
	"testing"
)

func saveDataLimit(t *testing.T) func() {
	t.Helper()
	var lim syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_DATA, &lim); err != nil {
		t.Fatalf("Getrlimit: %v", err)
	}
	return func() {
		if err := syscall.Setrlimit(syscall.RLIMIT_DATA, &lim); err != nil {
			t.Errorf("restoring RLIMIT_DATA: %v", err)
		}
	}
}

func TestLimitDataSegmentIsRelative(t *testing.T) {
	t.Cleanup(saveDataLimit(t))
	if err := limitDataSegment(1 << 50); err != nil {
		t.Fatalf("limitDataSegment: %v", err)
	}
	var lim syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_DATA, &lim); err != nil {
		t.Fatalf("Getrlimit: %v", err)
	}
	if lim.Cur < 1<<50 && lim.Cur != lim.Max {
		t.Fatalf("RLIMIT_DATA soft = %d, want at least the budget or the hard limit", lim.Cur)
	}
}

func TestParseVmData(t *testing.T) {
	got, err := parseVmData(strings.NewReader("Name:\tdibs\nVmData:\t   2048 kB\nVmStk:\t 132 kB\n"))
	if err != nil || got != 2048<<10 {
		t.Fatalf("parseVmData = %d, %v; want %d", got, err, 2048<<10)
	}
	if _, err := parseVmData(strings.NewReader("VmData:\tlots kB\n")); err == nil {
		t.Fatal("parseVmData accepted a non-numeric VmData")
	}
	if _, err := parseVmData(strings.NewReader("Name:\tdibs\n")); err == nil {
		t.Fatal("parseVmData accepted status without VmData")
	}
	if _, err := parseVmData(failingIO{}); err == nil {
		t.Fatal("parseVmData ignored a read error")
	}
}
