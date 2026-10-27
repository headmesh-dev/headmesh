// Package bootid identifies one container lifetime. Processes and tmux
// windows can't survive a container restart, so anything stamped with an
// older boot identity is reported as lost.
package bootid

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// clockTicks is USER_HZ, which is 100 on every Linux architecture Go targets.
const clockTicks = 100

// Identity is the kernel boot plus the start of PID 1 in this PID namespace.
type Identity struct {
	BootID           string    `json:"boot_id"`
	ContainerStarted time.Time `json:"container_started_at"`
	// Key combines both; compare keys, not fields.
	Key string `json:"key"`
}

// Source reads the identity. Tests substitute their own.
type Source func() (Identity, error)

// Proc reads /proc of the current PID namespace.
func Proc() (Identity, error) { return read("/proc") }

func read(procRoot string) (Identity, error) {
	raw, err := os.ReadFile(procRoot + "/sys/kernel/random/boot_id") //nolint:gosec // G304: /proc, or a fake in tests
	if err != nil {
		return Identity{}, fmt.Errorf("boot_id: %w", err)
	}
	bootID := strings.TrimSpace(string(raw))
	stat, err := os.ReadFile(procRoot + "/1/stat") //nolint:gosec // G304: /proc, or a fake in tests
	if err != nil {
		return Identity{}, fmt.Errorf("pid 1 stat: %w", err)
	}
	ticks, err := StartTicks(stat)
	if err != nil {
		return Identity{}, err
	}
	btime, err := bootTime(procRoot)
	if err != nil {
		return Identity{}, err
	}
	started := btime.Add(time.Duration(ticks) * time.Second / clockTicks) //nolint:gosec // G115: ticks since boot fit in int64 for 2900 years
	return Identity{
		BootID:           bootID,
		ContainerStarted: started.UTC(),
		Key:              fmt.Sprintf("%s/%d", bootID, ticks),
	}, nil
}

// StartTicks parses field 22 (starttime) of a /proc/<pid>/stat line. The
// command name in field 2 may contain spaces and parentheses, so parsing
// starts after the last ')'.
func StartTicks(stat []byte) (uint64, error) {
	i := bytes.LastIndexByte(stat, ')')
	if i < 0 {
		return 0, fmt.Errorf("malformed stat line")
	}
	fields := strings.Fields(string(stat[i+1:]))
	// fields[0] is field 3 (state); starttime is field 22.
	const idx = 22 - 3
	if len(fields) <= idx {
		return 0, fmt.Errorf("stat line has %d fields after comm", len(fields))
	}
	v, err := strconv.ParseUint(fields[idx], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("starttime: %w", err)
	}
	return v, nil
}

// ProcessStartTicks returns the start time of pid in clock ticks since boot,
// used to tell a live process from a reused PID.
func ProcessStartTicks(pid int) (uint64, error) {
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, fmt.Errorf("stat pid %d: %w", pid, err)
	}
	return StartTicks(stat)
}

func bootTime(procRoot string) (time.Time, error) {
	f, err := os.Open(procRoot + "/stat") //nolint:gosec // G304: path built by sandboxd or chosen by the calling agent
	if err != nil {
		return time.Time{}, fmt.Errorf("proc stat: %w", err)
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if rest, ok := strings.CutPrefix(sc.Text(), "btime "); ok {
			secs, err := strconv.ParseInt(strings.TrimSpace(rest), 10, 64)
			if err != nil {
				return time.Time{}, fmt.Errorf("btime: %w", err)
			}
			return time.Unix(secs, 0), nil
		}
	}
	return time.Time{}, fmt.Errorf("btime not found in %s/stat", procRoot)
}
