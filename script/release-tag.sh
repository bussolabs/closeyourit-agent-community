#!/usr/bin/env bash
# CYAG-11 — the shape of the release tag, and the channel that follows from it.
#
# One rule, in one place. It used to live inside the `docker` job step, and that left a crack open:
# the `goreleaser` job runs in parallel and did not go through there. A tag like `v1.2.3+rc1` made
# the image fail and let the GitHub release go ahead — published, to boot, as final, because
# goreleaser looks at the semver prerelease field and in `+rc1` that field is empty. Half a release
# done, half not, and the half that got done is the wrong one.
#
# Now the check runs in the test job, which both publishing jobs depend on: a malformed tag stops
# them before they start.
#
#   ./script/release-tag.sh --check v1.2.3     only validates
#   ./script/release-tag.sh --channel v1.2.3   prints stable|beta
#   ./script/release-tag.sh --moving v1.2.3    prints the moving reference: latest|beta
#
# Without a tag argument it reads GITHUB_REF_NAME.
set -eu

MODE="${1:---check}"
TAG="${2:-${GITHUB_REF_NAME:-}}"

# The full reference of an image cannot exceed 128 characters in the tag part, and our reference is
# `v` + version. A semver is legal at any length, so the regex alone is not enough: a long
# prerelease would pass here and be rejected by the registry halfway through publishing, with the
# version image already pushed and the moving one not.
MAX_TAG=128

if [ -z "$TAG" ]; then
  echo "::error::no tag to check: pass it as an argument or set GITHUB_REF_NAME" >&2
  exit 1
fi

# Strict semver. No `+build`: image references do not allow `+`.
# `grep -Eq` anchors to the LINE, not the string: a newline inside the value would make it enough
# for ONE line to be semver. Git does not allow control characters in ref names, but the check must
# not depend on that guarantee — so any multi-line value is rejected first.
# `$(printf '\n')` CANNOT be used here: command substitution strips trailing newlines, the result is
# the empty string, and `*""*` matches anything - rejecting every tag.
if [[ "$TAG" == *$'\n'* ]]; then
  echo "::error::the tag contains a newline, publishing rejected" >&2
  exit 1
fi

if [ "${#TAG}" -gt "$MAX_TAG" ]; then
  echo "::error::tag is ${#TAG} characters long: over the $MAX_TAG the registry accepts in an image reference" >&2
  exit 1
fi

if ! printf '%s' "$TAG" | grep -Eq '^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?$'; then
  echo "::error::tag is not semver, publishing rejected: $TAG" >&2
  echo "Allowed shape: vX.Y.Z for a final version, vX.Y.Z-<preview> for a preview." >&2
  echo "No build metadata (+...): the channel would decide it one way and goreleaser another." >&2
  exit 1
fi

# Preview = there is a dash, i.e. the semver prerelease field is not empty. It is the same rule
# goreleaser applies with `prerelease: auto` (it looks at the prerelease field of the parsed semver,
# not at the mere presence of a dash): since build metadata is forbidden here, the two rules cannot
# diverge.
if printf '%s' "$TAG" | grep -q -- '-'; then
  CHANNEL=beta
  MOVING=beta
else
  CHANNEL=stable
  MOVING=latest
fi

case "$MODE" in
  --check) echo "valid tag: $TAG (channel $CHANNEL)" ;;
  --channel) echo "$CHANNEL" ;;
  --moving) echo "$MOVING" ;;
  *) echo "::error::unknown mode: $MODE (expected --check, --channel, --moving)" >&2; exit 2 ;;
esac
