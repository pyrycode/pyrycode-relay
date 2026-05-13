package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"testing"
)

// TestBinaryDoesNotImportPprof asserts that net/http/pprof is not in the
// transitive import set of the cmd/pyrycode-relay package. A blank-import
// of net/http/pprof anywhere in the dependency graph registers
// /debug/pprof/* handlers on http.DefaultServeMux and can expose an
// unauthenticated profiler endpoint. The boot-time CheckListenerPorts
// guard catches the :6060-bind variant; this build-graph test catches
// the handler-registration variant that would attach to an existing
// mux rather than opening a new port.
//
// Implementation: shell out to `go list -deps -json` against the
// canonical import path. The output is a stream of concatenated JSON
// objects (one per dep); we decode in a loop and fail if any
// ImportPath equals "net/http/pprof".
func TestBinaryDoesNotImportPprof(t *testing.T) {
	t.Parallel()

	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skipf("go binary not in PATH: %v", err)
	}

	const importPath = "github.com/pyrycode/pyrycode-relay/cmd/pyrycode-relay"
	cmd := exec.Command(goBin, "list", "-deps", "-json", importPath)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("go list -deps -json %s: %v\nstderr: %s", importPath, err, stderr.String())
	}

	dec := json.NewDecoder(&stdout)
	for {
		var pkg struct {
			ImportPath string `json:"ImportPath"`
		}
		if err := dec.Decode(&pkg); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatalf("decoding go list output: %v", err)
		}
		if pkg.ImportPath == "net/http/pprof" {
			t.Fatalf("net/http/pprof is in the transitive imports of %s; "+
				"remove the import or it will register debug handlers on "+
				"http.DefaultServeMux", importPath)
		}
	}
}
