package relay

import (
	"errors"
	"strings"
	"testing"
)

func TestListenerPort_Matrix(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		addr     string
		wantPort uint16
		wantErr  bool
	}{
		{name: "all-interfaces 443", addr: ":443", wantPort: 443},
		{name: "all-interfaces 80", addr: ":80", wantPort: 80},
		{name: "loopback ipv4 high port", addr: "127.0.0.1:8080", wantPort: 8080},
		{name: "loopback ipv6 bracketed", addr: "[::1]:443", wantPort: 443},
		{name: "zero ipv4 host", addr: "0.0.0.0:9000", wantPort: 9000},
		{name: "empty rejected", addr: "", wantErr: true},
		{name: "no colon rejected", addr: "443", wantErr: true},
		{name: "host only rejected", addr: "127.0.0.1", wantErr: true},
		{name: "zero port rejected", addr: ":0", wantErr: true},
		{name: "out of range rejected", addr: ":99999", wantErr: true},
		{name: "non-numeric port rejected", addr: ":notaport", wantErr: true},
		{name: "negative port rejected", addr: ":-1", wantErr: true},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ListenerPort(tc.addr)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ListenerPort(%q) = %d, nil; want error", tc.addr, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ListenerPort(%q) returned error: %v", tc.addr, err)
			}
			if got != tc.wantPort {
				t.Errorf("ListenerPort(%q) = %d, want %d", tc.addr, got, tc.wantPort)
			}
		})
	}
}

func portSet(ports ...uint16) map[uint16]struct{} {
	s := make(map[uint16]struct{}, len(ports))
	for _, p := range ports {
		s[p] = struct{}{}
	}
	return s
}

func TestCheckListenerPorts_ACMatrix(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		expected    map[uint16]struct{}
		actual      map[uint16]struct{}
		wantErr     bool
		mustContain []string
	}{
		{
			name:     "autocert_match",
			expected: portSet(443, 80),
			actual:   portSet(443, 80),
			wantErr:  false,
		},
		{
			name:     "insecure_match",
			expected: portSet(8080),
			actual:   portSet(8080),
			wantErr:  false,
		},
		{
			name:        "surplus_pprof",
			expected:    portSet(443, 80),
			actual:      portSet(443, 80, 6060),
			wantErr:     true,
			mustContain: []string{"6060"},
		},
		{
			name:     "actual_subset_of_expected",
			expected: portSet(443, 80),
			actual:   portSet(443),
			wantErr:  false,
		},
		{
			name:     "empty_both",
			expected: portSet(),
			actual:   portSet(),
			wantErr:  false,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := CheckListenerPorts(tc.expected, tc.actual)
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("CheckListenerPorts: got err %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, ErrUnexpectedListener) {
				t.Fatalf("CheckListenerPorts: got err %v, want errors.Is(err, ErrUnexpectedListener)", err)
			}
			msg := err.Error()
			for _, sub := range tc.mustContain {
				if !strings.Contains(msg, sub) {
					t.Errorf("error message %q missing substring %q", msg, sub)
				}
			}
		})
	}
}

func TestCheckListenerPorts_SurplusListedSorted(t *testing.T) {
	t.Parallel()

	err := CheckListenerPorts(portSet(443), portSet(443, 9000, 6060, 1234))
	if !errors.Is(err, ErrUnexpectedListener) {
		t.Fatalf("got err %v, want errors.Is(err, ErrUnexpectedListener)", err)
	}
	msg := err.Error()
	idx := func(s string) int { return strings.Index(msg, s) }
	i1234, i6060, i9000 := idx("1234"), idx("6060"), idx("9000")
	if i1234 < 0 || i6060 < 0 || i9000 < 0 {
		t.Fatalf("error message %q missing one of 1234/6060/9000", msg)
	}
	if !(i1234 < i6060 && i6060 < i9000) {
		t.Errorf("surplus ports not in ascending order in %q (positions: 1234=%d 6060=%d 9000=%d)",
			msg, i1234, i6060, i9000)
	}
}

func TestCheckListenerPorts_MultipleSurplus(t *testing.T) {
	t.Parallel()

	err := CheckListenerPorts(portSet(443, 80), portSet(443, 80, 6060, 9090))
	if !errors.Is(err, ErrUnexpectedListener) {
		t.Fatalf("got err %v, want errors.Is(err, ErrUnexpectedListener)", err)
	}
	msg := err.Error()
	for _, sub := range []string{"6060", "9090"} {
		if !strings.Contains(msg, sub) {
			t.Errorf("error message %q missing surplus port %q", msg, sub)
		}
	}
}

func TestErrUnexpectedListener_IsBranchable(t *testing.T) {
	t.Parallel()

	if !errors.Is(ErrUnexpectedListener, ErrUnexpectedListener) {
		t.Fatal("ErrUnexpectedListener should be errors.Is itself")
	}
	err := CheckListenerPorts(portSet(443), portSet(443, 6060))
	if !errors.Is(err, ErrUnexpectedListener) {
		t.Errorf("returned error %v should satisfy errors.Is(err, ErrUnexpectedListener)", err)
	}
}
