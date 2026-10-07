//go:build !windows

package health

import (
	"os"
	"syscall"
)

const noFollow = syscall.O_NOFOLLOW

func ownedByCurrentUser(info os.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Getuid()
}
