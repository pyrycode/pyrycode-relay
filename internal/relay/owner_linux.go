package relay

import (
	"os"
	"syscall"
)

// fileOwnerUID returns the numeric UID of the file's owner from a stat
// result, if discoverable. Linux-only impl reads syscall.Stat_t.Uid;
// see owner_other.go for the non-Linux fallback. ok=false means "the
// platform did not return a recognisable stat shape" — callers must
// treat that as "ownership unknown, do not silently relax checks".
func fileOwnerUID(info os.FileInfo) (uint32, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return st.Uid, true
}
