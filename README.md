# arkex

A terminal AI agent for your own models. Read, edit, and run code with
OpenAI-compatible APIs, local model servers, or a ChatGPT subscription.
One binary. No telemetry. MIT licensed.

## Install

**macOS / Linux**

```sh
curl -fsSL https://get.arkex.dev/install.sh | sh
```

[Inspect the installer](https://get.arkex.dev/install.sh) before running it.
Installs to `~/.arkex/bin`. Releases include signed checksums; set
`ARKEX_REQUIRE_SIGNATURE=1` to require installer signature verification.

Supported: macOS Apple Silicon/Intel and Linux ARM64/x64.
Update with `arkex update`.

On Linux, shell and write-capable tools require distro-provided Bubblewrap at
`/usr/bin/bwrap` and enabled unprivileged user namespaces. Ubuntu releases that
restrict user namespaces through AppArmor also need an AppArmor profile that
permits `/usr/bin/bwrap` to create them; keep the system-wide restriction
enabled. On macOS, arkex uses the built-in `/usr/bin/sandbox-exec`. If the
platform sandbox is unavailable, affected tool execution is blocked. Windows is
not supported.

## Configure

Run `arkex` in your project, open `/connections`, and add your model server,
API connection, or ChatGPT account. Choose a model and start chatting.

Configuration and sessions stay in `~/.arkex/`. API keys can reference environment
variables instead of being stored directly, for example `"apiKey": "$MY_API_KEY"`.
Prompts and relevant project content are sent to the model provider you choose.

### Check reasoning on a custom server

While idle, open **Reasoning effort** (`/effort`) and choose **Check reasoning…**.
The explicit check uses the selected model's configured endpoint, key and headers.
It sends only a synthetic prompt, never conversation history, attachments or tools.
It tries the configured reasoning format first, then other formats Arkex supports,
preferring effort levels over a toggle-only result. It is not automatic discovery
and can incur provider charges: at most 35 requests, each requesting 64 output
tokens, with a 15-second request timeout and a three-minute overall limit.
There are no retries or redirects; Esc cancels the check.

Choices are reported as **accepted by server** only when valid settings are
accepted and a deliberately invalid control is rejected with a relevant validation
error. A permissive server is **inconclusive**, not assumed to ignore the field.
Request acceptance does not prove that the model uses a setting or that High
reasons more than Low. Authentication, rate-limit and transport failures discard
partial results. Fixed request/control overrides in `compat.extraBody` block the
check rather than being silently removed.

Review the result and choose **Use these options for this model** to save its
`reasoningLevels`, `compat.thinking` and `reasoningChecked` marker. This replaces
only that model's controls and resets the current session's effort to Default;
history, other models and the configured default selector stay unchanged.
Nothing is saved before this action. Later configuration edits prevent saving a
stale check. You can still configure levels manually; clear `reasoningChecked`
when replacing checked levels by hand. A check does not discover the server's
default effort.

### Sign in with ChatGPT

Choose **Subscription → ChatGPT → Continue with ChatGPT** in `/connections`.
Arkex uses OpenAI's official open-source Sign in with ChatGPT flow and public
Responses API, not the private Codex backend. Existing ChatGPT connections need
a fresh browser sign-in; old credentials cannot be reused. Your saved sessions
and connection settings remain. Refetch models after signing in to see the
models currently available to your account.

Signing in and granting ChatGPT plan usage are separate. If the browser grants
only identity access, Arkex keeps the registration but blocks inference and
offers **Enable ChatGPT plan usage**. Availability and limits depend on OpenAI's
preview and your account; manage usage at `https://chatgpt.com/settings/usage`.
An API-key connection remains a separate option.

Credentials, account-bound client IDs, and the installation's stable host ID
live in `~/.arkex/auth.json` with owner-only permissions. Refresh tokens rotate
under a cross-process lock. **Sign out** attempts remote revocation and clears
local tokens while keeping the registration and configured models. Deleting a
connection also attempts revocation, then forgets its local registration. If
revocation cannot be confirmed, disconnect the app in ChatGPT Settings.

Inference streams with `store=false` and explicit conversation/tool history.
Failed, incomplete, and disconnected streams are not treated as completed
responses. The current model SDK retains reasoning metadata locally but does
not replay encrypted reasoning; visible messages and tool results are replayed.

For terminal sound/visual alerts, add `"ui": {"bell": true}` to your config.
Alerts fire when retry input is needed and when a run completes, fails, or
pauses—not when you cancel. Off by default; your terminal controls the sound
and may need its bell enabled.

## Use

```sh
arkex                                 # interactive session
arkex -c                              # resume the latest session here
arkex -p "explain this project"        # one-shot answer
```

Tools do not require routine per-call approval. Permission policy uses only
`"allow"` and `"deny"`; known tools are allowed by default and unknown tools are
denied. Set global policy in `~/.arkex/config.json` and optionally tighten it in
`.arkex/config.json` in the project. Project configuration cannot relax a deny.
Config v4 migrates legacy `"ask"` entries to `"allow"` and renames the old
`install_dependency` permission to `packages`, preserving denies under either name.

Reads may access paths outside the workspace. Ordinary writes—including shell
changes—are limited to the fixed workspace and active, run-owned scratch
directory; changing a command's working directory does not expand those roots.
Shell commands run in private copies, never writable binds of live host files.
After completion, arkex captures output into separate trusted files and publishes
changed entries using fresh-file replacement. Existing outside hard links keep
their contents and permissions; subprocesses cannot create new hard links. The
startup probe checks this restriction. Missing confinement blocks execution.

Linux mounts the copies at the original paths. On macOS, use relative paths in
shell code and `$TMPDIR` for scratch: Seatbelt cannot remap paths, so original
absolute workspace/scratch paths remain read-only. The structured `workdir`
field is mapped automatically. Private execution paths expire after the call;
builds embedding absolute paths and absolute symlinks may need adjustment.

Publication preserves regular-file contents, permission bits and modification
times, directories, and symlink entries. Unchanged files are left alone. Host
edits conflicting with command changes stop preflight before any publication;
unrelated edits survive. This is optimistic, **not a multi-file transaction**:
an editor can race the final check/rename, and application failures can leave
earlier entries applied. Avoid editing the same files while a command runs.
Nonzero command exits still publish valid changes; cancellation discards
unpublished changes. Unsupported entries (devices, sockets, FIFOs), directory
type transitions, and symlinks into private execution paths block publication.
Publication does not reproduce ownership, ACLs, arbitrary xattrs or hard-link
identity. Each root is staged before execution and content-hashed afterward,
without a default size/count cutoff. Only changed regular files receive a second,
trusted capture; unchanged trees need no further host scan. On macOS, eligible
files use APFS copy-on-write clones when supported, including trusted capture;
other files and Linux use ordinary copies. Clones have separate inodes and may
retain source extended attributes, but are never directly promoted into the live
workspace. Preparation and hashing still cost time on large projects. The shell
timeout covers execution, not preparation or publication; caller cancellation
applies throughout.

This boundary assumes a trusted host that does not move authorized roots,
insert mount aliases, or inject host inodes into private execution directories.
Replacing an inside hard link can still change shared link-count/ctime metadata.
It is not remote, network or availability protection: IP networking remains
enabled, and host services must not be used as write proxies. Background jobs
are unsupported; process cleanup is best-effort on macOS. Even a surviving
descendant cannot modify published files through retained execution descriptors.

### Large workspaces

Normal shell calls stage the **whole workspace and active scratch**, including
hidden files, nested repositories, dependencies and build outputs. `workdir`
changes where a command starts, not what gets staged. There is **no default byte
or entry cap**: crossing 8 GiB or 250,000 entries does not disable execution.
Available disk space, memory, filesystem permissions and cancellation still
apply; this does not make physical resources unlimited.

To impose an optional resource budget for this project, merge this section into
`.arkex/config.json` (or use `~/.arkex/config.json` for a global budget):

```json
{
  "sandbox": {
    "maxBytes": 17179869184,
    "maxEntries": 500000
  }
}
```

This example caps each root at 16 GiB and 500,000 entries. To impose that cap
for one launch instead:

```sh
ARKEX_SANDBOX_MAX_BYTES=17179869184 ARKEX_SANDBOX_MAX_ENTRIES=500000 arkex
```

Values must be positive decimal integers. Precedence for each value is
environment → project → global → no cap. To remove a cap, unset its environment
variable and remove the relevant config settings; zero is not a config value for
unlimited execution. Optional budgets count logical regular-file bytes (even for
sparse/cloned files) and all entries, including each root, independently. They
apply to preparation, output capture and publication preflight, not filesystem
permissions. The runtime reads them at setup, not from shell exports in a model's
command. Budget errors identify the effective source and limit, the measured
total, the entry that crossed the cap, and the largest files/top-level
directories. The extra inventory is bounded to five seconds, one million entries
and depth 64; incomplete totals are labeled as lower bounds. Calls that prepare
a tree at 90% of either cap report a warning.
Allow disk space and time for execution and trusted capture; removing a cap does
not create more disk space. Storage/quota failures report a diagnostic hint and
do not trigger unrelated cleanup or unrestricted execution.

macOS attempts per-file cloning from the opened source descriptor into a new
private inode. Source and clone do not share writable file contents. The baseline
is hashed from the private clone, and capture clones are hash-checked before
publication. Files with BSD flags (such as immutable/append-only) use ordinary
copying so staging stays editable and cleanable without changing source flags.
Unsupported cloning or different source/destination volumes also fall back to
ordinary copying; permissions, I/O and storage failures remain errors. Each call
still owns a fresh tree; no persistent execution cache is introduced.

When staging fails, the agent can explicitly call `bash` with
`"read_only": true` to run `ls`, `du`, `git status`, or other diagnostics without
copying either root. All original files remain read-only, including scratch;
`$TMPDIR` points to a fresh disposable directory. Its writes are discarded and
never published. `GIT_OPTIONAL_LOCKS=0` suppresses optional Git index writes.
The same OS confinement and host-IPC restrictions remain enforced; **network
effects are not read-only**. This is not an automatic fallback for writable
commands and cannot use the managed-browser option. Missing confinement still
blocks execution. Package inspection/search/info also avoid workspace staging
and use disposable query caches; host installation restrictions are unchanged.

Independent `ls` and `grep` tools need neither a shell nor staging. They include
hidden/generated files, do not follow symlinks, and report traversal/output
truncation. `ls` can list recursively and filter with a Go filepath glob; `grep`
uses a Go regular expression and optional file glob, returns matching line
numbers, skips binary/files over 1 MiB, and searches at most 32 MiB of file data.
`**` is not a recursive glob. Narrow the path/glob when a bound is reached.
Reads outside the workspace remain permitted. `remove` and `move` work only in
the workspace or active scratch, honor configured tool denies, and must respect
user authorization for destructive actions. Recursive deletion requires an
explicit flag; move never replaces an existing destination or crosses write
roots. Neither can remove/move a write root or follow symlink parents.

There are no ignore rules or persistent writable staging caches. Unrelated WIP
must not be deleted to work around a failure. The confined recovery path and
optional budgets preserve the existing private-copy write boundary.

### Dependencies and browser automation

The structured `packages` tool lets the model inspect the OS and installed
supported managers, search packages, read version information, and install
necessary prerequisites. The model chooses based on project requirements and
existing installations; there is no Node-specific rule or required project file.
Project-local dependencies still use the project's manager through sandboxed shell
commands. npm/npx use a session-scratch cache automatically; `HOME` and browser
discovery settings remain intact so existing installations are still readable.
For missing test resources, direct the tool's downloads into scratch and repeat
the same location override when running it (for example,
`PLAYWRIGHT_BROWSERS_PATH="$TMPDIR/browsers"` for both browser installation and
Playwright tests). Shell exports do not persist between calls; scratch does not
survive a session restart. A missing browser is not itself a permission denial.
Host Unix sockets such as Docker remain blocked; macOS permits only the system
DNS resolver socket as a host-socket exception.

On **macOS**, browser automation uses the bash tool's optional
`"browser": "chromium"` field. Arkex starts a verified stock headless Chromium
for that call and supplies `ARKEX_BROWSER_WS_ENDPOINT`. The script attaches with
`chromium.connectOverCDP(process.env.ARKEX_BROWSER_WS_ENDPOINT, {isLocal:true})`;
ordinary `chromium.launch()` remains blocked. Install the project's Playwright
client normally, but no separate browser installation is needed for this mode.
This is CDP attachment, not full Playwright launch compatibility, and does not
support custom binaries, launch flags, Firefox/WebKit, or browser reuse across
calls. Run a local test server and its browser script in the same call. Linux
continues to use ordinary Playwright launch without the `browser` option.

The first managed call downloads about 100 MB from Google's Chrome-for-Testing
distribution into session scratch. Arkex checks a pinned SHA-256 before each
use and extracts the entire stock distribution outside tool-writable paths.
The browser receives only its own PID-specific Mach rendezvous permission, a
clean environment, and permission to execute only that stock distribution.
Ordinary shell processes receive no additional IPC permissions. The verified
browser is trusted code for its IPC behavior; this is not a security guarantee
against vulnerabilities in Chromium itself. Chromium runs under Arkex's Seatbelt
profile with its own nested sandbox disabled, as in Playwright's default launch.
Browser profiles are disposable, and browser shutdown precedes file publication.
Cancellation discards unpublished changes, as with other shell calls. No
unrestricted-launch fallback exists.

Supported host-manager operations are:

| Manager | Search and information | Installation |
| --- | --- | --- |
| Homebrew | Formula search; stable and installed versions | Official core formula bottles and missing runtime dependencies |
| apt | Cached name search; installed/candidate versions and sources | Not supported: administrator access is not granted |
| dnf | Cached search and package information, plugins disabled | Not supported: administrator access is not granted |

Discovery trusts only standard executable locations: `/opt/homebrew/bin/brew`
or `/usr/local/bin/brew` on macOS, `/home/linuxbrew/.linuxbrew/bin/brew` on Linux,
and `/usr/bin/apt-cache` or `/usr/bin/dnf` on Linux. It does not trust a manager
found through project-controlled `PATH` or allow custom executable paths.
Queries run inside the ordinary sandbox with scratch-local caches.

Only the fixed Homebrew installation sequence runs outside that sandbox. It
checks installed formulas first and accepts an optional exact version, not a
version range; unavailable versions and automatic upgrades/downgrades are rejected.
Use package information to choose a canonical or versioned formula name. The
tool does not use `sudo`, bootstrap managers, accept arbitrary commands/flags,
add repositories or installer URLs, install casks, build from source, or uninstall.
It rejects system/prefix `brew.env` files that could override its fixed settings.
Missing or incompatible bottles stop the operation rather than relaxing its
limits; earlier dependencies may remain installed after a failure. Installation
persists in the host Homebrew prefix and trusts the manager and official package
installation scripts. The tool verifies package-manager records; the model must
then check executable availability and project compatibility inside the sandbox.

Use `/model` to switch models, `/resume` for saved sessions, `/new` for a fresh
conversation, and `/help` for commands. Press Esc twice to stop a run or Ctrl+C
twice to exit when idle. Model output is untrusted: review changes before shipping.

To edit a sent prompt, select it with Tab/Shift+Tab from an empty input and press
Enter, or click the prompt. Edit and confirm to resend in a new conversation
branch; the original stays in `/resume`. Esc cancels editing. File changes are
not undone. The command palette's **Edit sent prompt** also includes prompts
saved before context compaction.

Choose **Remove from here…** from a sent prompt's menu (or press Delete while
it is selected) to create a branch without that prompt and everything after it.
Confirmation is required; the original conversation and file changes remain.

