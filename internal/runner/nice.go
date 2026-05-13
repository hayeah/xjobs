package runner

import "golang.org/x/sys/unix"

// setpriority applies nice +n to a PID. macOS + Linux compatible.
func setpriority(pid, niceN int) error {
	return unix.Setpriority(unix.PRIO_PROCESS, pid, niceN)
}
