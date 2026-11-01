package shadow

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// FreeBytes returns the number of free bytes available to an unprivileged
// caller on the filesystem that holds path (creates path if missing so Statfs
// can resolve a mount point).
func FreeBytes(path string) (int64, error) {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return 0, err
	}
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, fmt.Errorf("shadow: failed to stat filesystem for %s: %w", path, err)
	}
	// Prefer Bavail (unprivileged free) over Bfree (includes reserved blocks).
	return int64(st.Bavail) * int64(st.Bsize), nil
}
