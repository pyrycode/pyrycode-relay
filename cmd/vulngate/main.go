// Command vulngate turns `govulncheck -json` output into an exit status and,
// when the code calls a vulnerable function, the issue title and body that the
// security-scan workflow's file-issue job posts.
//
// In JSON mode govulncheck exits 0 whatever it finds, and it emits an osv
// record for every advisory against a required module, reachable or not.
// vulngate applies the rule that makes text-mode govulncheck exit 3: a finding
// is relevant only when the first frame of its trace names a function
// (symbol level). Module- and package-level findings and bare osv records are
// not.
//
// Exit status: 0 when nothing relevant was found, 3 when something was (and the
// issue files were written), 1 on any malformed input or I/O failure (and no
// issue files were written).
//
// Usage:
//
//	vulngate -in govulncheck.json -out DIR -run-url URL
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	exitClean    = 0
	exitError    = 1
	exitFindings = 3

	titleFile = "issue-title.txt"
	bodyFile  = "issue-body.md"

	maxTokenLen   = 200
	maxSummaryLen = 200
	// Caps keep the body far below GitHub's issue-body limit: one reachable
	// stdlib advisory can list dozens of called symbols.
	maxSymbols = 5
	maxVulns   = 100
)

// message is one record of the govulncheck v1 JSON stream. Exactly one field
// is set; progress, SBOM and unknown record kinds decode to all-nil and are
// ignored.
type message struct {
	Config  *config  `json:"config"`
	OSV     *osv     `json:"osv"`
	Finding *finding `json:"finding"`
}

type config struct {
	ProtocolVersion string `json:"protocol_version"`
	ScanLevel       string `json:"scan_level"`
}

type osv struct {
	ID      string `json:"id"`
	Summary string `json:"summary"`
}

type finding struct {
	OSV          string  `json:"osv"`
	FixedVersion string  `json:"fixed_version"`
	Trace        []frame `json:"trace"`
}

type frame struct {
	Module   string `json:"module"`
	Version  string `json:"version"`
	Package  string `json:"package"`
	Function string `json:"function"`
	Receiver string `json:"receiver"`
}

// vuln is one advisory whose vulnerable code the scanned module calls,
// aggregated over all of its symbol-level finding records.
type vuln struct {
	ID      string
	Module  string
	Version string
	Fixed   string
	Symbols []string
	Summary string
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("vulngate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	in := fs.String("in", "", "govulncheck -json output file")
	out := fs.String("out", ".", "directory for "+titleFile+" and "+bodyFile)
	runURL := fs.String("run-url", "", "workflow run URL for the issue body")
	if err := fs.Parse(args); err != nil {
		return exitError
	}
	if *in == "" || fs.NArg() != 0 {
		fmt.Fprintln(stderr, "vulngate: usage: vulngate -in FILE [-out DIR] [-run-url URL]")
		return exitError
	}

	f, err := os.Open(*in)
	if err != nil {
		fmt.Fprintf(stderr, "vulngate: opening govulncheck output: %v\n", err)
		return exitError
	}
	defer f.Close()

	vulns, err := parse(bufio.NewReader(f))
	if err != nil {
		fmt.Fprintf(stderr, "vulngate: unusable govulncheck output: %v\n", err)
		return exitError
	}
	if len(vulns) == 0 {
		fmt.Fprintln(stdout, "vulngate: no called vulnerable functions")
		return exitClean
	}

	title, body := render(vulns, *runURL)
	// Body before title: the workflow uploads only when the title file
	// exists, so a failed second write never ships a title without a body.
	if err := os.WriteFile(filepath.Join(*out, bodyFile), []byte(body), 0o644); err != nil {
		fmt.Fprintf(stderr, "vulngate: writing issue body: %v\n", err)
		return exitError
	}
	if err := os.WriteFile(filepath.Join(*out, titleFile), []byte(title+"\n"), 0o644); err != nil {
		fmt.Fprintf(stderr, "vulngate: writing issue title: %v\n", err)
		return exitError
	}
	fmt.Fprintf(stdout, "vulngate: %d called vulnerable advisories\n%s", len(vulns), body)
	return exitFindings
}

// parse reads a govulncheck v1 JSON stream and returns the advisories with at
// least one symbol-level finding, one entry per OSV id, sorted by id. Any
// input that is not a complete symbol-level govulncheck stream is an error, so
// an empty, truncated or differently-scoped scan never reads as clean.
func parse(r io.Reader) ([]vuln, error) {
	dec := json.NewDecoder(r)
	summaries := map[string]string{}
	byID := map[string]*vuln{}
	symbols := map[string]map[string]bool{}
	seenConfig := false

	for i := 0; ; i++ {
		var m message
		if err := dec.Decode(&m); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, fmt.Errorf("decoding record %d: %w", i, err)
		}

		switch {
		case m.Config != nil:
			if i != 0 {
				return nil, fmt.Errorf("record %d: config record after the first", i)
			}
			if !strings.HasPrefix(m.Config.ProtocolVersion, "v1.") {
				return nil, fmt.Errorf("unsupported protocol_version %q", m.Config.ProtocolVersion)
			}
			if m.Config.ScanLevel != "symbol" {
				return nil, fmt.Errorf("scan_level %q, want symbol", m.Config.ScanLevel)
			}
			seenConfig = true
		case !seenConfig:
			return nil, errors.New("stream does not start with a config record")
		case m.OSV != nil:
			summaries[m.OSV.ID] = m.OSV.Summary
		case m.Finding != nil:
			fd := m.Finding
			if fd.OSV == "" || len(fd.Trace) == 0 {
				return nil, fmt.Errorf("record %d: finding without osv id or trace", i)
			}
			top := fd.Trace[0]
			if top.Function == "" {
				continue // module- or package-level: required or imported, never called
			}
			sym := top.Package + "."
			if top.Receiver != "" {
				sym += top.Receiver + "."
			}
			sym += top.Function
			for _, s := range []string{fd.OSV, top.Module, top.Version, fd.FixedVersion, sym} {
				if s != "" && !safeToken(s) {
					return nil, fmt.Errorf("record %d: unsafe string %q", i, s)
				}
			}
			if top.Module == "" {
				return nil, fmt.Errorf("record %d: symbol-level finding without module", i)
			}
			v := byID[fd.OSV]
			if v == nil {
				v = &vuln{ID: fd.OSV, Module: top.Module, Version: top.Version, Fixed: fd.FixedVersion}
				byID[fd.OSV] = v
				symbols[fd.OSV] = map[string]bool{}
			}
			symbols[fd.OSV][sym] = true
		}
	}
	if !seenConfig {
		return nil, errors.New("empty stream")
	}

	var vulns []vuln
	for id, v := range byID {
		for s := range symbols[id] {
			v.Symbols = append(v.Symbols, s)
		}
		sort.Strings(v.Symbols)
		v.Summary = cleanSummary(summaries[id])
		vulns = append(vulns, *v)
	}
	sort.Slice(vulns, func(i, j int) bool { return vulns[i].ID < vulns[j].ID })
	return vulns, nil
}

