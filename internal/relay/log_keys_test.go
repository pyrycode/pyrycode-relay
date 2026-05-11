package relay

// TestLogKeysAreAllowlisted enforces docs/threat-model.md § Log hygiene at
// test time. It AST-walks every non-test .go file in this package, finds
// every selector call whose method name is Info/Warn/Error/Debug (the four
// slog.Logger level methods used in this package), extracts the string-
// literal key arguments, and fails if any key is absent from
// allowedLogKeys in log_allowlist.go.
//
// Matched shape: any *ast.CallExpr whose Fun is a *ast.SelectorExpr with
// Sel.Name in {Info, Warn, Error, Debug}. The walker matches by selector
// name only, not by receiver type — so a package-level slog.Info(...) is
// caught the same way as logger.Info(...). False positives from an
// unrelated type that happens to have an Info/Warn/Error/Debug method are
// not present in this package today; if one is introduced later, the
// violation message names file:line so a reviewer can disambiguate.
//
// Narrowing: a matched call's first positional argument must itself be a
// string literal (the slog msg convention used at every existing call
// site). This filters out unrelated same-named methods/funcs whose first
// arg is something else — notably http.Error(w, msg, code) where the
// first arg is an http.ResponseWriter. A future log call with a
// computed msg would slip past the matcher, but the threat model gates
// keys, not msgs; if a contributor needs computed msgs, the convention
// (and this filter) gets revisited in the same PR.
//
// Bypasses the walker explicitly rejects, with distinct error messages:
//   - non-literal key argument (e.g. a variable, slog.String(...),
//     fmt.Sprintf(...)) — dynamic keys defeat the allowlist.
//   - odd-length key/value arg list — malformed; stops walking that call.
//   - logger.With(...) / LogAttrs(...) / Log(...) — these methods accept
//     keys in shapes this walker does not parse. A non-test file using
//     them must either be converted to positional Info/Warn/Error/Debug
//     or the walker must be extended in the same PR.
//
// Out of scope (deliberate, see spec): value-side leaks (an allowlisted
// key like "err" carrying a string that contains a secret). The test
// inspects keys only. Value-side redaction belongs in the future runtime
// slog middleware named in docs/threat-model.md § Future hardening.
//
// Negative-path manual check (do not commit a permanent negative test):
// to confirm the walker reports correctly, temporarily add e.g.
// `"forbidden_key", "x"` to a logger.Info call in this package and
// re-run `go test ./internal/relay/ -run TestLogKeysAreAllowlisted`. The
// failure should name the file, line, and offending key without -v.
// Revert before committing.

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestLogKeysAreAllowlisted(t *testing.T) {
	matches, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob *.go: %v", err)
	}
	var nonTest []string
	for _, f := range matches {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		nonTest = append(nonTest, f)
	}
	if len(nonTest) == 0 {
		t.Fatalf("no non-test .go files found in cwd; test is in the wrong directory")
	}

	levelMethods := map[string]struct{}{
		"Info":  {},
		"Warn":  {},
		"Error": {},
		"Debug": {},
	}
	unsupportedMethods := map[string]struct{}{
		"With":     {},
		"LogAttrs": {},
		"Log":      {},
	}

	var violations []string
	fset := token.NewFileSet()

	for _, path := range nonTest {
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}

		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			name := sel.Sel.Name

			if _, bad := unsupportedMethods[name]; bad {
				pos := fset.Position(call.Lparen)
				violations = append(violations, fmt.Sprintf(
					"%s: %s used; this method is not gated by TestLogKeysAreAllowlisted. "+
						"Either convert the call to positional Info/Warn/Error/Debug or extend the walker.",
					pos, selString(sel)))
				return true
			}
			if _, ok := levelMethods[name]; !ok {
				return true
			}

			// args[0] is the msg; key/value pairs follow. Require msg
			// to be a string literal — this is the slog convention at
			// every existing call site, and the filter that
			// distinguishes logger.Info(...) from same-named non-slog
			// calls like http.Error(w, "", code).
			if len(call.Args) < 1 {
				return true
			}
			msgLit, ok := call.Args[0].(*ast.BasicLit)
			if !ok || msgLit.Kind != token.STRING {
				return true
			}
			args := call.Args[1:]
			i := 0
			for i < len(args) {
				keyArg := args[i]
				lit, isLit := keyArg.(*ast.BasicLit)
				if !isLit || lit.Kind != token.STRING {
					kpos := fset.Position(keyArg.Pos())
					violations = append(violations, fmt.Sprintf(
						"%s: non-literal log key argument in %s call "+
							"(dynamic key defeats the allowlist; use a literal string key)",
						kpos, name))
					// Stop walking this call's args — we can't trust the pairing.
					break
				}
				key, err := strconv.Unquote(lit.Value)
				if err != nil {
					kpos := fset.Position(keyArg.Pos())
					violations = append(violations, fmt.Sprintf(
						"%s: unparseable string literal log key %s: %v",
						kpos, lit.Value, err))
					break
				}
				if _, ok := allowedLogKeys[key]; !ok {
					kpos := fset.Position(keyArg.Pos())
					violations = append(violations, fmt.Sprintf(
						"%s: log key %q is not in allowedLogKeys "+
							"(see internal/relay/log_allowlist.go and docs/threat-model.md § Log hygiene)",
						kpos, key))
				}
				// Skip the value at i+1; an odd-length list is a malformed call.
				if i+1 >= len(args) {
					kpos := fset.Position(keyArg.Pos())
					violations = append(violations, fmt.Sprintf(
						"%s: log key %q has no paired value (odd-length key/value list)",
						kpos, key))
					break
				}
				i += 2
			}
			return true
		})
	}

	if len(violations) == 0 {
		return
	}
	for _, v := range violations {
		t.Error(v)
	}
	t.Fatalf("found %d log-hygiene violation(s)", len(violations))
}

func selString(sel *ast.SelectorExpr) string {
	if id, ok := sel.X.(*ast.Ident); ok {
		return id.Name + "." + sel.Sel.Name
	}
	return "<expr>." + sel.Sel.Name
}
