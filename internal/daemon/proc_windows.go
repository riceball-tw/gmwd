package daemon

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// stillActive is the exit code GetExitCodeProcess reports for a live process.
const stillActive = 259

func processAlive(pid int) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		// It exists, we just may not look at it.
		return errors.Is(err, windows.ERROR_ACCESS_DENIED)
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	return code == stillActive
}

// Windows locks are mandatory, so lock a byte far past the pid we write;
// a second daemon can then still read the pid to report it.
const lockOffsetHigh = 1 // 4 GiB in

// lockFile takes a non-blocking exclusive lock on f.
func lockFile(f *os.File) error {
	ol := windows.Overlapped{OffsetHigh: lockOffsetHigh}
	return windows.LockFileEx(windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &ol)
}

func unlockFile(f *os.File) {
	ol := windows.Overlapped{OffsetHigh: lockOffsetHigh}
	windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &ol)
}
