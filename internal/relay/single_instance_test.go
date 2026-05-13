package relay

import (
	"errors"
	"strings"
	"testing"
)

func TestCheckSingleInstance_Matrix(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name         string
		env          map[string]string
		wantSentinel bool
	}{
		{
			name: "multi-instance signal present, bypass unset returns sentinel",
			env: map[string]string{
				envFlyAppName: "pyrycode-relay",
			},
			wantSentinel: true,
		},
		{
			name: "bypass exact 1, signal present returns nil",
			env: map[string]string{
				envFlyAppName:           "pyrycode-relay",
				envSingleInstanceBypass: "1",
			},
			wantSentinel: false,
		},
		{
			name:         "no signal, no bypass returns nil",
			env:          map[string]string{},
			wantSentinel: false,
		},
		{
			name: "bypass set, signal absent returns nil",
			env: map[string]string{
				envSingleInstanceBypass: "1",
			},
			wantSentinel: false,
		},
		{
			name: "bypass true (non-exact) with signal returns sentinel",
			env: map[string]string{
				envFlyAppName:           "pyrycode-relay",
				envSingleInstanceBypass: "true",
			},
			wantSentinel: true,
		},
		{
			name: "bypass 0 with signal returns sentinel",
			env: map[string]string{
				envFlyAppName:           "pyrycode-relay",
				envSingleInstanceBypass: "0",
			},
			wantSentinel: true,
		},
		{
			name: "bypass leading-space with signal returns sentinel",
			env: map[string]string{
				envFlyAppName:           "pyrycode-relay",
				envSingleInstanceBypass: " 1",
			},
			wantSentinel: true,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := CheckSingleInstance(fakeGetenv(tc.env))
			if tc.wantSentinel {
				if !errors.Is(err, ErrMultiInstanceDeployDetected) {
					t.Errorf("got err %v, want errors.Is(err, ErrMultiInstanceDeployDetected)", err)
				}
			} else {
				if err != nil {
					t.Errorf("got err %v, want nil", err)
				}
			}
		})
	}
}

func TestErrMultiInstanceDeployDetected_MessageContainsACSubstrings(t *testing.T) {
	t.Parallel()

	msg := ErrMultiInstanceDeployDetected.Error()
	if !strings.Contains(msg, "multi-instance deploy detected") {
		t.Errorf("error message %q must contain %q (AC #2)", msg, "multi-instance deploy detected")
	}
	if !strings.Contains(msg, "PYRYCODE_RELAY_SINGLE_INSTANCE") {
		t.Errorf("error message %q must contain bypass env var name %q (AC #2)", msg, "PYRYCODE_RELAY_SINGLE_INSTANCE")
	}
}

func TestErrMultiInstanceDeployDetected_IsBranchable(t *testing.T) {
	t.Parallel()

	if !errors.Is(ErrMultiInstanceDeployDetected, ErrMultiInstanceDeployDetected) {
		t.Fatal("ErrMultiInstanceDeployDetected should be errors.Is itself")
	}

	env := map[string]string{envFlyAppName: "pyrycode-relay"}
	err := CheckSingleInstance(fakeGetenv(env))
	if !errors.Is(err, ErrMultiInstanceDeployDetected) {
		t.Errorf("returned error %v should satisfy errors.Is(err, ErrMultiInstanceDeployDetected)", err)
	}
}
