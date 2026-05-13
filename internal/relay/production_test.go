package relay

import (
	"errors"
	"testing"
)

func fakeGetenv(env map[string]string) func(string) string {
	return func(k string) string { return env[k] }
}

func TestIsProductionMode_ValueMatrix(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		value string
		set   bool
		want  bool
	}{
		{name: "exact 1 is production", value: "1", set: true, want: true},
		{name: "unset is non-production", set: false, want: false},
		{name: "empty string is non-production", value: "", set: true, want: false},
		{name: "zero is non-production", value: "0", set: true, want: false},
		{name: "true is non-production", value: "true", set: true, want: false},
		{name: "yes is non-production", value: "yes", set: true, want: false},
		{name: "leading space is non-production", value: " 1", set: true, want: false},
		{name: "trailing space is non-production", value: "1 ", set: true, want: false},
		{name: "uppercase PRODUCTION is non-production", value: "PRODUCTION", set: true, want: false},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := map[string]string{}
			if tc.set {
				env[envProductionMode] = tc.value
			}
			if got := IsProductionMode(fakeGetenv(env)); got != tc.want {
				t.Errorf("IsProductionMode(%q set=%v) = %v, want %v", tc.value, tc.set, got, tc.want)
			}
		})
	}
}

func TestCheckInsecureListenInProduction_Matrix(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name           string
		productionMode string // "" means unset
		setEnv         bool
		insecureListen string
		wantSentinel   bool
	}{
		{
			name:           "non-production + insecure-listen returns nil",
			setEnv:         false,
			insecureListen: ":8080",
			wantSentinel:   false,
		},
		{
			name:           "production + insecure-listen returns sentinel",
			productionMode: "1",
			setEnv:         true,
			insecureListen: ":8080",
			wantSentinel:   true,
		},
		{
			name:           "production + autocert (insecureListen empty) returns nil",
			productionMode: "1",
			setEnv:         true,
			insecureListen: "",
			wantSentinel:   false,
		},
		{
			name:           "non-production + autocert returns nil",
			setEnv:         false,
			insecureListen: "",
			wantSentinel:   false,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := map[string]string{}
			if tc.setEnv {
				env[envProductionMode] = tc.productionMode
			}
			err := CheckInsecureListenInProduction(tc.insecureListen, fakeGetenv(env))
			if tc.wantSentinel {
				if !errors.Is(err, ErrInsecureListenInProduction) {
					t.Errorf("got err %v, want errors.Is(err, ErrInsecureListenInProduction)", err)
				}
			} else {
				if err != nil {
					t.Errorf("got err %v, want nil", err)
				}
			}
		})
	}
}

func TestErrInsecureListenInProduction_IsBranchable(t *testing.T) {
	t.Parallel()

	if !errors.Is(ErrInsecureListenInProduction, ErrInsecureListenInProduction) {
		t.Fatal("ErrInsecureListenInProduction should be errors.Is itself")
	}

	env := map[string]string{envProductionMode: "1"}
	err := CheckInsecureListenInProduction(":8080", fakeGetenv(env))
	if !errors.Is(err, ErrInsecureListenInProduction) {
		t.Errorf("returned error %v should satisfy errors.Is(err, ErrInsecureListenInProduction)", err)
	}
}

func fakeGeteuid(n int) func() int {
	return func() int { return n }
}

func TestCheckRunningAsRoot_Matrix(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name           string
		productionMode string // "" with setEnv=false means unset
		setEnv         bool
		uid            int
		wantSentinel   bool
	}{
		{
			name:         "non-production + uid 0 returns nil",
			setEnv:       false,
			uid:          0,
			wantSentinel: false,
		},
		{
			name:           "production + uid 1000 returns nil",
			productionMode: "1",
			setEnv:         true,
			uid:            1000,
			wantSentinel:   false,
		},
		{
			name:           "production + uid 0 returns sentinel",
			productionMode: "1",
			setEnv:         true,
			uid:            0,
			wantSentinel:   true,
		},
		{
			name:           "production + nobody uid 65534 returns nil",
			productionMode: "1",
			setEnv:         true,
			uid:            65534,
			wantSentinel:   false,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := map[string]string{}
			if tc.setEnv {
				env[envProductionMode] = tc.productionMode
			}
			err := CheckRunningAsRoot(fakeGeteuid(tc.uid), fakeGetenv(env))
			if tc.wantSentinel {
				if !errors.Is(err, ErrRunningAsRoot) {
					t.Errorf("got err %v, want errors.Is(err, ErrRunningAsRoot)", err)
				}
			} else {
				if err != nil {
					t.Errorf("got err %v, want nil", err)
				}
			}
		})
	}
}

func TestErrRunningAsRoot_IsBranchable(t *testing.T) {
	t.Parallel()

	if !errors.Is(ErrRunningAsRoot, ErrRunningAsRoot) {
		t.Fatal("ErrRunningAsRoot should be errors.Is itself")
	}

	env := map[string]string{envProductionMode: "1"}
	err := CheckRunningAsRoot(fakeGeteuid(0), fakeGetenv(env))
	if !errors.Is(err, ErrRunningAsRoot) {
		t.Errorf("returned error %v should satisfy errors.Is(err, ErrRunningAsRoot)", err)
	}
}
