package relay

import "errors"

// envProductionMode is the env var that signals production mode. The
// contract is "1" means on; anything else means off. Mirrors the shape
// of PYRYCODE_RELAY_SINGLE_INSTANCE so operators only have to remember
// one pattern.
const envProductionMode = "PYRYCODE_RELAY_PRODUCTION"

// ErrInsecureListenInProduction is returned by CheckInsecureListenInProduction
// when the relay is configured for production mode (PYRYCODE_RELAY_PRODUCTION=1)
// AND the --insecure-listen flag is set. Serving plaintext traffic from a
// production-tagged process is a fail-fast misconfiguration, not a runtime
// degradation: the relay refuses to start so a dev manifest accidentally
// promoted to prod fails the deploy's health check rather than serving traffic.
var ErrInsecureListenInProduction = errors.New("relay: --insecure-listen is set with PYRYCODE_RELAY_PRODUCTION=1; refusing to start")

// IsProductionMode reports whether the relay is in production mode.
//
// The contract is: PYRYCODE_RELAY_PRODUCTION="1" means production; any other
// value (including unset, "0", "true", "yes", "PRODUCTION", or whitespace)
// means non-production. Only the exact string "1" enables production mode.
// The strictness is intentional: anything fuzzier ("truthy" parsing) creates
// a class of subtle misconfigurations where an operator believes production
// mode is on but the relay disagrees.
//
// The env var is read on every call via getenv. Pass os.Getenv at call sites;
// tests inject a func to exercise the matrix without mutating process env.
func IsProductionMode(getenv func(string) string) bool {
	return getenv(envProductionMode) == "1"
}

// CheckInsecureListenInProduction returns ErrInsecureListenInProduction when
// production mode is on (per IsProductionMode) AND insecureListen is non-empty.
// Returns nil otherwise. Intended to be called from main after flag parse,
// before any listener is started.
//
// getenv is the env-var lookup function; pass os.Getenv at the call site,
// an injected func in tests.
func CheckInsecureListenInProduction(insecureListen string, getenv func(string) string) error {
	if IsProductionMode(getenv) && insecureListen != "" {
		return ErrInsecureListenInProduction
	}
	return nil
}

// ErrRunningAsRoot is returned by CheckRunningAsRoot when the relay is
// configured for production mode (PYRYCODE_RELAY_PRODUCTION=1) AND the
// effective uid is 0. Running an internet-exposed process as root in
// production is a fail-fast misconfiguration, not a runtime degradation:
// a `docker run --user 0` at deploy time or a missing/overridden USER
// directive escapes the CI non-root-build check, and any RCE in the relay
// would then escalate to a root RCE on the host. The relay refuses to
// start so the misconfigured deploy fails its health check rather than
// serving traffic.
var ErrRunningAsRoot = errors.New("relay: effective uid is 0 with PYRYCODE_RELAY_PRODUCTION=1; refusing to start")

// CheckRunningAsRoot returns ErrRunningAsRoot when production mode is on
// (per IsProductionMode) AND geteuid returns 0. Returns nil otherwise.
// Intended to be called from main after flag parse, before any listener
// is started.
//
// geteuid is the effective-uid lookup function; pass syscall.Geteuid at
// the call site, an injected func in tests. The seam exists because no
// stdlib equivalent of t.Setenv exists for uid (a process cannot change
// its own euid mid-test without re-exec), so the uid-0 branch can only
// be exercised in a unit test via an injected function.
//
// getenv is the env-var lookup function; pass os.Getenv at the call
// site, an injected func in tests.
func CheckRunningAsRoot(geteuid func() int, getenv func(string) string) error {
	if IsProductionMode(getenv) && geteuid() == 0 {
		return ErrRunningAsRoot
	}
	return nil
}
