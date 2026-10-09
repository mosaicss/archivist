# Archivist CLI

Mosaic's command line surface for filings research. Any AI agent (Claude Code, Cursor, custom orchestrators) or shell context (bash, cron, CI) can search and read SEC and SEDAR filings with it. Every passage it returns carries a permalink that opens the passage in Mosaic's filing viewer, so an answer can cite its source.

The CLI returns passages, not answers: Mosaic runs no model for it. Chat and tables run only in the Mosaic web app.

## Install

### One-step install script (macOS, Linux, Windows)

```sh
curl -fsSL https://github.com/mosaicss/archivist/releases/latest/download/install.sh | bash
```

```powershell
& ([scriptblock]::Create((irm https://github.com/mosaicss/archivist/releases/latest/download/install.ps1)))
```

The script installs or updates archivist (`~/.local/bin`, or
`%LOCALAPPDATA%\Programs\archivist` on Windows) after checking the archive
against the release's `SHA256SUMS`, and adds the Claude Code skill when
`~/.claude/skills` exists. A Homebrew install is upgraded with `brew upgrade`
instead (only while the cask is still installed). With a pairing code from the Mosaic workspace it also connects this
machine; see [One-step connect](#one-step-connect).

### Homebrew (macOS)

```sh
brew install mosaic-finance-inc/tap/archivist
```

Installs the universal macOS binary as a Homebrew cask and places the Claude
Code skill in `~/.claude/skills/archivist/`.

### GitHub Releases (macOS, Linux, Windows)

Download the archive for your platform and the checksum file from
https://github.com/mosaicss/archivist/releases, verify, and put the binary on
your PATH. For example, Linux amd64:

```sh
VER=0.2.23
curl -fsSLO https://github.com/mosaicss/archivist/releases/download/v${VER}/archivist_v${VER}_linux_amd64.tar.gz
curl -fsSLO https://github.com/mosaicss/archivist/releases/download/v${VER}/archivist_v${VER}_SHA256SUMS
grep "archivist_v${VER}_linux_amd64.tar.gz" archivist_v${VER}_SHA256SUMS | sha256sum -c -
tar -xzf archivist_v${VER}_linux_amd64.tar.gz archivist
install -m 0755 archivist ~/.local/bin/archivist
```

Then add the Claude Code skill:

```sh
archivist update --skill
```

### go install

```sh
go install github.com/mosaicss/archivist/cmd/archivist@latest
archivist update --skill   # optional: the Claude Code skill
```

## Authenticating

Agent access is part of a Mosaic Pro account (plans:
https://mosaic-finance.com/en/pricing/). Signed in on Mosaic, open your avatar
menu, then Manage account, then API keys, and add a key. Copy the `ak_...`
token and save it once:

```sh
archivist auth login --token ak_<your-token>
```

The CLI verifies the token against the server and writes it to
`~/.archivist/credentials` (mode 0600). `archivist auth status` shows which
credential is active and where it came from; `archivist auth logout` deletes
the saved file.

For CI and scripting, `ARCHIVIST_TOKEN=ak_...` overrides the saved file, and
`--token ak_...` overrides both for one call.

## Quickstart

```sh
# 1. Find a company's symbol
archivist companies search "Shopify"

# 2. Search its filings
archivist search "revenue growth drivers" --symbol SHOP --formtype 10-K

# 3. Read around a hit (ids come from the search results)
archivist read passage <chunk_id> --window 2
archivist toc <filing_id>
archivist read section <filing_id> "Item 7. Management's Discussion and Analysis"

# 4. List a company's filings, or check whether one filing mentions a term
archivist filings NVDA --formtype 10-K --limit 3
archivist find <filing_id> "TSMC"
```

On a terminal, `search` prints a table:

```text
CHUNK_ID  FILING_ID  SYMBOL   FORM  DATE        SECTION                SNIPPET                               URL
5f0c…     a9d2…      SHOP     10-K  2026-02-12  Item 7. Management's…  Revenue grew 26% driven by growth in…  https://mosaic-finance.com/filings/a9d2…/?c=5f0c…&t=…
```

Piped or redirected, every verb prints JSON instead: the server response,
indented, keys and values unchanged. Pass `--format json` or `--format table`
to choose. Diagnostics go to stderr, so stdout stays clean for `jq`.

`--format compact` (research verbs and `companies`) prints one line of
minified JSON for an agent: no nulls, no `truncated: false`, no `chunk_index`
or `exchange` on passages, no `formdescription` beside a US listing's form
code such as 10-K (Canadian and Turkish rows keep it), and no
`entity_resolution` when the symbol resolved plainly. Ids, `url`, `cite_as`
and exchange document ids stay, in the server's order. `search`,
`read passage` (its neighbours; the passage itself stays on every page),
`read section`, `toc`, `filings` and `find` also fit each page to 24,000 bytes
as a host prints it (about 6k tokens): a cut page says `truncated: true` and
carries a `c1.` cursor that continues the same request with `--format
compact` (other arguments or another format exit 2).

When a response is too large it is truncated and stderr says
`More results: rerun with --cursor <token>`.

## Verbs

| Verb | What it does |
|------|--------------|
| `search <query>` | Search passages. `--symbol`, `--formtype`, `--date-from`, `--date-to`, `--latest-only`, `--mode semantic\|broad`, `--limit 1-25`, `--cursor` |
| `read passage <chunk_id>` | A passage with up to `--window` (0 to 2, default 1) neighbours on each side |
| `read section <filing_id> <section_header>` | Every passage of one section, in order |
| `toc <filing_id>` | A filing's section headers |
| `filings <symbol>` | A company's filings, newest first. `--formtype`, `--date-from`, `--date-to`, `--limit 1-25` (default 10), `--cursor`; an empty list exits 0 |
| `find <filing_id> <term>` | Whether one filing mentions a term: `found`, `total_matches`, the best matches. `--limit 1-10` (default 5), `--cursor`; quote the term for an exact phrase; `found: false` exits 0 |
| `companies search <query>` | Find a company and its symbol |
| `companies get <issuer_key>` | One company's details |
| `auth login\|status\|whoami\|logout` | Manage the credential |
| `usage` | This month's fair use count and the rate limit |
| `doctor` | Check credential, connectivity, version and skill |
| `update` | Replace the binary with the latest release (`--skill`, `--check`) |
| `connect` | Drive your own Claude Code or Codex from the Mosaic workspace (preview, see below) |
| `version` | Print version, commit, build date and platform |

```text
$ archivist version
archivist-cli 0.2.23 (commit abc1234 built 2026-10-01) linux/amd64
```

## Fair use and errors

Every research call counts toward a monthly fair use limit. When it is
reached, calls exit 7 and stderr names the reset date; `archivist usage` shows
the count. A free account exits 4 with the plans page.

With `--format json`, a failure also prints
`{"error": <code>, "message", "exit_code"}` on stdout, plus `suggestion`,
`account_url` or `reset_date` when the server sent them.

## Exit codes

Agents should branch on exit codes, not parse output text:

| Code | Meaning |
| ---- | ------- |
| 0    | success |
| 1    | generic error |
| 2    | usage error (bad flag, id or argument; unknown command) |
| 3    | not found (no passages; unknown id or company) |
| 4    | auth error (missing or invalid credential; no Pro account) |
| 5    | server error (5xx after retries, a search 504 or timeout without one; network failure; minimum CLI version block) |
| 6    | ambiguous match (a search symbol matched several issuers) |
| 7    | rate limit or monthly fair use limit reached |
| 8    | reserved, not emitted |
| 9    | not implemented (do not retry) |

An old binary exits 5 with "Run 'archivist update' to upgrade."

## MCP server

`archivist mcp serve` runs the binary as an MCP server over stdio. Each
research verb becomes a read only MCP tool with the same credential and exit
codes, so any MCP host (Claude Desktop, Cursor, custom agents) can search
filings without shell access.

Claude Desktop config (`claude_desktop_config.json`):

```json
{
  "mcpServers": {
    "archivist": {
      "command": "archivist",
      "args": ["mcp", "serve"],
      "env": { "ARCHIVIST_TOKEN": "ak_..." }
    }
  }
}
```

The `env` block is optional once `archivist auth login` has saved a
credential.

Tools (8): `search`, `read_passage`, `read_section`, `toc`, `filings`,
`find`, `companies_search`, `companies_get`. Tool names join the verb path
with underscores. Every tool has a title and the annotations
`readOnlyHint: true`, `destructiveHint: false`, `openWorldHint: false`. Every
result is the `--format compact` output (the tools take no `format` or
`dry_run` argument), so a page stays well under a host's tool output limit.

`auth login`, `auth logout`, `update` and `connect` are not exposed: token
setup, binary replacement and harness supervision are operator actions.
`auth status`, `auth whoami`, `doctor`, `usage` and `version` are shell verbs
only, so agents do not pay for their descriptions. A failed call returns an
error result naming the exit code, with the verb's stderr and stdout.

`--token-file <path>` reads the token from a file on every tool call, so a
supervisor can rotate it without restarting the server (it cannot be combined
with `--token`).

**Task mode.** When the token is a session task token (`mst_...`, minted by
`archivist connect` for one workspace session), the server exposes only the
tools whose chat-api routes a task token may call: `search`,
`companies_search`, `read_passage`, `read_section`, `toc`, `filings` and
`find`. `companies_get` falls back to the full company catalog, which task
tokens cannot read, so it is left out. Task mode results stay the full
`--format json` body: the Mosaic workspace reads those records. `auth login` refuses task tokens. With `--publish-session <id>`
and `--publish-dir <dir>` (set by `archivist connect` when the token carries
the `publish` scope) task mode adds `publish_artifact`, which uploads one file
from that directory to the session's workspace (see below).

## Connect your Claude Code or Codex (preview)

`archivist connect` lets the Mosaic workspace drive the Claude Code or Codex
installed on your machine. It keeps one outbound WebSocket to the Mosaic
agent relay; nothing listens on your machine.

### One-step connect

The workspace shows a single use pairing code (valid for 10 minutes) and a
command that includes it. Paste the command into a terminal, or ask your own
agent to run it:

```sh
curl -fsSL https://github.com/mosaicss/archivist/releases/latest/download/install.sh | bash -s -- --pair ABCDE-FGHJK
```

It installs or updates archivist, then runs:

```sh
archivist connect --pair ABCDE-FGHJK   # redeem the code for a new ak_ key, saved to ~/.archivist/credentials
archivist connect --install            # run connect as a background user service
archivist connect --status             # report the service, its ceiling and the saved key
```

`--pair` accepts the code in any case, with or without the hyphen or spaces,
and never prints the key (only its masked form). An unknown, used or expired
code exits 4: get a new code in Mosaic. The redeem request is sent once and
never retried. If `ARCHIVIST_TOKEN` is set it still takes precedence in that
terminal, and `--pair` warns about it. Pairing again replaces the saved key;
the earlier key stays active, and `--pair` says so (masked) so you can
revoke it in Mosaic. A `~/.archivist/credentials` symlink (dotfiles) is kept:
the key is written to the file it points to, which is created when it does
not exist yet.

`--install` writes a launchd agent on macOS
(`~/Library/LaunchAgents/com.mosaic-finance.archivist.connect.plist`, loaded
into `gui/<uid>`, started at login) or a systemd user unit on Linux
(`~/.config/systemd/user/archivist-connect.service`, enabled and started,
with `loginctl --no-ask-password enable-linger` attempted so it also starts
at boot). The
service runs `archivist connect --service` with only `PATH`, `HOME`, `LANG`,
`LC_ALL`, an absolute `CODEX_HOME` or `CLAUDE_CONFIG_DIR`, and
`ARCHIVIST_BASE_URL`/`ARCHIVIST_RELAY_URL` when set, captured at install
time; it reads the saved key only (`ARCHIVIST_TOKEN` and `--token` are
ignored, even when injected into the service environment). It logs to
`~/.archivist/connect/connect.log` (0600; at service start a log over 10 MiB
is moved to `connect.log.1`) and writes its process id to
`~/.archivist/connect/service.pid` while it runs. A crash restarts it after
10 seconds; a clean stop (another `archivist connect` took over, the key was
revoked, the feature is off, no usable Claude Code or Codex) leaves it
stopped until `archivist connect --install`, the next login (macOS, Linux
without lingering) or the next boot. Running `--install` again restarts it
on the current binary. The install script does that on every update;
after `archivist update` or `brew upgrade`, run `archivist connect --install`
yourself so the service runs the new binary (`archivist update` reminds you
when a service is installed). `archivist connect --uninstall`
stops and removes it and keeps the saved key.

`archivist connect --install --max-permission <mode>` sets the service's
permission ceiling (see Session controls below): the service then runs
`archivist connect --service --max-permission <mode>`. Without the flag a
first install runs the default ceiling (`auto_edits`) and writes no flag,
and a reinstall keeps the ceiling the installed service already has, so the
install script's reinstall on every update never changes it; give
`--max-permission auto_edits` to return to the default. `--status` shows the
installed ceiling. `--mode`, `--model` and `--effort` are refused with
`--install` (they need `--session`). A service whose `--max-permission` the
running binary does not know (after a downgrade, or a hand edit) logs the
reason and stops cleanly instead of restarting every 10 seconds. An
archivist older than the ceiling flag refuses it as an unknown flag and is
restarted every 10 seconds until the service is installed again. After a
downgrade, run `archivist connect --install` so the service definition is
written by the binary it runs (a ceiling it does not know falls back to the
default).

`--status` exits 0 when the service is running (Linux: the unit is active;
macOS: the job is loaded and the daemon in `service.pid` is alive; Windows:
the task is registered and the daemon in `service.pid` is alive) and a key
is saved, else 1, and prints the ceiling and the last log line. A loaded launchd job whose
daemon has stopped shows "loaded, not running"; a job loaded a moment ago gets
about 5 seconds to write its pid first. `--pair`, `--install`,
`--uninstall` and `--status` are used one at a time.

#### Windows

In PowerShell (designed to need no admin rights; nothing asks for elevation):

```powershell
& ([scriptblock]::Create((irm https://github.com/mosaicss/archivist/releases/latest/download/install.ps1))) -Pair ABCDE-FGHJK
```

`install.ps1` installs or updates `archivist.exe` and `archivistw.exe` in
`%LOCALAPPDATA%\Programs\archivist` (a running exe is renamed aside, never
overwritten), adds that folder to your user PATH, then pairs, installs and
reports background connect like `install.sh`. Options: `-Pair CODE`,
`-NoService`; `ARCHIVIST_INSTALL_DIR`, `ARCHIVIST_INSTALL_VERSION` and
`ARCHIVIST_RELEASE_BASE_URL` work as for `install.sh`.

On Windows `--install` registers a scheduled task
`MosaicArchivistConnect-<user>-<hash>` (the hash, 8 hex digits derived from
your account's SID, survives renaming the PC or the account and keeps two
users with similar names apart) that starts at your login (logon trigger
and principal for your user only, least privilege, no time limit, runs on
battery). It runs `archivistw.exe connect --service`, the same program
built without a console window, which supervises the daemon: an unexpected
exit restarts it after 10 seconds, a clean stop leaves it stopped. The task
definition is kept in `~\.archivist\connect\MosaicArchivistConnect.xml` and
the captured environment in `~\.archivist\connect\service.env`; `--status`
reads the task's registration and the daemon's `service.pid`. Ending the task
or signing out ends every session's process tree (each Claude Code or Codex
session runs in its own Windows job object). Claude Code and Codex must be
the native executables: an npm install is used through its native
`claude.exe` or `codex.exe`, never through the `.cmd` script; Codex sessions
run in Codex's unelevated Windows sandbox. archivist is not code signed yet,
so SmartScreen or Smart App Control may warn about it or block it.

Install script options: `--pair CODE`, `--no-service` (pair only).
`ARCHIVIST_INSTALL_DIR` changes the install directory,
`ARCHIVIST_INSTALL_VERSION` installs a given tag, and
`ARCHIVIST_RELEASE_BASE_URL` points the script at another release host (for
testing). Each released script is stamped with its own tag.

### Running it yourself

```bash
archivist connect --check   # what was detected; connects nothing
archivist connect           # serve workspace sessions until Ctrl-C
archivist connect --claude-model claude-sonnet-5 --claude-effort low
archivist connect --codex-model gpt-6-luna --codex-effort low
archivist connect --max-permission full_auto   # allow Full auto sessions here
archivist connect --web-search  # sessions here may search the web (provider side)
```

Requirements: an `ak_` API key (`archivist auth login`) on a Pro account, and
at least one of: Claude Code 2.1.280 or newer logged in with a claude.ai
subscription (`claude auth status` shows `authMethod: claude.ai`), or Codex
0.160.0 or newer logged in with ChatGPT (`codex login status`). API key logins
are refused. Newer versions are reported, not blocked; a harness that is not
usable when `archivist connect` starts is reported as unavailable and its
sessions are refused (restart after logging in).

### Permission modes, model and effort

Each session runs in one of four permission modes, chosen in the workspace
when it starts and changeable while it runs. Low to high:

| Mode | Label | What it means | Claude Code | Codex |
|---|---|---|---|---|
| `read_only` | Read only | Reads and research run; anything that would change something is denied without asking | `--permission-mode plan` | `never`, `read-only` sandbox |
| `ask` | Ask every time | Every call that needs permission is a card | `--permission-mode default` | `untrusted`, `workspace-write` sandbox |
| `auto_edits` | Auto edits | File edits run (Claude Code also runs simple file commands such as mkdir, mv, cp and rm inside the session folder); other commands ask | `--permission-mode acceptEdits` | `untrusted`, `workspace-write` sandbox; the daemon accepts file changes inside the session folder |
| `full_auto` | Full auto | The harness runs without asking, but anything it still asks about (for example deleting a critical folder) is shown as a card | `--permission-mode bypassPermissions` | `never`, `danger-full-access` (no sandbox, network on) |

The Mosaic search and read tools (`search`, `companies_search`,
`companies_get`, `read_passage`, `read_section`, `toc`, `filings`, `find`) never ask, in any
mode and on both harnesses. `publish_artifact` follows the mode. The daemon
enforces the mode itself on every approval request that reaches it,
whatever the harness version does: Mosaic read tools are allowed, `read_only`
denies everything else without a card, and every other mode shows a card,
`full_auto` included (Claude Code keeps some prompts even in bypass
permissions, such as removing a critical directory). In `auto_edits` the
daemon accepts a Codex file change without a card only when it asks for no
extra root and every path, resolved through symbolic links, is inside the
session folder; Codex commands always ask. These rules come before answers
remembered for the session ("allow for session" does not let a `read_only`
session change anything). Every Claude Code launch also passes
`--settings '{"useAutoModeDuringPlan":false}'`, so plan mode never lets the
auto mode classifier approve shell commands.

`--max-permission <mode>` is the ceiling on this machine: no session runs
above it. The default is `auto_edits` (`full_auto` for `connect --session`,
where the Mosaic cloud sandbox is the isolation). A session that asks for
more starts at the ceiling, a change above it is refused, and the workspace
can never raise it. A session started without a mode runs `ask` (`full_auto`
with `--session`, or `--mode <mode>`), within the ceiling. Only a
`full_auto` ceiling passes Claude Code `--allow-dangerously-skip-permissions`
(needed to switch to bypass permissions later); Claude Code refuses bypass
permissions as root outside a recognised sandbox, and the session then
fails rather than running in a lower mode. `--dangerously-skip-permissions`,
`--bare` and the `auto` and `dontAsk` modes are never used.

### Web search

`--web-search` turns on each harness's own web search: Claude Code's
`WebSearch` tool (it runs at Anthropic) and Codex's `web_search` set to
`live` (it runs in the OpenAI Responses backend). The search runs at the
provider, never on this machine, and the agent's guidance then says to use
the Mosaic tools for filings and company facts and web search only for news,
market prices, macro data and events after the latest filing. It is off by
default for `archivist connect` and on by default for `connect --session`
(the Mosaic cloud sandbox), where `--web-search=false` turns it off. It is a
machine setting: the relay and the workspace never choose it. Background
connect (`--install`, `--service`) does not take `--web-search` yet (refused
with a message; run `archivist connect --web-search` in the foreground).

A web search is a read: like the Mosaic read tools it never asks, in any
mode (`read_only` included) and on both harnesses. Claude Code gets
`WebSearch` in `--tools` and pre-allowed in `--allowedTools`, and the
daemon's own check allows it; with web search off a `WebSearch` request is
decided by the mode like any other tool. Codex never asks for a search.
Fetching pages stays off either way: Claude Code's `WebFetch` is never in
`--tools`, every Claude launch sets `CLAUDE_CODE_DISABLE_WEB_FETCH=1`, and a
session whose Claude Code reports `WebFetch` (or `WebSearch` with web search
off) fails; Codex's standalone page fetch feature is never set. The
workspace removes links to filing source sites (regulators, exchanges and
filing vendors) from agent answers and shows each search as a "Searching
the web" card with its query.

A mode change applies to Claude Code once Claude confirms it
(`set_permission_mode`; the daemon's own checks switch then too) and to Codex
at the next turn (every `turn/start` carries the current policy and sandbox;
the daemon's own checks switch at once). A Codex turn already running keeps the
mode it started with until it ends: lowering a session from `full_auto` in
the middle of a turn leaves that turn with full access until your next
message. Stop the turn to apply a lower mode at once. Lowering a session to
`read_only` denies its open approval cards; other changes leave open cards
as they are.
A paused session, or one whose Claude Code stopped before confirming, takes
the new mode when it resumes. The mode,
model and effort are kept with the session, so a resumed session keeps them,
lowered to the current ceiling.

After connecting, the daemon reports a `controls` frame next to its
capabilities: the ceiling, the default mode, and the models and efforts a
session may pick. For Claude Code these are the aliases `default`, `opus`,
`sonnet`, `haiku` and `fable` (Haiku has no effort levels); `best`,
`opusplan` and the `[1m]` variants are not offered (Opus 5.5 and Sonnet 5.x
already have a 1M context window); for Codex, the models
`codex app-server` lists (`model/list`, hidden ones left out), probed once at
startup in a throwaway Codex home. A session's model and effort override
`--claude-model`/`--claude-effort` or `--codex-model`/`--codex-effort`; a
start naming a model or effort outside that list fails with a status naming
it. An older relay refuses the `controls` frame; sessions then run with the
machine flags and the default mode. The relay accepts a `controls` frame of
at most 6144 bytes; a larger Codex list is cut from the end (its default
model kept) and the log says how many models were left out. A typical
report is about 2 KB.

The session-bound mode takes the same choices as flags: `connect --session
<uuid> --agent claude|codex --prompt-file <path> [--mode <mode>] [--model
<id>] [--effort <id>]`. `--mode`, `--model` and `--effort` need `--session`;
the model and effort are checked against the same list when the session
starts, and one this machine does not offer refuses the start: connect
exits 1 and the sandbox task fails. Web search is on there unless
`--web-search=false` is given.

For each "My Claude Code" session the daemon:

- runs `claude -p` in stream-json mode in a fresh temporary directory, with
  only `HOME`, `PATH`, `USER`, `LOGNAME`, `SHELL`, `LANG`, `TERM`, `TMPDIR`,
  `LC_*` and `XDG_*` in its environment (provider keys and `CLAUDE_*`,
  `ARCHIVIST_*` variables never pass, except an absolute `CLAUDE_CONFIG_DIR`
  naming an existing directory, so a login kept outside `~/.claude` is found;
  the daemon itself adds `CLAUDE_CODE_DISABLE_WEB_FETCH=1`)
  and without your user, project or local Claude settings;
- refuses the session unless Claude reports a subscription login
  (`apiKeySource: none`) and the permission mode of the session's mode (in
  the session-bound sandbox mode any built in sign in method is accepted;
  see "Sign in" below);
- gives Claude the built-in tools Bash, Read, Edit, Write, Glob and Grep
  (plus `WebSearch`, pre-allowed, with web search on; never `WebFetch`) and
  the Mosaic search and read tools (`archivist mcp serve` in task mode, under
  a 15 minute task token it rotates and revokes); a tool call the session's
  mode leaves to you becomes an approval card in the workspace (allow once,
  allow for session, deny once, deny for session; no answer within 60
  seconds denies).
  "Allow for session" and "deny for session" apply to every later call of that
  tool in the session, whatever its input, without another card; they live in
  the daemon's memory, so restarting `archivist connect` clears them;
- streams the session as `mosaic-event/1` events, each validated before it is
  sent, with three exceptions stamped per event type: the session-bound
  mode's sign-in prompt, a `mosaic-event/2` `data-auth-prompt` that adds the
  Codex device code and the sign-in expiry; and, as `mosaic-event/3`, a
  `data-usage` that carries `contextTokens` (the context in use after the
  turn's last model call) or `contextWindow` (the model's window), each only
  when the harness reported it, never guessed (a usage report without them
  stays `mosaic-event/1`), and `data-session-controls`
  (`{mode, maxMode, model?, effort?}`: the session's mode, the ceiling, and
  its model and effort when set; for Claude Code the session's choice or
  else the machine flag, for Codex the model and effort its thread started
  with, Codex's default model included), sent when the session starts running and after every
  `set_mode` outcome while it runs, a refused one included (a change on a
  paused or starting session is reported when it runs). The relay must
  accept `mosaic-event/2` and `mosaic-event/3` before a release that sends
  them: an older relay refuses those events, which would drop the session
  controls and every usage report that carries context. Codex applies a
  mode change at its next turn, so while a Codex turn is running the event
  already reports the new mode and that turn keeps its old policy until it
  ends.

For each "My Codex" session the daemon:

- runs one `codex app-server --stdio --strict-config` in a fresh temporary
  directory, with the same environment allowlist plus `CODEX_HOME` set to a
  private per-session home, `~/.archivist/connect/codex-home/<session>/`
  (0700). That home holds only `auth.json` as a link to your login
  (`$CODEX_HOME/auth.json` if `CODEX_HOME` is an absolute path, else
  `~/.codex/auth.json`), never a copy; Codex refreshes the login in place
  through the link. Your Codex config, `AGENTS.md`, rules, hooks, plugins,
  skills, memories, MCP servers, notifications and project trust are not
  loaded and never written; parent `OPENAI_*` and `CODEX_*` variables never
  pass;
- fixes the rest with `-c` overrides: ChatGPT login only, the `openai`
  provider, a `workspace-write` sandbox (`ask`, `auto_edits`; `read_only`
  is `read-only`, and `full_auto` runs `danger-full-access`: no sandbox,
  with network) that can write only the session
  directory (not `/tmp` or `$TMPDIR`, where other sessions' directories
  live; `TMPDIR` points at `<session dir>/.tmp`) with no network, a core
  shell environment for commands, web search off (`web_search="live"` with
  web search on), history off,
  no project root markers, and the archivist MCP server (task mode tools,
  task token as above; Mosaic read tools are approved without asking). The
  approval policy and sandbox (from the session's mode, reviewed by you)
  and model are set per thread and on every turn; `--codex-model` defaults
  to Codex's default model and `--codex-effort` to Codex's default effort;
- refuses the session (error plus a failed status, Codex stopped, nothing
  else sent) unless Codex reports the session home, a ChatGPT account, the
  `openai` provider, the approval policy and sandbox of the session's mode
  reviewed by the user (for `workspace-write`: without network, extra roots,
  `/tmp` or `$TMPDIR`, TMPDIR pointing inside the session directory; for
  `read-only`: without network), the launch's `web_search` value set by its
  `-c` flags (`config/read`), no instruction files, and the archivist MCP
  server ready with no other MCP server. A later
  switch away from the ChatGPT login ends the session the same way;
- turns command, file change and archivist tool approvals into workspace
  cards. Your answer maps one to one onto Codex's choices: allow once is
  accept, allow for session is Codex's accept for session (the identical
  command, or the same file, runs again without asking for the rest of the
  session), deny once (or no answer within 60 seconds) is decline, and deny
  for session is cancel (deny and end the turn); for an archivist tool, allow
  for session uses Codex's session-only persistence. archivist connect never
  sends Codex's rule amendments or "always" approvals, so no approval is
  written to your config;
- interrupts with `turn/interrupt` and then cleans the thread's background
  terminals; after a restart the next message resumes the same thread
  (`thread/resume`) from the kept session home;
- stops every process a session started, including commands Codex detaches
  with `setsid`: the daemon records each session's descendants and, on
  Linux, adopts orphaned ones as child subreaper, then kills and reaps them
  when the session stops, is interrupted past its timeout, crashes or
  Ctrl-C ends the daemon.

Both harnesses also get `publish_artifact`, which publishes one file from the
session directory to the workspace, where it opens beside the conversation.
It is never pre-allowed: in `ask` and `auto_edits` each call asks for
approval through the card (Claude's permission prompt, Codex's
`approval_mode = "prompt"` for that tool) unless you chose "allow for
session" for it; `read_only` denies it, and in `full_auto` the harness
approves it itself (a card if it still asks). It refuses, and
uploads nothing, for a path outside the session directory or containing
`..`, any symbolic link, a file that is not regular, empty or over 10 MiB, and
anything but `.pdf`, `.txt`, `.md`, `.csv` and `.json` whose content matches
(a PDF header; UTF-8 text without NUL; JSON that parses). A successful call
adds a `data-artifact` event to the session stream.

At most four harness processes run at once. A new session or a resume on a
full daemon parks the least recently active idle session (no turn running, no
approval open): its process stops and a `disconnected` status says the next
message resumes it. When every session is busy, the new one is refused.
The session-bound mode (`connect --session`) serves one session and never
parks it.

Stopping connect (Ctrl-C) answers every open approval deny before it stops
the harness (Claude Code: "Denied: archivist connect stopped before an
approval arrived.", Codex: decline).

Sign in (session-bound mode, a Mosaic cloud sandbox):

- A harness that is installed but logged out signs in before the first turn:
  Codex with a ChatGPT device code, Claude Code with `claude auth login
  --claudeai` on a terminal (the link goes out as a `data-auth-prompt`, the
  user sends the code the page shows as the next message). The sign in has 5
  minutes.
- Anthropic's terms for hosting Claude Code forbid removing a built in sign
  in method, so in this mode only Claude Code also runs on a Console login or
  on the user's own API key. `ARCHIVIST_SIGNIN_FILE=<absolute path>` (read
  only with `--session`; a relative path is ignored with a warning) names the
  sign in request file the sandbox Worker writes atomically when the user
  picks another method on the sign in card: `{"id":"<uuid>","method":"console"}`
  or `{"id":"<uuid>","method":"api_key","key":"sk-ant-..."}`. While the Claude
  sign in waits connect reads it about every second and acts once per new
  `id`; an unreadable or invalid request, an unknown method, a malformed key
  or an `id` already acted on is ignored. `console` stops the claude.ai login,
  runs `claude auth login --console` the same way and sends a new prompt (a
  new `promptId`, the Console link) with a fresh 5 minute window from the
  switch, never past 10 minutes from the start of the sign in (the prompt
  names that expiry). A login that already exited or reported success is
  settled before any request. `api_key` stops the login and runs the session in API key mode:
  every Claude Code child (the `claude auth status` proof and each `claude -p`
  launch) gets `ANTHROPIC_API_KEY` set to the file's key, the one
  `ANTHROPIC_*` variable connect ever passes, and only in this mode. The key
  is never logged or put in an error (logs name environment keys only). In
  the Mosaic sandbox the file carries a fixed placeholder that the sandbox's
  egress replaces with the user's key on `api.anthropic.com` only, so the
  real key never enters the container.
- The login proof in this mode accepts each built in method: a claude.ai
  login must still be a claude.ai subscription with init `apiKeySource:
  none`; after a Console login any login `claude auth status` reports as
  logged in passes, and the init frame must report a key source other than
  `none` (the one `claude auth status` named, when it named one); in API key
  mode `claude auth status` and the init frame must both report
  `ANTHROPIC_API_KEY`. A sandbox home that already holds a logged in login other than
  claude.ai (a remembered Console login) starts in Console login mode
  without a sign in. The local daemon keeps its claude.ai only rules.

Idle pause and resume (session-bound mode, a Mosaic cloud sandbox):

- `ARCHIVIST_ACTIVITY_FILE=<absolute path>` (read only with `--session`)
  makes connect write `{"v":1,"idleSince":<ms>|null,"approvalSince":<ms>|null}`
  there (unix milliseconds, 0600, atomic rename) whenever either value
  changes. `idleSince` is when the session last became quiet: no harness turn
  in flight (a turn silent for hours is still in flight), no approval open,
  nothing queued and no interrupt outstanding; it is `null` otherwise.
  `approvalSince` is when the open approvals went from none to some, `null`
  when none is open. The sandbox Worker reads it to pause an idle sandbox; no
  file means busy. A relative path is ignored with a warning.
- SIGTERM (a pause or stop) first answers every open approval deny (Claude
  Code: "Denied: the sandbox stopped before an approval arrived.", Codex:
  decline), stops the harness and exits 0.
- `--resume-from <uuid>` (with `--session`; another session's id) continues
  that earlier session's harness conversation from its files in `HOME`
  (`~/.claude/projects`, `~/.archivist/connect/sessions`,
  `~/.archivist/connect/codex-home`): the new session reuses the earlier
  working directory path and its Claude Code session (`--resume`) or Codex
  thread (`thread/resume`, with the earlier Codex home moved to the new
  session's and its `auth.json` link recreated). The earlier record is ended.
  Only the harness conversation is restored: the earlier working directory
  path is recreated, and in a sandbox that is a fresh container, so the files
  the agent wrote there earlier are not there.
  When the earlier session cannot be used (no record, another agent, no
  harness id) or the resume fails (Codex `thread/resume` refused, Claude
  Code exiting before its init because no transcript matches), the session
  starts fresh and its first answer begins with "The earlier conversation
  could not be restored, so this answer starts without it."

The session home is kept while the session can resume and removed when the
session stops, fails or is found inactive at the next start.

The relay sends only data (session ids, prompts, approval decisions,
interrupt and stop, and the mode, model and effort ids, each checked against
the closed set or this machine's own list). Binaries, arguments and flags are fixed on your machine.
Session ids and working directories are kept in `~/.archivist/connect/`
(0700); after a restart, the next message resumes the same Claude session.
Each running session's task token and MCP config sit in
`~/.archivist/connect/run/<session>/` (`task-token`, `mcp.json`, 0600), are
removed when the session stops or the daemon exits, and the token is revoked
then. Ctrl-C stops every Claude Code and Codex process and revokes its tokens.

`ARCHIVIST_CONNECT_DEBUG=1` adds debug lines to the log (the harness
reported context use and window after each turn, as also sent on
`data-usage`).

`ARCHIVIST_RELAY_URL` overrides the relay address (default
`wss://relay.mosaic-finance.com`). It runs on macOS, Linux and Windows
(Windows 10 or 11; each session's process tree is held in a job object).

Exit codes: 0 stopped by Ctrl-C (or `--check` found a usable Claude Code or
Codex); 1 refused or stopped (feature not enabled, another `archivist connect`
took over, Claude Code too old, or a session-bound start refused, for
example a model or effort this machine does not offer), service not
running, or service setup failed; 2 bad flag or relay URL; 3 no usable harness (Claude Code not found,
or neither Claude Code nor an installed Codex is usable); 4 credential or
Claude login problem, or an invalid or expired pairing code; 5 server or
network error while pairing; 7 pairing rate limited. When Codex is not
installed, the Claude Code codes apply as before.

## Claude Code skill

The skill in `skill/` teaches Claude Code the research loop: search, read,
cite the permalink. Homebrew installs it; elsewhere run
`archivist update --skill`. Plugin manifest: `.claude-plugin/plugin.json`.

## Local development

```sh
git clone https://github.com/mosaicss/archivist.git
cd archivist
go test ./... -race
go build ./cmd/archivist
./archivist version
```

`go.mod` pins the toolchain (`toolchain go1.27.1`); CI and releases build with
exactly that version. Lint and release config checks match CI:

```sh
golangci-lint run                 # v2.14.0
goreleaser check
goreleaser build --snapshot --clean
```

## Release process

Releases are cut by pushing a semver tag on a merged, CI green commit:

```sh
git tag v0.2.23
git push origin v0.2.23
```

`.github/workflows/release.yml` runs goreleaser: 5 platform binaries plus a
universal macOS binary, `archivist_v<version>_SHA256SUMS`,
`archivist_v<version>_skill-bundle.tar.gz`, a GitHub Release, SLSA build
provenance, and the Homebrew cask `Casks/archivist.rb` pushed to
`mosaic-finance-inc/homebrew-tap` (needs the `HOMEBREW_TAP_GITHUB_TOKEN`
secret). macOS signing and notarization run only when the `APPLE_*` secrets are
set.

## Architecture

This binary is a thin client over Mosaic's chat-api: the `/research` routes for
search and reading, `/account` routes for credentials and usage. The monorepo
reference is `reference/archivist/cli.md`.

## License

[Apache 2.0](./LICENSE). Copyright 2026 Mosaic Finance Inc.
