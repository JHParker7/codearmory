//go:build linux

package main

import "syscall"

// diskUsage reports the total and available bytes of the filesystem backing path.
//
// Available, not free: the reserved-for-root blocks are counted as used, because the
// pipeline does not run as root and a build fails when it runs out of *available*
// space, not when the filesystem reaches literal zero.
func diskUsage(path string) (total, avail uint64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	bs := uint64(st.Bsize)
	return st.Blocks * bs, st.Bavail * bs, nil
}