### Context compaction

Arkex compacts automatically near the context limit; `/compact` requests it
manually. `ctx*` means the model's limit is assumed, not confirmed. The trigger
estimates the upcoming request, including instructions, tool definitions, new
messages and output headroom. Token estimates are not exact.

Compaction saves the original context first, summarizes older text in bounded
chronological chunks, and retains a bounded recent suffix plus the fresh user
prompt. Tool calls and their results stay together. Attachments remain attached
verbatim rather than being converted to base64 text for summarization.
The handoff includes a readable original-history reference so the agent can
recover omitted details; summaries themselves are not guaranteed lossless.

The footer shows the observed phase, request number and elapsed time. Each
summary request has a 90-second deadline; the whole operation has a five-minute
deadline and a 32-request cap, including at most one smaller-chunk overflow
retry. Esc twice cancels. Incomplete, oversized or unusable summaries and failed
saves leave the original context active. An automatic failure stops the run
instead of silently sending the oversized request again. Retry `/compact` or
switch models after addressing the reported error.

Private history snapshots live under the workspace's session directory in
`context-history/<session-id>/`. Their references survive resume and repeated
compaction. Snapshots are retained separately, including after deleting a
session file, because edited branches can still refer to them. One-shot print
runs normally remain unsaved, but create a recoverable session if compaction
is needed. Recovery through tools still respects configured read permissions.

## Build from source

Use the Go version in [go.mod](go.mod):

```sh
go build ./cmd/arkex
go test ./...
```

CI tests Linux and macOS. See [RELEASING.md](RELEASING.md) for signed
release builds and [LICENSE](LICENSE) for the MIT license.
