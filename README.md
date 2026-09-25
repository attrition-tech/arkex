# arkex

A terminal coding agent for your own models. Read, edit, and run code with
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

**Windows:** download the x64 ZIP from [Releases](https://github.com/attrition-tech/arkex/releases/latest),
extract it, and add its folder to `PATH`. Shell tools require Git Bash on `PATH`.

Supported: macOS Apple Silicon/Intel, Linux ARM64/x64, Windows x64.
Update with `arkex update`.

## Configure

Run `arkex` in your project, open `/connections`, and add your model server,
API connection, or ChatGPT account. Choose a model and start chatting.

Configuration and sessions stay in `~/.arkex/`. API keys can reference environment
variables instead of being stored directly, for example `"apiKey": "$MY_API_KEY"`.
Prompts and relevant project content are sent to the model provider you choose.

For terminal sound/visual alerts, add `"ui": {"bell": true}` to your config.
Alerts fire when approval or retry input is needed, and when a run completes,
fails, or pauses—not when you cancel. Off by default; your terminal controls
the sound and may need its bell enabled.

## Use

```sh
arkex                                 # interactive session
arkex -c                              # resume the latest session here
arkex -p "explain this project"        # one-shot answer
arkex --mode plan -p "plan the change" # investigate without editing
```

- **Build** (default): reads are allowed; edits and shell commands ask by default.
- **Plan:** read-only tools for investigation and planning.
- **Auto:** skips configured asks inside the workspace or trusted directories;
  explicit denies still apply. Permission checks are not an OS sandbox.

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

CI tests Linux, macOS, and Windows. See [RELEASING.md](RELEASING.md) for signed
release builds and [LICENSE](LICENSE) for the MIT license.
