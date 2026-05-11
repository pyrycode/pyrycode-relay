package relay

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClientIP(t *testing.T) {
	tests := []struct {
		name              string
		remoteAddr        string
		xffHeader         string
		setXFF            bool
		trustForwardedFor bool
		want              string
	}{
		{
			name:       "RemoteAddr_IPv4_WithPort",
			remoteAddr: "192.0.2.5:54321",
			want:       "192.0.2.5",
		},
		{
			name:       "RemoteAddr_IPv6_Bracketed",
			remoteAddr: "[2001:db8::1]:54321",
			want:       "2001:db8::1",
		},
		{
			name:       "RemoteAddr_Loopback_IPv6",
			remoteAddr: "[::1]:8080",
			want:       "::1",
		},
		{
			name:       "RemoteAddr_MalformedNoPort",
			remoteAddr: "192.0.2.5",
			want:       "",
		},
		{
			name:       "RemoteAddr_Empty",
			remoteAddr: "",
			want:       "",
		},
		{
			name:              "XFF_Disabled_HeaderIgnored",
			remoteAddr:        "192.0.2.5:54321",
			xffHeader:         "203.0.113.7",
			setXFF:            true,
			trustForwardedFor: false,
			want:              "192.0.2.5",
		},
		{
			name:              "XFF_Enabled_Single",
			remoteAddr:        "10.0.0.1:443",
			xffHeader:         "203.0.113.7",
			setXFF:            true,
			trustForwardedFor: true,
			want:              "203.0.113.7",
		},
		{
			name:              "XFF_Enabled_MultiEntry",
			remoteAddr:        "10.0.0.1:443",
			xffHeader:         "203.0.113.7, 192.0.2.10, 198.51.100.4",
			setXFF:            true,
			trustForwardedFor: true,
			want:              "203.0.113.7",
		},
		{
			name:              "XFF_Enabled_LeadingWhitespace",
			remoteAddr:        "10.0.0.1:443",
			xffHeader:         "   203.0.113.7   , 192.0.2.10",
			setXFF:            true,
			trustForwardedFor: true,
			want:              "203.0.113.7",
		},
		{
			name:              "XFF_Enabled_Absent_FallsBackToRemoteAddr",
			remoteAddr:        "192.0.2.5:54321",
			trustForwardedFor: true,
			want:              "192.0.2.5",
		},
		{
			name:              "XFF_Enabled_Empty_FallsBackToRemoteAddr",
			remoteAddr:        "192.0.2.5:54321",
			xffHeader:         "",
			setXFF:            true,
			trustForwardedFor: true,
			want:              "192.0.2.5",
		},
		{
			name:              "XFF_Enabled_WhitespaceOnlyFirstEntry_FallsBackToRemoteAddr",
			remoteAddr:        "192.0.2.5:54321",
			xffHeader:         "   , 198.51.100.4",
			setXFF:            true,
			trustForwardedFor: true,
			want:              "192.0.2.5",
		},
		{
			name:              "XFF_Enabled_RemoteAddrAlsoMalformed_ReturnsEmpty",
			remoteAddr:        "garbage",
			xffHeader:         "",
			setXFF:            true,
			trustForwardedFor: true,
			want:              "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.RemoteAddr = tc.remoteAddr
			if tc.setXFF {
				r.Header.Set("X-Forwarded-For", tc.xffHeader)
			}

			got := ClientIP(r, tc.trustForwardedFor)
			if got != tc.want {
				t.Errorf("ClientIP(remoteAddr=%q, xff=%q set=%v, trust=%v) = %q, want %q",
					tc.remoteAddr, tc.xffHeader, tc.setXFF, tc.trustForwardedFor, got, tc.want)
			}
		})
	}
}
