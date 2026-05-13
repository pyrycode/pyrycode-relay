//go:build !linux

package relay

import (
	"fmt"
	"log/slog"
	"runtime"
)

// CheckCapabilities is a no-op on non-Linux platforms. The /proc/self/status
// path the Linux variant reads exists only on Linux; on darwin/Windows/BSD
// there is no capability model to check, so the function returns nil and
// logs a one-line note that the check was skipped. The log emits via
// slog.Default(), so the caller's slog handler captures it alongside the
// rest of startup output.
//
// GOOS is embedded in the msg rather than as a structured field because
// the log-key allowlist (log_allowlist.go) gates structured keys; the
// AC's literal message format ("skipping linux-only capability check on
// <GOOS>") keeps the operator-visible string identical without growing
// the allowlist for a startup-only diagnostic.
func CheckCapabilities() error {
	slog.Default().Info(fmt.Sprintf("skipping linux-only capability check on %s", runtime.GOOS))
	return nil
}
