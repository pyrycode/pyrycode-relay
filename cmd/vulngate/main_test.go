package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func openFixture(t *testing.T, name string) *os.File {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func TestParse(t *testing.T) {
	calledWant := []vuln{
		{
			ID:      "GO-2021-0265",
			Module:  "github.com/tidwall/gjson",
			Version: "v1.6.5",
			Fixed:   "v1.9.3",
			Symbols: []string{"github.com/tidwall/gjson.Get", "github.com/tidwall/gjson.Result.Get"},
			Summary: "Denial of service in gjson",
		},
		{
			ID:      "GO-2024-2687",
			Module:  "stdlib",
			Version: "v1.22.1",
			Symbols: []string{"net/http.ListenAndServe"},
			Summary: "HTTP/2 CONTINUATION flood in net/http",
		},
	}
	tests := []struct {
		fixture string
		want    []vuln
		wantErr bool
	}{
		{fixture: "clean.json", want: nil},
		{fixture: "imported-only.json", want: nil},
		{fixture: "called.json", want: calledWant},
		{fixture: "truncated.json", wantErr: true},
		{fixture: "module-scan.json", wantErr: true},
		{fixture: "no-config.json", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.fixture, func(t *testing.T) {
			got, err := parse(openFixture(t, tt.fixture))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parse succeeded with %d vulns, want error", len(got))
				}
				return
			}
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("parse =\n%+v\nwant\n%+v", got, tt.want)
			}
		})
	}
}

func TestParseEmptyInput(t *testing.T) {
	if _, err := parse(strings.NewReader("")); err == nil {
		t.Fatal("parse of empty input succeeded, want error")
	}
}

func TestParseMalformedFinding(t *testing.T) {
	const cfg = `{"config":{"protocol_version":"v1.0.0","scan_level":"symbol"}}`
	tests := map[string]string{
		"no osv id":      `{"finding":{"trace":[{"module":"m","function":"F"}]}}`,
		"no trace":       `{"finding":{"osv":"GO-2021-0001"}}`,
		"config twice":   cfg,
		"bad protocol":   `{"config":{"protocol_version":"v2.0.0","scan_level":"symbol"}}`,
		"not an object":  `[1,2]`,
		"trailing junk":  `{"progress":{}} }`,
		"quote in id":    `{"finding":{"osv":"GO-\"1","trace":[{"module":"m","package":"p","function":"F"}]}}`,
		"newline in mod": `{"finding":{"osv":"GO-2021-0001","trace":[{"module":"a\nb","package":"p","function":"F"}]}}`,
	}
	for name, rec := range tests {
		t.Run(name, func(t *testing.T) {
			in := cfg + "\n" + rec
			if name == "bad protocol" {
				in = rec
			}
			if _, err := parse(strings.NewReader(in)); err == nil {
				t.Fatalf("parse(%q) succeeded, want error", in)
			}
		})
	}
}

func TestRun(t *testing.T) {
	tests := []struct {
		fixture   string
		wantCode  int
		wantFiles bool
	}{
		{"called.json", exitFindings, true},
		{"clean.json", exitClean, false},
		{"imported-only.json", exitClean, false},
		{"truncated.json", exitError, false},
		{"module-scan.json", exitError, false},
		{"no-config.json", exitError, false},
		{"missing.json", exitError, false},
	}
	for _, tt := range tests {
		t.Run(tt.fixture, func(t *testing.T) {
			out := t.TempDir()
			var stdout, stderr bytes.Buffer
			code := run([]string{
				"-in", filepath.Join("testdata", tt.fixture),
				"-out", out,
				"-run-url", "https://github.com/o/r/actions/runs/1",
			}, &stdout, &stderr)
			if code != tt.wantCode {
				t.Fatalf("exit = %d, want %d; stderr: %s", code, tt.wantCode, stderr.String())
			}
			for _, name := range []string{titleFile, bodyFile} {
				_, err := os.Stat(filepath.Join(out, name))
				if exists := err == nil; exists != tt.wantFiles {
					t.Errorf("%s exists = %v, want %v", name, exists, tt.wantFiles)
				}
			}
		})
	}
}

func TestRunRendersCalledFindings(t *testing.T) {
	out := t.TempDir()
	code := run([]string{"-in", "testdata/called.json", "-out", out, "-run-url", "https://example/run/7"}, io.Discard, io.Discard)
	if code != exitFindings {
		t.Fatalf("exit = %d, want %d", code, exitFindings)
	}
	title, err := os.ReadFile(filepath.Join(out, titleFile))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(title), "security-scan regression: GO-2021-0265 in Go module github.com/tidwall/gjson\n"; got != want {
		t.Errorf("title = %q, want %q", got, want)
	}
	body, err := os.ReadFile(filepath.Join(out, bodyFile))
	if err != nil {
		t.Fatal(err)
	}
	b := string(body)
	if n := strings.Count(b, "GO-2021-0265\t"); n != 1 {
		t.Errorf("GO-2021-0265 listed %d times in body, want 1:\n%s", n, b)
	}
	for _, want := range []string{
		"GO-2021-0265\tgithub.com/tidwall/gjson@v1.6.5\tfixed in v1.9.3",
		"GO-2024-2687\tstdlib@v1.22.1\tno fix",
		"https://example/run/7",
	} {
		if !strings.Contains(b, want) {
			t.Errorf("body missing %q:\n%s", want, b)
		}
	}
	if strings.Contains(b, "GO-2021-0113") {
		t.Errorf("body lists package-level-only GO-2021-0113:\n%s", b)
	}
}

// TestRenderDeterministic: the same findings, in any record order, render the
// same title and body, so file-issue's exact-title dedup suppresses a repeat.
func TestRenderDeterministic(t *testing.T) {
	raw, err := os.ReadFile("testdata/called.json")
	if err != nil {
		t.Fatal(err)
	}
	var msgs []json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(raw))
	for dec.More() {
		var m json.RawMessage
		if err := dec.Decode(&m); err != nil {
			t.Fatal(err)
		}
		msgs = append(msgs, m)
	}
	// Keep the config record first; reverse everything after it.
	reversed := []json.RawMessage{msgs[0]}
	for i := len(msgs) - 1; i > 0; i-- {
		reversed = append(reversed, msgs[i])
	}
	var buf bytes.Buffer
	for _, m := range reversed {
		buf.Write(m)
		buf.WriteByte('\n')
	}

	a, err := parse(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	b, err := parse(&buf)
	if err != nil {
		t.Fatal(err)
	}
	titleA, bodyA := render(a, "u")
	titleB, bodyB := render(b, "u")
	if titleA != titleB || bodyA != bodyB {
		t.Errorf("render differs by record order:\n%s\n%s\n---\n%s\n%s", titleA, bodyA, titleB, bodyB)
	}
}

func TestRenderCapsSymbols(t *testing.T) {
	v := vuln{ID: "GO-2026-0001", Module: "stdlib", Symbols: []string{"p.A", "p.B", "p.C", "p.D", "p.E", "p.F", "p.G"}}
	_, body := render([]vuln{v}, "u")
	if want := "calls p.A, p.B, p.C, p.D, p.E (+2 more)\t"; !strings.Contains(body, want) {
		t.Errorf("body missing %q:\n%s", want, body)
	}
	if strings.Contains(body, "p.F") {
		t.Errorf("body lists symbols past the cap:\n%s", body)
	}
}
