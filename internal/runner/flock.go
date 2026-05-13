package runner

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// flockHandle is a held exclusive flock on a file. Release with Close.
type flockHandle struct {
	f *os.File
}

func (h *flockHandle) Close() error {
	if h == nil || h.f == nil {
		return nil
	}
	// LOCK_UN happens implicitly on close.
	err := h.f.Close()
	h.f = nil
	return err
}

// flockAcquire opens path (creating it if missing) and takes an exclusive
// non-blocking flock. Returns (handle, true) on success; (nil, false) on
// contention. A real error (open/stat) is returned as error.
func flockAcquire(path string) (*flockHandle, bool, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, false, fmt.Errorf("open lock %s: %w", path, err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		if err == unix.EWOULDBLOCK {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("flock %s: %w", path, err)
	}
	return &flockHandle{f: f}, true, nil
}

// flockAvailable reports whether path's exclusive flock can currently be
// acquired and released. Used by the reaper to detect stranded rows.
func flockAvailable(path string) (bool, error) {
	h, ok, err := flockAcquire(path)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, nil
	}
	h.Close()
	return true, nil
}

// acquireRunnerLock takes the exclusive flock on the per-state-dir runner
// lock file. On success, truncates the file and writes the current PID so
// a future contender can report the holder. On contention, parses
// whatever PID it can from the file (0 if unreadable/missing) and returns
// (nil, false, holderPID, nil).
func acquireRunnerLock(path string) (*flockHandle, bool, int, error) {
	h, ok, err := flockAcquire(path)
	if err != nil {
		return nil, false, 0, err
	}
	if !ok {
		holder, _ := readPIDFile(path)
		return nil, false, holder, nil
	}
	if err := writePIDToLock(h.f, os.Getpid()); err != nil {
		h.Close()
		return nil, false, 0, fmt.Errorf("write pid to %s: %w", path, err)
	}
	return h, true, 0, nil
}

func writePIDToLock(f *os.File, pid int) error {
	if err := f.Truncate(0); err != nil {
		return err
	}
	if _, err := f.Seek(0, 0); err != nil {
		return err
	}
	_, err := fmt.Fprintf(f, "%d\n", pid)
	return err
}

func readPIDFile(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	var pid int
	if _, err := fmt.Sscanf(string(data), "%d", &pid); err != nil {
		return 0, err
	}
	return pid, nil
}
