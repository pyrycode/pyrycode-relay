package relay

import (
	"errors"
	"strings"
	"testing"
)

// statusFixture builds a /proc/self/status fixture with a custom CapEff line.
func statusFixture(capEffHex string) string {
	return "Name:\trelay\nState:\tR (running)\nCapEff:\t" + capEffHex + "\nCapBnd:\tffffffffffffffff\n"
}

func TestParseCapEff_ValueMatrix(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		input   string
		want    uint64
		wantErr bool
	}{
		{name: "zero mask", input: statusFixture("0000000000000000"), want: 0},
		{name: "only CAP_NET_BIND_SERVICE", input: statusFixture("0000000000000400"), want: 0x400},
		{name: "bits 0-37", input: statusFixture("0000003fffffffff"), want: 0x3fffffffff},
		{name: "all bits", input: statusFixture("ffffffffffffffff"), want: ^uint64(0)},
		{name: "short value", input: statusFixture("0"), want: 0},
		{name: "missing CapEff line", input: "Name:\trelay\n", wantErr: true},
		{name: "non-hex value", input: statusFixture("not-hex"), wantErr: true},
		{name: "empty file", input: "", wantErr: true},
		{name: "trailing junk after hex", input: statusFixture("400 trailing junk"), wantErr: true},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseCapEff(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseCapEff(%q) err=nil, want error", tc.input)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseCapEff(%q) unexpected err: %v", tc.input, err)
			}
			if got != tc.want {
				t.Errorf("parseCapEff(%q) = %#x, want %#x", tc.input, got, tc.want)
			}
		})
	}
}

func TestCheckCapEffMask_AllowlistMatrix(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name           string
		mask           uint64
		wantSentinel   bool
		wantSubstrings []string
	}{
		{
			name:         "empty CapEff is nil",
			mask:         0,
			wantSentinel: false,
		},
		{
			name:         "only CAP_NET_BIND_SERVICE is nil",
			mask:         0x400,
			wantSentinel: false,
		},
		{
			name:           "CAP_SYS_ADMIN is sentinel",
			mask:           uint64(1) << 21,
			wantSentinel:   true,
			wantSubstrings: []string{"CAP_SYS_ADMIN", "bit 21"},
		},
		{
			name:           "allowed plus disallowed reports only disallowed",
			mask:           0x400 | (uint64(1) << 21),
			wantSentinel:   true,
			wantSubstrings: []string{"CAP_SYS_ADMIN"},
		},
		{
			name:           "unknown bit 63 is sentinel",
			mask:           uint64(1) << 63,
			wantSentinel:   true,
			wantSubstrings: []string{"bit 63"},
		},
		{
			name:           "all bits names allowlist contents",
			mask:           ^uint64(0),
			wantSentinel:   true,
			wantSubstrings: []string{"CAP_NET_BIND_SERVICE"},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := checkCapEffMask(tc.mask)
			if !tc.wantSentinel {
				if err != nil {
					t.Fatalf("checkCapEffMask(%#x) = %v, want nil", tc.mask, err)
				}
				return
			}
			if !errors.Is(err, ErrUnexpectedCapability) {
				t.Fatalf("checkCapEffMask(%#x) = %v, want errors.Is(err, ErrUnexpectedCapability)", tc.mask, err)
			}
			msg := err.Error()
			for _, want := range tc.wantSubstrings {
				if !strings.Contains(msg, want) {
					t.Errorf("checkCapEffMask(%#x) error %q missing substring %q", tc.mask, msg, want)
				}
			}
		})
	}
}

func TestCheckCapEffMask_DisallowedReportsOnlyOffendingBits(t *testing.T) {
	t.Parallel()

	// CAP_NET_BIND_SERVICE is allowed; ensure its name does not appear in
	// the unexpected-bits portion when combined with a disallowed bit.
	err := checkCapEffMask(0x400 | (uint64(1) << 21))
	if !errors.Is(err, ErrUnexpectedCapability) {
		t.Fatalf("got %v, want sentinel", err)
	}
	msg := err.Error()
	// "CAP_NET_BIND_SERVICE" appears in the allowlist portion; the
	// unexpected-bits portion (before "; allowlist:") must not contain it.
	idx := strings.Index(msg, "; allowlist:")
	if idx < 0 {
		t.Fatalf("error %q missing allowlist section", msg)
	}
	if strings.Contains(msg[:idx], "CAP_NET_BIND_SERVICE") {
		t.Errorf("unexpected-bits section %q must not name an allowed cap", msg[:idx])
	}
}

func TestErrUnexpectedCapability_IsBranchable(t *testing.T) {
	t.Parallel()

	if !errors.Is(ErrUnexpectedCapability, ErrUnexpectedCapability) {
		t.Fatal("ErrUnexpectedCapability should errors.Is itself")
	}

	err := checkCapEffMask(uint64(1) << 21)
	if !errors.Is(err, ErrUnexpectedCapability) {
		t.Errorf("returned error %v should satisfy errors.Is(err, ErrUnexpectedCapability)", err)
	}
}

func TestCapabilityName(t *testing.T) {
	t.Parallel()

	if got := capabilityName(10); got != "CAP_NET_BIND_SERVICE" {
		t.Errorf("capabilityName(10) = %q, want CAP_NET_BIND_SERVICE", got)
	}
	if got := capabilityName(21); got != "CAP_SYS_ADMIN" {
		t.Errorf("capabilityName(21) = %q, want CAP_SYS_ADMIN", got)
	}
	if got := capabilityName(63); got != "" {
		t.Errorf("capabilityName(63) = %q, want empty (unknown bit)", got)
	}
	if got := capabilityName(999); got != "" {
		t.Errorf("capabilityName(999) = %q, want empty (out of range)", got)
	}
}

func TestAllowedMask(t *testing.T) {
	t.Parallel()

	// AllowedCapabilities contains CAP_NET_BIND_SERVICE (bit 10).
	want := uint64(1) << 10
	if got := allowedMask(); got != want {
		t.Errorf("allowedMask() = %#x, want %#x", got, want)
	}
}
