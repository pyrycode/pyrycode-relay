package relay

import (
	"errors"
	"fmt"
)

// ErrInvalidConfigSentinel is the package-level sentinel that every
// *ErrInvalidConfig matches against via errors.Is. Detect any env-var
// validation failure with errors.Is(err, ErrInvalidConfigSentinel); extract
// the offending key and reason with errors.As(err, &cfgErr) where cfgErr is
// *ErrInvalidConfig.
var ErrInvalidConfigSentinel = errors.New("relay: invalid env-var config")

// ErrInvalidConfig is the structured error returned by CheckEnvConfig when a
// registered env var is missing-but-required or present-but-malformed. It
// exposes the offending env-var name and a human-readable reason so the
// caller can log a per-key remediation message at boot.
//
// Branchable via errors.Is(err, ErrInvalidConfigSentinel). Fields are
// extracted via errors.As(err, &cfgErr).
type ErrInvalidConfig struct {
	Key    string // env-var name (e.g. "PYRYCODE_RELAY_PRODUCTION")
	Reason string // human-readable shape violation
}

func (e *ErrInvalidConfig) Error() string {
	return fmt.Sprintf("%s: %s: %s", ErrInvalidConfigSentinel, e.Key, e.Reason)
}

// Is matches against the package-level sentinel so errors.Is(err,
// ErrInvalidConfigSentinel) returns true for any *ErrInvalidConfig instance
// regardless of Key / Reason.
func (e *ErrInvalidConfig) Is(target error) bool {
	return target == ErrInvalidConfigSentinel
}

// envContract is one row in the env-var registry. The validator iterates the
// registry in order at boot; each entry specifies the env-var name, whether
// the relay refuses to start when the var is unset, and a per-key shape
// validator. validate receives the raw value when the var is present and
// non-empty; the registry walk handles the unset and present-but-empty cases.
type envContract struct {
	name     string
	required bool
	validate func(value string) error
}

// envContracts is the single source of truth for "what env vars the relay
// reads at boot". Every new env-var read added under cmd/pyrycode-relay or
// internal/relay must register an entry here. Code review enforces.
var envContracts = []envContract{
	{
		name:     envProductionMode,
		required: false,
		validate: func(v string) error {
			if v == "1" {
				return nil
			}
			return fmt.Errorf("expected %q or unset, got %q", "1", v)
		},
	},
	{
		name:     envSingleInstanceBypass,
		required: false,
		validate: func(v string) error {
			if v == "1" {
				return nil
			}
			return fmt.Errorf("expected %q or unset, got %q", "1", v)
		},
	},
}

// CheckEnvConfig walks envContracts and returns *ErrInvalidConfig on the
// first failure. Returns nil on full pass. Intended to be called from main
// after flag parse, before any listener is started.
//
// lookup is the env-var lookup function with the os.LookupEnv signature: it
// returns (value, present). Pass os.LookupEnv at the call site; tests inject
// a func built from a map[string]string. Env values not registered in
// envContracts are ignored (the validator polices its declared contract, not
// arbitrary process env).
func CheckEnvConfig(lookup func(string) (string, bool)) error {
	return checkEnvConfigWith(lookup, envContracts)
}

func checkEnvConfigWith(lookup func(string) (string, bool), contracts []envContract) error {
	for _, c := range contracts {
		val, present := lookup(c.name)
		if !present {
			if c.required {
				return &ErrInvalidConfig{Key: c.name, Reason: "missing"}
			}
			continue
		}
		if err := c.validate(val); err != nil {
			return &ErrInvalidConfig{Key: c.name, Reason: "malformed-value: " + err.Error()}
		}
	}
	return nil
}
