package relay

import (
	"errors"
	"strings"
	"testing"
)

func fakeLookup(env map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := env[k]
		return v, ok
	}
}

func TestCheckEnvConfig_ValidEnvReturnsNil(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		env  map[string]string
	}{
		{name: "production unset is valid", env: map[string]string{}},
		{name: "production exact 1 is valid", env: map[string]string{envProductionMode: "1"}},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := CheckEnvConfig(fakeLookup(tc.env)); err != nil {
				t.Errorf("CheckEnvConfig(%v) = %v, want nil", tc.env, err)
			}
		})
	}
}

func TestCheckEnvConfig_MalformedValueReturnsStructuredError(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name          string
		value         string
		wantReasonSub string
	}{
		{name: "true is malformed", value: "true", wantReasonSub: `expected "1" or unset, got "true"`},
		{name: "zero is malformed", value: "0", wantReasonSub: `expected "1" or unset, got "0"`},
		{name: "yes is malformed", value: "yes", wantReasonSub: `expected "1" or unset, got "yes"`},
		{name: "leading space is malformed", value: " 1", wantReasonSub: `expected "1" or unset, got " 1"`},
		{name: "trailing space is malformed", value: "1 ", wantReasonSub: `expected "1" or unset, got "1 "`},
		{name: "uppercase PRODUCTION is malformed", value: "PRODUCTION", wantReasonSub: `expected "1" or unset, got "PRODUCTION"`},
		{name: "empty string is malformed", value: "", wantReasonSub: `expected "1" or unset, got ""`},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := map[string]string{envProductionMode: tc.value}
			err := CheckEnvConfig(fakeLookup(env))
			if err == nil {
				t.Fatalf("CheckEnvConfig with value %q returned nil, want error", tc.value)
			}
			if !errors.Is(err, ErrInvalidConfigSentinel) {
				t.Errorf("err %v should satisfy errors.Is(err, ErrInvalidConfigSentinel)", err)
			}
			var cfgErr *ErrInvalidConfig
			if !errors.As(err, &cfgErr) {
				t.Fatalf("err %v should satisfy errors.As(err, &*ErrInvalidConfig)", err)
			}
			if cfgErr.Key != envProductionMode {
				t.Errorf("got Key=%q, want %q", cfgErr.Key, envProductionMode)
			}
			if !strings.HasPrefix(cfgErr.Reason, "malformed-value: ") {
				t.Errorf("got Reason=%q, want prefix %q", cfgErr.Reason, "malformed-value: ")
			}
			if !strings.Contains(cfgErr.Reason, tc.wantReasonSub) {
				t.Errorf("got Reason=%q, want contains %q", cfgErr.Reason, tc.wantReasonSub)
			}
		})
	}
}

func TestCheckEnvConfig_SingleInstanceBypassMalformedValue(t *testing.T) {
	t.Parallel()

	// Lock the envContracts row added for PYRYCODE_RELAY_SINGLE_INSTANCE so
	// a typo'd bypass (e.g. =true) fails CheckEnvConfig with a structured
	// per-key error before CheckSingleInstance reads the value. See
	// docs/specs/architecture/65-startup-multi-instance-check.md.
	env := map[string]string{envSingleInstanceBypass: "true"}
	err := CheckEnvConfig(fakeLookup(env))
	if err == nil {
		t.Fatal("CheckEnvConfig with bypass=true returned nil, want error")
	}
	if !errors.Is(err, ErrInvalidConfigSentinel) {
		t.Errorf("err %v should satisfy errors.Is(err, ErrInvalidConfigSentinel)", err)
	}
	var cfgErr *ErrInvalidConfig
	if !errors.As(err, &cfgErr) {
		t.Fatalf("err %v should satisfy errors.As(err, &*ErrInvalidConfig)", err)
	}
	if cfgErr.Key != envSingleInstanceBypass {
		t.Errorf("got Key=%q, want %q", cfgErr.Key, envSingleInstanceBypass)
	}
	if !strings.HasPrefix(cfgErr.Reason, "malformed-value: ") {
		t.Errorf("got Reason=%q, want prefix %q", cfgErr.Reason, "malformed-value: ")
	}
}

