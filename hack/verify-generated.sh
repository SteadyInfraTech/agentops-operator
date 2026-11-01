#!/usr/bin/env bash
# Fails when generated code or manifests differ from what is committed.
#
# Expects `make manifests generate` to have run already (the Makefile target
# `verify-generated` takes care of that) and CONTROLLER_GEN to point at the
# controller-gen binary.
set -euo pipefail

: "${CONTROLLER_GEN:?CONTROLLER_GEN must point at the controller-gen binary}"

fail() {
  echo "ERROR: $1 Run 'make manifests generate' and commit the result." >&2
  exit 1
}

# 1. Modified or new files under api/ and config/ after regeneration.
if [ -n "$(git status --porcelain -- api config)" ]; then
  git --no-pager diff --stat -- api config
  git --no-pager diff -- api config
  fail "generated files are out of date."
fi

# 2. controller-gen only writes files for API types that exist and never prunes
# old ones, so a removed type would leave its CRD behind unnoticed. Regenerate
# the CRDs into an empty directory and compare the file set as well as contents.
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
"$CONTROLLER_GEN" crd paths="./..." output:crd:artifacts:config="$tmp"
if ! diff -rq "$tmp" config/crd/bases; then
  fail "config/crd/bases does not match the API types (obsolete or missing CRD file?)."
fi
