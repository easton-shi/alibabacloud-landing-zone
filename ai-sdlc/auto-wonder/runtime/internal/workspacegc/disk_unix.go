//go:build !windows

package workspacegc

import "syscall"

func ReadDiskUsage(path string) (DiskUsage, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return DiskUsage{}, err
	}
	return DiskUsage{
		Total: stat.Blocks * uint64(stat.Bsize),
		Free:  stat.Bavail * uint64(stat.Bsize),
	}, nil
}
