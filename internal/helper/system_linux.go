package helper

import (
	"os"
	"syscall"
)

const syscallNoFollow = syscall.O_NOFOLLOW

// statUID returns the owner of a file.
func statUID(fi os.FileInfo) (int, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(st.Uid), true
}
