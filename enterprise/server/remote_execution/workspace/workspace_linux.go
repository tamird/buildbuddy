//go:build linux && !android

package workspace

import (
	"fmt"
	"time"

	"golang.org/x/sys/unix"
)

// lstatDir returns the fingerprint of the directory at path, and false if path
// isn't a directory.
func lstatDir(path string) (dirFingerprint, bool, error) {
	var st unix.Stat_t
	if err := unix.Lstat(path, &st); err != nil {
		return dirFingerprint{}, false, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		return dirFingerprint{}, false, nil
	}
	return dirFingerprint{
		ino:        st.Ino,
		ctimeNanos: st.Ctim.Nano(),
		mode:       st.Mode,
		uid:        st.Uid,
		gid:        st.Gid,
	}, true, nil
}

// waitForTimestampTick waits until the clock that the kernel uses for file
// timestamps is past t. Without multigrain timestamps, the kernel stamps
// changes with a coarse clock that only advances once per tick, so a change
// made in the same tick as the last change before a fingerprint was recorded
// would leave the directory's ctime as recorded. Waiting takes at most one
// tick.
func waitForTimestampTick(t int64) error {
	for {
		var now unix.Timespec
		if err := unix.ClockGettime(unix.CLOCK_REALTIME_COARSE, &now); err != nil {
			return fmt.Errorf("read coarse clock: %w", err)
		}
		if now.Nano() > t {
			return nil
		}
		time.Sleep(500 * time.Microsecond)
	}
}
