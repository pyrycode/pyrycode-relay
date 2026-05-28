//go:build !linux

package relay

import "os"

// fileOwnerUID is unsupported on non-Linux platforms. Returning
// ok=false routes callers into their existing "ownership unknown"
// branch — for NewAutocertManager, that is the unchanged
// ErrCacheDirInsecure rejection path.
func fileOwnerUID(_ os.FileInfo) (uint32, bool) { return 0, false }
