package main

import (
	"bytes"
	"context"
	"go/ast"
	"go/constant"
	"go/format"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pyrycode/pyrycode-relay/internal/relay"
)

// publicHeaderLimits reads the actual wiring literals so a forgotten listener
// cannot pass a behavioural test that only copies the intended policy value.
func publicHeaderLimits(t *testing.T) map[string]int {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	limits := make(map[string]int)
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "run" {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			typ, ok := lit.Type.(*ast.SelectorExpr)
			if !ok || typ.Sel.Name != "Server" {
				return true
			}
			pkg, ok := typ.X.(*ast.Ident)
			if !ok || pkg.Name != "http" {
				return true
			}
			fields := make(map[string]string)
			for _, elt := range lit.Elts {
				kv := elt.(*ast.KeyValueExpr)
				var value bytes.Buffer
				if err := format.Node(&value, fset, kv.Value); err != nil {
					t.Fatal(err)
				}
				fields[kv.Key.(*ast.Ident).Name] = value.String()
			}
			name := fields["Addr"]
			for field, want := range map[string]string{
				"ReadHeaderTimeout": "5 * time.Second",
				"ReadTimeout":       "60 * time.Second",
				"WriteTimeout":      "60 * time.Second",
				"IdleTimeout":       "120 * time.Second",
			} {
				if got := fields[field]; got != want {
					t.Errorf("%s %s = %q, want %q", name, field, got, want)
				}
			}
			// Missing MaxHeaderBytes means Go's default, which the tests must
			// exercise as well as reporting the wiring omission.
			limit := int64(0)
			if expr := fields["MaxHeaderBytes"]; expr != "" {
				value, err := types.Eval(fset, nil, token.NoPos, expr)
				if err != nil {
					t.Fatalf("%s MaxHeaderBytes: %v", name, err)
				}
				var exact bool
				limit, exact = constant.Int64Val(value.Value)
				if !exact {
					t.Fatalf("%s MaxHeaderBytes is not an integer", name)
				}
			}
			if limit != 16*1024 {
				t.Errorf("%s MaxHeaderBytes = %d, want 16384", name, limit)
			}
			limits[name] = int(limit)
			return false
		})
	}
	if len(limits) != 3 {
		t.Fatalf("found %d public server literals, want 3", len(limits))
	}
	for _, name := range []string{"*insecureListen", "*httpsListen", "*httpListen"} {
		if _, ok := limits[name]; !ok {
			t.Fatalf("missing public server %s", name)
		}
	}
	return limits
}

func TestPublicServers_OversizedHeadersRejectedBeforeHandler(t *testing.T) {
	for name, limit := range publicHeaderLimits(t) {
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.WriteHeader(http.StatusNoContent)
			}))
			srv.Config.MaxHeaderBytes = limit
			if name == "*httpsListen" {
				srv.StartTLS()
			} else {
				srv.Start()
			}
			defer srv.Close()
			client := srv.Client()
			client.Timeout = 3 * time.Second
			for _, tc := range []struct {
				name, value string
				wantStatus  int
			}{
				{"ordinary", "small", http.StatusNoContent},
				{"oversized", strings.Repeat("a", 64*1024), http.StatusRequestHeaderFieldsTooLarge},
			} {
				req, err := http.NewRequest(http.MethodGet, srv.URL, nil)
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("X-Test-Header", tc.value)
				resp, err := client.Do(req)
				if err != nil {
					t.Fatalf("%s request: %v", tc.name, err)
				}
				resp.Body.Close()
				if resp.Proto != "HTTP/1.1" || resp.StatusCode != tc.wantStatus {
					t.Errorf("%s response = %s %d, want HTTP/1.1 %d", tc.name, resp.Proto, resp.StatusCode, tc.wantStatus)
				}
				if got := calls.Load(); got != 1 {
					t.Errorf("%s handler calls = %d, want 1 (ordinary request only)", tc.name, got)
				}
			}
		})
	}
}

func TestPublicServers_HTTP01ResponsesUnchanged(t *testing.T) {
	limit := publicHeaderLimits(t)["*httpListen"]
	mgr, err := relay.NewAutocertManager("relay.invalid", t.TempDir()+"/certs")
	if err != nil {
		t.Fatal(err)
	}
	// Autocert's cache key lets the handler serve a local challenge without
	// issuing a certificate or contacting an ACME service.
	const body = "fixture-token.fixture-authorization"
	if err := mgr.Cache.Put(context.Background(), "fixture-token+http-01", []byte(body)); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(mgr.HTTPHandler(http.NotFoundHandler()))
	srv.Config.MaxHeaderBytes = limit
	srv.Start()
	defer srv.Close()
	client := srv.Client()
	client.Timeout = 3 * time.Second
	for _, tc := range []struct {
		path string
		code int
		body string
	}{
		{"/.well-known/acme-challenge/fixture-token", http.StatusOK, body},
		{"/", http.StatusNotFound, "404 page not found\n"},
		{"/healthz", http.StatusNotFound, "404 page not found\n"},
	} {
		req, err := http.NewRequest(http.MethodGet, srv.URL+tc.path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = "relay.invalid"
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != tc.code || string(got) != tc.body {
			t.Errorf("GET %s = %d %q, want %d %q", tc.path, resp.StatusCode, got, tc.code, tc.body)
		}
	}
}
