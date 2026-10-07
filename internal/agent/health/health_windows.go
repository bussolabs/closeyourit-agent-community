//go:build windows

package health

import "os"

// Windows has neither O_NOFOLLOW nor a uid in the stat result: the data dir is per-user there.
const noFollow = 0

func ownedByCurrentUser(os.FileInfo) bool { return true }
