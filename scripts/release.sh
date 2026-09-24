#!/usr/bin/env bash
# Build and publish arkex releases (see .goreleaser.yaml for the layout).
#
#   scripts/release.sh local [DIR]     snapshot build → static tree in DIR
#                                      (default .release/site) for a local mirror
#   scripts/release.sh publish         build the checked-out tag and upload to R2
#
# Environment (publish requires all; local requires only ARKEX_SIGNING_KEY):
#   ARKEX_SIGNING_KEY     base64 ed25519 seed        (go run ./scripts/relsign keygen)
#   ARKEX_DOWNLOAD_BASE   public base URL baked into the binary, e.g. https://get.arkex.dev
#   ARKEX_R2_BUCKET       bucket name
#   ARKEX_R2_ENDPOINT     https://<account-id>.r2.cloudflarestorage.com
#   AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY   R2 API token (Object Read & Write)
#
# Builds run one target at a time (--parallelism 1): 5 concurrent Go link steps
# exhaust an 8 GB machine.
set -euo pipefail
cd "$(dirname "$0")/.."

mode="${1:-}"
need() { for v in "$@"; do [ -n "${!v:-}" ] || { echo "error: $v is not set" >&2; exit 1; }; done; }

case "$mode" in
  local)
    site="${2:-.release/site}"
    need ARKEX_SIGNING_KEY
    export ARKEX_DOWNLOAD_BASE="${ARKEX_DOWNLOAD_BASE:-http://127.0.0.1:8766}"
    goreleaser release --snapshot --clean --skip=publish --parallelism 1
    version=$(jq -r .version dist/metadata.json)
    rm -rf "$site" && mkdir -p "$site/v$version"
    cp dist/arkex_*.tar.gz dist/arkex_*.zip dist/SHA256SUMS dist/SHA256SUMS.sig "$site/v$version/"
    cp .release/stable.json .release/install.sh "$site/"
    echo
    echo "static release tree in $site (version $version, base $ARKEX_DOWNLOAD_BASE):"
    find "$site" -type f | sort
    ;;
  publish)
    need ARKEX_SIGNING_KEY ARKEX_DOWNLOAD_BASE ARKEX_R2_BUCKET ARKEX_R2_ENDPOINT AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY
    tag=$(git describe --tags --exact-match 2>/dev/null) || { echo "error: HEAD is not at a v* tag; run: git tag -a vX.Y.Z -m vX.Y.Z" >&2; exit 1; }
    if [ "${GITHUB_ACTIONS:-}" = true ]; then
      [ "${GITHUB_REF_TYPE:-}" = tag ] || { echo "error: publishing requires a tag push" >&2; exit 1; }
      tag="$GITHUB_REF_NAME"
      [ "$(git rev-parse "$tag^{commit}")" = "$(git rev-parse HEAD)" ] || { echo "error: tag does not match checkout" >&2; exit 1; }
      export GORELEASER_CURRENT_TAG="$tag"
    fi
    [ -z "$(git status --porcelain)" ] || { echo "error: working tree is dirty" >&2; exit 1; }
    git merge-base --is-ancestor HEAD origin/main || { echo "error: release commit must be on main" >&2; exit 1; }
    python3 scripts/release_control.py check "$tag"
    # The Actions publish job already depends on native verification. Local
    # publishing must verify the same commit passed the GitHub main CI run.
    if [ "${GITHUB_ACTIONS:-}" != true ]; then
      result=$(gh run list --repo attrition-tech/arkex --workflow ci.yml --branch main --event push \
        --commit "$(git rev-parse HEAD)" --limit 1 --json conclusion --jq '.[0].conclusion // "missing"')
      [ "$result" = success ] || { echo "error: native GitHub CI for HEAD must pass before publishing (state: $result)" >&2; exit 1; }
    fi
    go test ./... >/dev/null
    goreleaser release --clean --parallelism 1
    echo
    echo "published $tag → $ARKEX_DOWNLOAD_BASE"
    echo "  install: curl -fsSL $ARKEX_DOWNLOAD_BASE/install.sh | sh"
    echo "  update:  arkex update"
    ;;
  *)
    sed -n '2,18p' "$0" >&2
    exit 2
    ;;
esac
