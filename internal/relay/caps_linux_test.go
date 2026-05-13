package relay

import (
	"errors"
	"testing"
)

func TestCheckCapabilitiesWithReader_Matrix(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name         string
		readStatus   func() (string, error)
		wantSentinel bool
		wantErr      bool // any non-nil error (sentinel or otherwise)
	}{
		{
			name:         "only CAP_NET_BIND_SERVICE returns nil",
			readStatus:   func() (string, error) { return statusFixture("0000000000000400"), nil },
			wantSentinel: false,
			wantErr:      false,
		},
		{
			name:         "CAP_SYS_ADMIN returns sentinel",
			readStatus:   func() (string, error) { return statusFixture("0000000000200000"), nil },
			wantSentinel: true,
			wantErr:      true,
		},
		{
			name:         "missing CapEff line returns wrapped parse error",
			readStatus:   func() (string, error) { return "Name:\trelay\n", nil },
			wantSentinel: false,
			wantErr:      true,
		},
		{
			name:         "reader error propagates wrapped",
			readStatus:   func() (string, error) { return "", errors.New("io: synthetic") },
			wantSentinel: false,
			wantErr:      true,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := checkCapabilitiesWithReader(tc.readStatus)
			if tc.wantErr && err == nil {
				t.Fatalf("checkCapabilitiesWithReader err=nil, want error")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("checkCapabilitiesWithReader err=%v, want nil", err)
			}
			if tc.wantSentinel && !errors.Is(err, ErrUnexpectedCapability) {
				t.Errorf("err %v should satisfy errors.Is(err, ErrUnexpectedCapability)", err)
			}
			if !tc.wantSentinel && err != nil && errors.Is(err, ErrUnexpectedCapability) {
				t.Errorf("err %v should NOT satisfy errors.Is(err, ErrUnexpectedCapability)", err)
			}
		})
	}
}
