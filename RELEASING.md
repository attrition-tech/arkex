# Releasing arkex

Signed releases are static files on Cloudflare R2 at `https://get.arkex.dev`.
`install.sh` and `arkex update` continue to use that host. Source and release
downloads are also published at `https://github.com/attrition-tech/arkex`.
The Go module path remains unchanged so existing imports keep working.

## Layout on the host

```
<base>/install.sh                                  installer (curl | sh)
<base>/stable.json                                 {"version":"0.1.0","published_at":…}
<base>/v0.1.0/arkex_0.1.0_<os>_<arch>.tar.gz       (zip on windows)
<base>/v0.1.0/SHA256SUMS
<base>/v0.1.0/SHA256SUMS.sig                       "<keyid> <base64 ed25519 signature>"
```

Trust chain: public keys in `internal/update/keys.txt` (compiled into the
binary; PEM copy in `install.sh`) → verify `SHA256SUMS.sig` → `SHA256SUMS`
pins each archive. The manifest is unsigned on purpose: a hostile host can
withhold an update but cannot install anything unsigned, and clients never
move to a lower version. Per-version directories are immutable; only
`stable.json` and `install.sh` change.

## One-time setup

1. Generate the release key (print once, store as a secret, never commit):

   ```sh
   go run ./scripts/relsign keygen > signing.key   # stdout = private seed (base64)
   ```

   stderr prints the `keys.txt` line and the PEM for `install.sh`. Commit
   both; keep `signing.key` only in the `ARKEX_SIGNING_KEY` secret.
   Rotation: add the new line to `keys.txt`, ship a release signed by the
   old key, then switch signing to the new key. Users who skip that
   intermediate release must reinstall.

2. Cloudflare R2: create a bucket, connect a custom domain (e.g.
   `get.arkex.dev`, public access), and create an API token with
   *Object Read & Write* on that bucket.

3. Secrets/environment for the release machine (Amp project secrets when
   releasing from an orb):

   | Variable | Value |
   | --- | --- |
   | `ARKEX_SIGNING_KEY` | base64 seed from step 1 |
   | `ARKEX_DOWNLOAD_BASE` | `https://get.arkex.dev` (baked into the binary as the update source) |
   | `ARKEX_R2_BUCKET` | bucket name |
   | `ARKEX_R2_ENDPOINT` | `https://<account-id>.r2.cloudflarestorage.com` |
   | `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` | R2 API token |

## Cut a release

### Native CI gate

`.github/workflows/ci.yml` builds, vets and tests on Linux, macOS and Windows.
Linux/macOS run the race detector; Windows runs the normal suite. Each platform
also builds and executes the real CLI against an isolated local fake provider:
model listing, streaming print/JSON output, and a file-tool round trip. Logs are
retained as Actions artifacts for 14 days. These tests need no provider secrets.
The tag-triggered GitHub release workflow waits for this entire CI workflow on
the tagged revision before publishing.

Push changes to both the Amp `origin` and GitHub `github` remotes. A push to Amp
alone does not run native CI. Local `scripts/release.sh publish` now requires a
successful GitHub `ci.yml` main push run for the exact commit being released.
The GitHub tag workflow always verifies; its publish job is disabled until the
repository variable `ARKEX_RELEASE_PUBLISH` is set to `true` and the release
secrets/variables above are installed. Do not enable it while publishing the
same tag locally. The Amp GitHub App cannot administer Actions secrets; an owner
must configure them in GitHub settings. No release credentials are in the repo.

```sh
go build -o arkex-smoke ./cmd/arkex  # use arkex-smoke.exe on Windows
python scripts/smoke.py ./arkex-smoke
```

The smoke test uses an ephemeral port and disposable directories (including
spaces in paths); it neither contacts a real LLM nor modifies a user's config.
Hosted CI does **not** establish clipboard interoperability, physical frame rate,
real sleep/wake behavior, or terminal rendering in Terminal.app/Windows Terminal.
Keep the real-terminal checklist below as the release gate for those behaviors.

