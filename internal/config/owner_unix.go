//go:build !windows

package config

import (
	"os"
	"syscall"
)

// fileOwner returns the file's uid/gid (unix); Windows has none.
func fileOwner(fi os.FileInfo) (int, int, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return int(st.Uid), int(st.Gid), true
}
