#!/usr/bin/env bash
# CYAG-11 — no test file is left out of the build without anyone knowing.
#
# The defect this check makes impossible: a `_test.go` with a `//go:build` that no CI command
# passes is not even compiled. It does not fail: it disappears. The suite stays green because that
# file, for the compiler, does not exist. It already happened in this repository — 13 files have
# `//go:build testing`, and the release run left all of them out.
#
# How it works: it does not read the tags by hand and does not trust a list someone wrote. It asks
# the compiler which test files it ACTUALLY compiles with the CI tags, for each of the systems the
# release builds, and compares that list with the files in the tree.
#
# A file may be missing from a system for a good reason: `//go:build linux` on a file testing code
# that exists only on Linux, or the `_darwin_test.go` suffix that constrains by name. What is NOT
# fine is a file compiled NOWHERE: it means its tag is outside those the CI passes, and that test
# never runs.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

TEST_TAGS="${TEST_TAGS:-testing}"
# The system the CI runs on, not the set of those we publish.
#
# The first version of this check accepted a file if it was compiled on AT LEAST ONE of linux and
# darwin. But both runs run on the Linux CI runners: a file marked
# `//go:build darwin` runs in no run, and the check declared it fine. Writing
# `//go:build testing && darwin` on health_test.go was enough to recreate the original defect of the
# ticket - the only test of the command feeding the HEALTHCHECK stopped running - and stay green.
#
# The declared invariant was "the files the CI commands actually compile"; the verified one was
# "compiled somewhere". Now they match.
# The platforms the agent actually runs on: the image is built linux/amd64 + linux/arm64
# (release.yml) and goreleaser produces the same two binaries. A test file must compile on ALL of
# them, not on at least one: a test marked `//go:build amd64` would pass on an amd64 runner and never
# cover the arm64 half of the fleet. Pinning them here also makes the outcome independent of the
# machine running the command: without it, the same tree is green on an arm64 Mac and red on an
# amd64 runner.
PIATTAFORME="${PIATTAFORME:-linux/amd64 linux/arm64}"
# The files that on purpose do NOT run in CI, because they are bound to a system other than the
# runner's. It is a hand-written list ON PURPOSE: adding one must be a decision, not a side effect
# of a hastily added `//go:build`.
ESENTI="${ESENTI:-$(dirname "${BASH_SOURCE[0]}")/test-non-in-ci.txt}"

command -v go >/dev/null || { echo "go is missing" >&2; exit 3; }

# The test files the compiler compiles for a given system, with the CI tags.
#
# Fail-closed. If `go list` fails - a malformed `//go:build` is enough, and it stops loading ALL the
# packages - the output is empty, and an empty list is indistinguishable from "no file compiled".
# Without this check the command would say that 35 files out of 35 do not run, when the real failure
# is a single one and lies elsewhere. It is the same defect this work must make impossible.
compiled_on() {
  local platform="$1" output rc
  local os="${platform%%/*}" arch="${platform##*/}"
  set +e
  output="$(GOOS="$os" GOARCH="$arch" go list -tags "$TEST_TAGS" -f '{{$d := .Dir}}{{range .TestGoFiles}}{{$d}}/{{.}}
{{end}}{{range .XTestGoFiles}}{{$d}}/{{.}}
{{end}}' ./... 2> "$ERRORS")"
  rc=$?
  set -e
  if [ $rc -ne 0 ]; then
    echo >&2
    echo "::error::cannot list the test files for $platform: the code does not load." >&2
    sed 's/^/  /' "$ERRORS" >&2
    echo >&2
    echo "Until this is fixed the tag check cannot say anything, and it does not." >&2
    exit 2
  fi
  printf '%s\n' "$output" | sed "s#^$ROOT/##" | grep '_test\.go$' || true
}

# All the test files in the tree, excluding those that are not ours.
in_tree() {
  find . -name '*_test.go' -not -path './vendor/*' -not -path './dist/*' \
    | sed 's#^\./##' | sort
}

constraints_of() {
  # The file's `//go:build` lines, which end up in the error message: the reader must know which
  # tag to remove without opening the file.
  grep -h '^//go:build' "$1" 2>/dev/null | sed 's#^//go:build ##' | paste -sd' | ' - || true
}

TREE="$(in_tree)"
TOTAL="$(echo "$TREE" | grep -c '_test\.go$' || true)"

ERRORS="$(mktemp)"
trap 'rm -f "$ERRORS"' EXIT

echo "test build tags: TEST_TAGS=$TEST_TAGS"
echo "published platforms: $PIATTAFORME"

# The INTERSECTION, not the union: a file counts as covered only if it compiles on all of them.
COMPILED=""
for platform in $PIATTAFORME; do
  on_this="$(compiled_on "$platform" | sort)"
  echo "  $platform: $(echo "$on_this" | grep -c '_test\.go$' || true) test files out of $TOTAL"
  if [ -z "$COMPILED" ]; then
    COMPILED="$on_this"
  else
    COMPILED="$(comm -12 <(echo "$COMPILED") <(echo "$on_this"))"
  fi
done

# The files we know do not run in CI, one per line. Empty lines and comments ignored.
EXPECTED_OUT="$(grep -vE '^\s*(#|$)' "$ESENTI" 2>/dev/null | sed 's#[[:space:]]*$##' | sort || true)"

OUT="$(comm -23 <(echo "$TREE") <(echo "$COMPILED" | sort) | grep '_test\.go$' || true)"
OUT="$(echo "$OUT" | grep -v '^$' | sort || true)"

UNEXPECTED="$(comm -23 <(echo "$OUT") <(echo "$EXPECTED_OUT") | grep -v '^$' || true)"
STALE="$(comm -13 <(echo "$OUT") <(echo "$EXPECTED_OUT") | grep -v '^$' || true)"

if [ -n "$UNEXPECTED" ]; then
  echo >&2
  echo "::error::these test files are not compiled on all the published platforms ($PIATTAFORME), and are not declared among the exemptions." >&2
  echo "They do not fail: they do not run at all, and the suite stays green without them." >&2
  while IFS= read -r file; do
    [ -n "$file" ] || continue
    constraints="$(constraints_of "$file")"
    echo "::error file=$file::$file — constraint: ${constraints:-no //go:build, the name suffix constrains it}" >&2
    echo "  $file" >&2
    echo "    declared constraint: ${constraints:-no //go:build (the file name suffix constrains it)}" >&2
    echo "    tags passed by the CI: $TEST_TAGS" >&2
  done <<< "$UNEXPECTED"
  echo >&2
  echo "Remedy: remove the constraint, add the tag to TEST_TAGS in the Makefile, or - if that test" >&2
  echo "really cannot run in CI - declare it in $ESENTI, which is a deliberate choice." >&2
  exit 1
fi

if [ -n "$STALE" ]; then
  echo >&2
  echo "::error::these files are declared among the exemptions but do run in CI (or no longer exist):" >&2
  echo "$STALE" | sed 's#^#  #' >&2
  echo "An exemption that is no longer needed hides the next file that really stops running." >&2
  exit 1
fi

if [ -n "$EXPECTED_OUT" ]; then
  echo "  exempt (do not run in CI, declared in $(basename "$ESENTI")):"
  echo "$EXPECTED_OUT" | sed 's#^#    #'
fi
echo "all $TOTAL test files run in CI, except $(echo "$EXPECTED_OUT" | grep -c '[^[:space:]]' || true) declared."
