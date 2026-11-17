//go:build unix

package config

import (
	"os"
	"syscall"
)

// fileOwnerUID returns the owning user id of fi.
func fileOwnerUID(fi os.FileInfo) (uint32, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return st.Uid, true
}

// effectiveUID returns the process's effective user id.
func effectiveUID() uint32 { return uint32(os.Geteuid()) }