// render builds the issue title and body. The title names only the first
// advisory by id, so the same set of findings always yields the same title
// and file-issue's exact-title dedup suppresses a repeat.
func render(vulns []vuln, runURL string) (title, body string) {
	primary := vulns[0]
	title = fmt.Sprintf("security-scan regression: %s in Go module %s", primary.ID, primary.Module)

	var b strings.Builder
	b.WriteString("## Periodic security-scan regression\n\n")
	b.WriteString("**Scanner:** govulncheck (Go modules, called functions only)\n")
	fmt.Fprintf(&b, "**Primary finding:** `%s` in `%s`\n", primary.ID, primary.Module)
	fmt.Fprintf(&b, "**Workflow run:** %s\n\n", runURL)
	b.WriteString("### All findings\n\n```\n")
	for i, v := range vulns {
		if i == maxVulns {
			fmt.Fprintf(&b, "(+%d more advisories)\n", len(vulns)-maxVulns)
			break
		}
		fixed := "no fix"
		if v.Fixed != "" {
			fixed = "fixed in " + v.Fixed
		}
		mod := v.Module
		if v.Version != "" {
			mod += "@" + v.Version
		}
		syms := v.Symbols
		more := ""
		if len(syms) > maxSymbols {
			more = fmt.Sprintf(" (+%d more)", len(syms)-maxSymbols)
			syms = syms[:maxSymbols]
		}
		fmt.Fprintf(&b, "%s\t%s\t%s\tcalls %s%s\t%s\n", v.ID, mod, fixed, strings.Join(syms, ", "), more, v.Summary)
	}
	b.WriteString("```\n\n### Triage\n")
	b.WriteString("- Run `govulncheck ./...` locally against the named module to see call-site traces.\n")
	b.WriteString("- Bump the offending dependency (or the Go toolchain for `stdlib`).\n")
	b.WriteString("- Verify the next `security-scan` workflow run is green.\n")
	b.WriteString("- Close this issue once the next run passes.\n")
	return title, b.String()
}

// safeToken reports whether s can go into the issue title and the quoted
// `gh issue list --search` argument unchanged: bounded printable ASCII with
// no whitespace, quotes, backticks, `$` or backslash.
func safeToken(s string) bool {
	if s == "" || len(s) > maxTokenLen {
		return false
	}
	for _, c := range []byte(s) {
		if c <= ' ' || c >= 0x7f || strings.IndexByte("\"`$\\", c) >= 0 {
			return false
		}
	}
	return true
}

// cleanSummary flattens advisory free text to one bounded line with no
// backticks, so it cannot break out of the body's code block.
func cleanSummary(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == '`' || r < ' ' && r != '\t' && r != '\n' && r != '\r' || r == 0x7f {
			return -1
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > maxSummaryLen {
		cut := maxSummaryLen
		for cut > 0 && (s[cut]&0xc0) == 0x80 {
			cut--
		}
		s = s[:cut]
	}
	return s
}
