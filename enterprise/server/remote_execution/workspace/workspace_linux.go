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

// waitForTimestampTick waits until the kernel's coarse clock is past t so we
// can detect directory changes made by the action after we record fingerprints.
// Without multigrain timestamps, the kernel uses this clock for file timestamps
// and advances it only once per tick. If the action changes a directory in the
// same tick as setup's last change, the kernel assigns the same ctime we
// recorded, so we miss the action's change. We pass the newest recorded ctime
// as t and wait at most one tick before starting the action.
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
