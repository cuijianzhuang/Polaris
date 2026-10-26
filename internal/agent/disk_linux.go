//go:build linux

package agent

import "syscall"

// readDisk is the used share of the filesystem at path (as df shows it:
// used / (used + available to unprivileged users)) and its size in bytes.
func readDisk(path string) (float64, uint64) {
	var st syscall.Statfs_t
	if syscall.Statfs(path, &st) != nil || st.Blocks == 0 {
		return 0, 0
	}
	bs := uint64(st.Bsize)
	used := (st.Blocks - st.Bfree) * bs
	avail := st.Bavail * bs
	if used+avail == 0 {
		return 0, st.Blocks * bs
	}
	return round1(100 * float64(used) / float64(used+avail)), st.Blocks * bs
}
