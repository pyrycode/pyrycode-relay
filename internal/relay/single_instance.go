package relay

import "errors"

// envSingleInstanceBypass is the env var that asserts explicit operator
// intent to run a single instance on a multi-instance-capable platform.
// Contract is "1" means assert; anything else is treated as unset. Pinned
// by docs/architecture.md § Single-instance constraint.
const envSingleInstanceBypass = "PYRYCODE_RELAY_SINGLE_INSTANCE"

// envFlyAppName is the Fly.io platform-set signal: non-empty on every
// machine in a Fly app, which is also the substrate where
// `fly scale count > 1` is a one-line operator command. Presence is the
// heuristic that we are on a multi-instance-capable platform.
const envFlyAppName = "FLY_APP_NAME"

// ErrMultiInstanceDeployDetected is returned by CheckSingleInstance when
// the relay detects it is running on a multi-instance-capable platform
// (today: Fly.io) and the operator has not asserted single-instance intent
// via PYRYCODE_RELAY_SINGLE_INSTANCE=1.
//
// The connection registry is in-memory per process, so two replicas hold
// disjoint server-id tables and phones routed to the wrong replica close
// with 4404 even though the binary is online. The deterministic backstop
// to the docs/architecture.md prose half lives here.
var ErrMultiInstanceDeployDetected = errors.New("relay: multi-instance deploy detected; set PYRYCODE_RELAY_SINGLE_INSTANCE=1 to bypass")

// CheckSingleInstance returns ErrMultiInstanceDeployDetected when the
// bypass env var is not exactly "1" AND a multi-instance-capable platform
// signal is present (FLY_APP_NAME non-empty). Returns nil otherwise.
// Intended to be called from main after flag parse, before any listener
// is started.
//
// Bypass wins unconditionally: when PYRYCODE_RELAY_SINGLE_INSTANCE=1 the
// platform signal is not consulted. Only the exact string "1" bypasses;
// other values (e.g. "true", " 1") are rejected earlier by CheckEnvConfig
// via the env-config registry, but CheckSingleInstance treats them as
// "not set" defensively.
//
// getenv is the env-var lookup function; pass os.Getenv at the call site,
// an injected func in tests.
func CheckSingleInstance(getenv func(string) string) error {
	if getenv(envSingleInstanceBypass) == "1" {
		return nil
	}
	if getenv(envFlyAppName) != "" {
		return ErrMultiInstanceDeployDetected
	}
	return nil
}
