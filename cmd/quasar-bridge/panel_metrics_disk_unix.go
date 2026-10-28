//go:build linux || darwin

package main

import "syscall"

// stationDiskUsage reports the used and total megabytes of the filesystem that
// holds the bridge state directory. The station has no `df` worth parsing from a
// Go process, and statfs is one syscall with no output to decode.
func stationDiskUsage(path string) (usedMB, totalMB float64, err error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, 0, err
	}
	block := float64(stat.Bsize)
	total := float64(stat.Blocks) * block
	// Bavail, not Bfree: the reserved blocks are not available to the bridge.
	free := float64(stat.Bavail) * block
	return (total - free) / (1 << 20), total / (1 << 20), nil
}
