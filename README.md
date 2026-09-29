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
Ownership, ACLs, arbitrary xattrs and hard-link identity are not reproduced.
Each root is copied and scanned per command, with limits of 8 GiB and 250,000
entries; this costs disk space and time on large projects.

This boundary assumes a trusted host that does not move authorized roots,
insert mount aliases, or inject host inodes into private execution directories.
Replacing an inside hard link can still change shared link-count/ctime metadata.
It is not remote, network or availability protection: IP networking remains
enabled, and host services must not be used as write proxies. Background jobs
are unsupported; process cleanup is best-effort on macOS. Even a surviving
descendant cannot modify published files through retained execution descriptors.

The structured `packages` tool lets the model inspect the OS and installed
supported managers, search packages, read version information, and install
necessary prerequisites. The model chooses based on project requirements and
existing installations; there is no Node-specific rule or required project file.
Project-local dependencies still use the project's manager through sandboxed shell
commands. Supported host-manager operations are:

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

## Build from source

Use the Go version in [go.mod](go.mod):

```sh
go build ./cmd/arkex
go test ./...
```

CI tests Linux and macOS. See [RELEASING.md](RELEASING.md) for signed
release builds and [LICENSE](LICENSE) for the MIT license.