For TUI changes, run the native-terminal gate before publishing:

- On macOS arm64, paste/submit/recall a 20-line prompt. Check that the cursor
  stays inside it when resizing, including narrow and short windows.
- Drag-select prompt and transcript text; copy into another application. Check
  Ctrl+C selection precedence, terminal-forwarded Cmd+C/Ctrl+Shift+C, OSC 52 over
  SSH, and palette copying of Markdown and fenced/indented code.
- Exercise approval/retry dialogs at 24×8, then resize larger. All decision
  buttons must remain reachable; previews must recover their scroll behavior.
- Check emoji/CJK table borders and terminal tab titles in the terminals being
  supported. An ANSI-to-browser screenshot does not verify terminal glyph widths.
- On a high-refresh display, inspect scrolling and streaming frame pacing.
  `go test ./internal/tui -run '^$' -bench 'LongHistoryFrame|StreamingMarkdownSnapshot' -benchmem`
  measures UI work, not physical 120 FPS. Keep fixed fixtures/machine settings
  for before/after comparisons; distinguish cold CLI startup from warm dispatch
  when comparing other harnesses.

Increment only the patch component by default: `0.7.0` → `0.7.1` →
`0.7.2`. Do not bump the minor or major version without the owner's explicit
request. Versions are SemVer components, not decimal fractions; the patch
number is not capped at 99. Set the version by tagging only when publishing,
not during development. Publishing and pushing require authorization.

```sh
git tag -a v0.1.0 -m v0.1.0
scripts/release.sh publish
git push --atomic origin main v0.1.0
git push --atomic github main v0.1.0
gh release create v0.1.0 --repo attrition-tech/arkex --verify-tag \
  dist/arkex_*.tar.gz dist/arkex_*.zip dist/SHA256SUMS dist/SHA256SUMS.sig \
  --title 'v0.1.0' --notes 'Describe the changes and verification.'
```

`publish` refuses a dirty tree or an untagged HEAD, runs tests, builds all
targets one at a time (`--parallelism 1`; five parallel Go links exhaust an
8 GB machine), signs `SHA256SUMS`, and uploads `v0.1.0/…`, then
`stable.json` and `install.sh`. Then push the tag along with `main`.

## Test the whole flow locally

```sh
ARKEX_SIGNING_KEY=… scripts/release.sh local          # → .release/site, base http://127.0.0.1:8766
python3 -m http.server 8766 --bind 127.0.0.1 --directory .release/site &
HOME=/tmp/h ARKEX_DOWNLOAD_BASE=http://127.0.0.1:8766 sh .release/site/install.sh
GORELEASER_CURRENT_TAG=v9.9.9 ARKEX_SIGNING_KEY=… scripts/release.sh local   # a "newer" build
/tmp/h/.arkex/bin/arkex update
```

Snapshot versions look like `0.1.1-dev.<sha>`: valid semver, above the last
tag, so a dev channel can reuse the same machinery later.

`go test ./internal/update/` covers manifest parsing, semver ordering, key
parsing, signature/checksum rejection, self-test failure rollback and the
atomic swap against an in-process fake host.

## Static install site

`site/` is plain HTML/CSS/JavaScript with no build step, analytics, or external
fonts. `.github/workflows/pages.yml` deploys it on site changes and supports
manual dispatch. Enable **Settings → Pages → Source: GitHub Actions** once in
`attrition-tech/arkex`. Default URL: `https://attrition-tech.github.io/arkex/`.
The Amp GitHub App may lack permission to enable Pages; an owner must do that.

To use `arkex.dev`, first verify the domain in GitHub Pages, then configure the
custom domain and the DNS records GitHub specifies. Keep `get.arkex.dev` pointing
at R2; changing it would break existing installations. Do not add a CNAME file
or change DNS before the Pages site and domain ownership are ready.
