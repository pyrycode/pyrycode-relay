package relay

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"
)

func TestMarshalUnmarshal_RoundTripBytewiseOpacity(t *testing.T) {
	t.Parallel()

	inner := []byte(`{"type":"hello","nested":{"a":[1,2,3],"b":null,"c":"xé"}}`)

	out, err := Marshal("c-abc123", inner)
	if err != nil {
		t.Fatalf("Marshal: unexpected error: %v", err)
	}

	got, err := Unmarshal(out)
	if err != nil {
		t.Fatalf("Unmarshal: unexpected error: %v", err)
	}
	if got.ConnID != "c-abc123" {
		t.Errorf("ConnID: got %q, want %q", got.ConnID, "c-abc123")
	}

	var wantBuf, gotBuf bytes.Buffer
	if err := json.Compact(&wantBuf, inner); err != nil {
		t.Fatalf("compact want: %v", err)
	}
	if err := json.Compact(&gotBuf, got.Frame); err != nil {
		t.Fatalf("compact got: %v", err)
	}
	if !bytes.Equal(wantBuf.Bytes(), gotBuf.Bytes()) {
		t.Errorf("frame bytes diverged after round-trip\nwant: %s\n got: %s", wantBuf.Bytes(), gotBuf.Bytes())
	}
}

func TestMarshal_Rejections(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		connID string
		frame  []byte
		want   error
	}{
		{"empty conn id", "", []byte(`{"x":1}`), ErrEmptyConnID},
		{"nil frame", "c-1", nil, ErrInvalidFrameJSON},
		{"empty frame", "c-1", []byte(""), ErrInvalidFrameJSON},
		{"garbage frame", "c-1", []byte("not json"), ErrInvalidFrameJSON},
		{"truncated frame", "c-1", []byte(`{"a":`), ErrInvalidFrameJSON},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out, err := Marshal(tc.connID, tc.frame)
			if !errors.Is(err, tc.want) {
				t.Fatalf("Marshal: got err=%v, want errors.Is(_, %v)", err, tc.want)
			}
			if out != nil {
				t.Errorf("Marshal: expected nil output on error, got %s", out)
			}
		})
	}
}

func TestUnmarshal_Rejections(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want error
	}{
		{"malformed JSON", `{"conn_id":"c-1"`, ErrMalformedEnvelope},
		{"not an object", `[]`, ErrMalformedEnvelope},
		{"missing conn_id", `{"frame":{}}`, ErrMissingConnID},
		{"empty conn_id", `{"conn_id":"","frame":{}}`, ErrMissingConnID},
		{"null conn_id", `{"conn_id":null,"frame":{}}`, ErrMissingConnID},
		{"missing frame", `{"conn_id":"c-1"}`, ErrMissingFrame},
		{"null frame", `{"conn_id":"c-1","frame":null}`, ErrMissingFrame},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := Unmarshal([]byte(tc.in))
			if !errors.Is(err, tc.want) {
				t.Fatalf("Unmarshal: got err=%v, want errors.Is(_, %v)", err, tc.want)
			}
			if got.ConnID != "" || got.Frame != nil {
				t.Errorf("Unmarshal: expected zero Envelope on error, got %+v", got)
			}
		})
	}
}
