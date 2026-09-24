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

// TestUnmarshal_CloseDirective covers the disconnect half of the routing
// contract: a non-zero close_code makes a frameless envelope valid (the
// daemon closes a phone with no final frame), while a frame-carrying close
// directive keeps its frame. conn_id is still mandatory.
func TestUnmarshal_CloseDirective(t *testing.T) {
	t.Parallel()

	t.Run("close code, null frame is valid", func(t *testing.T) {
		t.Parallel()
		env, err := Unmarshal([]byte(`{"conn_id":"c-1","frame":null,"close_code":4401}`))
		if err != nil {
			t.Fatalf("Unmarshal: unexpected err %v", err)
		}
		if env.ConnID != "c-1" || env.CloseCode != 4401 {
			t.Fatalf("Unmarshal: got %+v", env)
		}
		if hasFrame(env.Frame) {
			t.Errorf("hasFrame = true, want false for null frame")
		}
	})

	t.Run("close code, absent frame is valid", func(t *testing.T) {
		t.Parallel()
		env, err := Unmarshal([]byte(`{"conn_id":"c-1","close_code":4403}`))
		if err != nil {
			t.Fatalf("Unmarshal: unexpected err %v", err)
		}
		if env.CloseCode != 4403 || hasFrame(env.Frame) {
			t.Fatalf("Unmarshal: got %+v", env)
		}
	})

	t.Run("close code with frame keeps the frame", func(t *testing.T) {
		t.Parallel()
		env, err := Unmarshal([]byte(`{"conn_id":"c-1","frame":{"type":"error"},"close_code":4401}`))
		if err != nil {
			t.Fatalf("Unmarshal: unexpected err %v", err)
		}
		if env.CloseCode != 4401 || !hasFrame(env.Frame) {
			t.Fatalf("Unmarshal: got %+v", env)
		}
	})

	t.Run("close code without conn_id still rejected", func(t *testing.T) {
		t.Parallel()
		if _, err := Unmarshal([]byte(`{"frame":null,"close_code":4401}`)); !errors.Is(err, ErrMissingConnID) {
			t.Fatalf("Unmarshal: got err=%v, want ErrMissingConnID", err)
		}
	})
}

// TestUnmarshal_PushWake covers the relay-addressed wake shape: no conn_id,
// no frame, a present push_wake. conn_id envelopes decode exactly as before,
// whatever push_wake they carry.
func TestUnmarshal_PushWake(t *testing.T) {
	t.Parallel()

	t.Run("wake envelope is valid", func(t *testing.T) {
		t.Parallel()
		env, err := Unmarshal([]byte(`{"push_wake":{"platform":"fcm","token":"t"}}`))
		if err != nil {
			t.Fatalf("Unmarshal: unexpected err %v", err)
		}
		if env.ConnID != "" || !bytes.Equal(env.PushWake, []byte(`{"platform":"fcm","token":"t"}`)) {
			t.Fatalf("Unmarshal: got %+v", env)
		}
	})

	t.Run("malformed wake object still reaches the waker", func(t *testing.T) {
		t.Parallel()
		env, err := Unmarshal([]byte(`{"push_wake":5}`))
		if err != nil || string(env.PushWake) != "5" {
			t.Fatalf("Unmarshal: got (%+v, %v)", env, err)
		}
	})

	for _, in := range []string{`{}`, `{"push_wake":null}`, `{"conn_id":"","push_wake":null,"frame":{}}`} {
		if _, err := Unmarshal([]byte(in)); !errors.Is(err, ErrMissingConnID) {
			t.Errorf("Unmarshal(%s): err=%v, want ErrMissingConnID", in, err)
		}
	}

	t.Run("conn_id envelope ignores push_wake", func(t *testing.T) {
		t.Parallel()
		env, err := Unmarshal([]byte(`{"conn_id":"c-1","frame":{"a":1},"push_wake":"garbage"}`))
		if err != nil || env.ConnID != "c-1" || !hasFrame(env.Frame) {
			t.Fatalf("Unmarshal: got (%+v, %v)", env, err)
		}
		if _, err := Unmarshal([]byte(`{"conn_id":"c-1","push_wake":{"platform":"fcm","token":"t"}}`)); !errors.Is(err, ErrMissingFrame) {
			t.Fatalf("conn_id + push_wake, no frame: err=%v, want ErrMissingFrame", err)
		}
	})

	t.Run("marshal output unchanged", func(t *testing.T) {
		t.Parallel()
		out, err := Marshal("c-1", []byte(`{}`))
		if err != nil || bytes.Contains(out, []byte("push_wake")) {
			t.Fatalf("Marshal: got (%s, %v)", out, err)
		}
	})
}