func TestCheckEnvConfig_MissingRequiredKey(t *testing.T) {
	t.Parallel()

	contracts := []envContract{
		{
			name:     "PYRYCODE_TEST_REQUIRED",
			required: true,
			validate: func(string) error { return nil },
		},
	}
	err := checkEnvConfigWith(fakeLookup(map[string]string{}), contracts)
	if err == nil {
		t.Fatal("checkEnvConfigWith with missing required key returned nil, want error")
	}
	if !errors.Is(err, ErrInvalidConfigSentinel) {
		t.Errorf("err %v should satisfy errors.Is(err, ErrInvalidConfigSentinel)", err)
	}
	var cfgErr *ErrInvalidConfig
	if !errors.As(err, &cfgErr) {
		t.Fatalf("err %v should satisfy errors.As(err, &*ErrInvalidConfig)", err)
	}
	if cfgErr.Key != "PYRYCODE_TEST_REQUIRED" {
		t.Errorf("got Key=%q, want %q", cfgErr.Key, "PYRYCODE_TEST_REQUIRED")
	}
	if cfgErr.Reason != "missing" {
		t.Errorf("got Reason=%q, want %q", cfgErr.Reason, "missing")
	}
}

func TestCheckEnvConfig_IgnoresUnregisteredKeys(t *testing.T) {
	t.Parallel()

	env := map[string]string{
		"FOO_BAR_BAZ":      "garbage",
		"PYRYCODE_UNKNOWN": "anything",
		"PATH":             "/usr/bin",
		// PYRYCODE_RELAY_PRODUCTION intentionally absent (optional, unset is valid).
	}
	if err := CheckEnvConfig(fakeLookup(env)); err != nil {
		t.Errorf("CheckEnvConfig with unregistered keys = %v, want nil", err)
	}
}

func TestErrInvalidConfigSentinel_IsBranchable(t *testing.T) {
	t.Parallel()

	err := &ErrInvalidConfig{Key: "X", Reason: "missing"}
	if !errors.Is(err, ErrInvalidConfigSentinel) {
		t.Error("*ErrInvalidConfig should satisfy errors.Is(_, ErrInvalidConfigSentinel)")
	}
	var cfgErr *ErrInvalidConfig
	if !errors.As(error(err), &cfgErr) {
		t.Error("*ErrInvalidConfig should satisfy errors.As(_, &*ErrInvalidConfig)")
	}
}

func TestCheckEnvConfig_FCMCredentialsValidKeyPasses(t *testing.T) {
	t.Parallel()

	env := map[string]string{envFCMCredentials: string(testServiceAccountJSON(t, ""))}
	if err := CheckEnvConfig(fakeLookup(env)); err != nil {
		t.Errorf("CheckEnvConfig with a valid service-account key = %v, want nil", err)
	}
}

func TestCheckEnvConfig_FCMCredentialsMalformedWithholdsValue(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"not JSON":   testFCMMarker + " {",
		"wrong type": `{"type":"` + testFCMMarker + `","client_email":"a@b","private_key":"x"}`,
		"empty":      "",
	}
	for name, value := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := CheckEnvConfig(fakeLookup(map[string]string{envFCMCredentials: value}))
			if !errors.Is(err, ErrInvalidConfigSentinel) {
				t.Fatalf("err = %v, want ErrInvalidConfigSentinel", err)
			}
			var cfgErr *ErrInvalidConfig
			if !errors.As(err, &cfgErr) {
				t.Fatalf("err %v should satisfy errors.As(err, &*ErrInvalidConfig)", err)
			}
			if cfgErr.Key != envFCMCredentials {
				t.Errorf("got Key=%q, want %q", cfgErr.Key, envFCMCredentials)
			}
			if !strings.HasPrefix(cfgErr.Reason, "malformed-value: ") {
				t.Errorf("got Reason=%q, want prefix %q", cfgErr.Reason, "malformed-value: ")
			}
			if strings.Contains(err.Error(), testFCMMarker) || (value != "" && strings.Contains(err.Error(), value)) {
				t.Errorf("error %q leaks the credential value", err)
			}
		})
	}
}
