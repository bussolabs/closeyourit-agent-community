BINARY := closeyourit-agent
MODULE := github.com/bussolabs/closeyourit-agent
VERSION ?= dev
LDFLAGS := -s -w -X $(MODULE)/internal/version.Version=$(VERSION)

# Build tag required to compile the whole suite: without it, files marked //go:build testing are
# not even compiled - they do not fail, they disappear. How many there are is reported by
# `make tags-check`, which compares the tree with what the compiler actually compiles; a number
# written here would go stale with the first file added.
TEST_TAGS := testing
# Minimum coverage threshold enforced by the gate (make cover). Anti-regression floor:
# raise it once the real value on the Linux CI has been observed.
COVERAGE_THRESHOLD ?= 55

.PHONY: build test vet lint run clean docker cover tags-check

build:
	CGO_ENABLED=0 go build -ldflags '$(LDFLAGS)' -o $(BINARY) ./cmd/closeyourit-agent

test:
	go test -tags $(TEST_TAGS) ./...

vet:
	go vet -tags $(TEST_TAGS) ./...

cover:
	go test -tags $(TEST_TAGS) -coverprofile=coverage.out ./...
	@total=$$(go tool cover -func=coverage.out | awk '/^total:/ {print $$3}' | tr -d '%'); \
	echo "Total coverage: $$total% (minimum threshold $(COVERAGE_THRESHOLD)%)"; \
	awk -v t="$$total" -v thr="$(COVERAGE_THRESHOLD)" 'BEGIN { if (t+0 < thr+0) { printf "Coverage %.1f%% below the threshold %s%%\n", t, thr; exit 1 } }'

# No test file is left out of the build without anyone knowing: a `//go:build` that no CI command
# passes does not make that test fail, it makes it DISAPPEAR, and the suite stays green without it.
# It runs in both pipelines, the pull request one and the release one (CYAG-11).
tags-check:
	TEST_TAGS='$(TEST_TAGS)' ./script/tags-check.sh

lint:
	golangci-lint run ./...

run: build
	./$(BINARY)

docker:
	docker build -t ghcr.io/bussolabs/closeyourit-agent:dev .

clean:
	rm -f $(BINARY) coverage.out
	rm -rf dist
