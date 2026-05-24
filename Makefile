# pyrycode-relay — build / test / lint targets

BINARY      := pyrycode-relay
PKG         := ./cmd/pyrycode-relay
VERSION     ?= dev
LDFLAGS     := -s -w -X main.Version=$(VERSION)

.PHONY: build check test vet lint run clean

build:
	@mkdir -p bin
	go build -ldflags "$(LDFLAGS)" -o bin/$(BINARY) $(PKG)

# `check` is the QA agent's mechanical gate (vet + race tests). Mirrors
# pyrycode/pyrycode's `check` target so the agents repo's qa/CLAUDE.md
# can stay convergent across forks. Heavier static-analysis (gosec,
# govulncheck) lives in `lint` and is run by humans, not the dispatcher.
check: vet test

test:
	go test -race ./...

vet:
	go vet ./...

lint:
	@command -v gosec >/dev/null || { echo "gosec not installed: go install github.com/securego/gosec/v2/cmd/gosec@latest"; exit 1; }
	@command -v govulncheck >/dev/null || { echo "govulncheck not installed: go install golang.org/x/vuln/cmd/govulncheck@latest"; exit 1; }
	gosec ./...
	govulncheck ./...

run: build
	./bin/$(BINARY) --insecure-listen :8080

clean:
	rm -rf bin/ dist/ coverage.txt
