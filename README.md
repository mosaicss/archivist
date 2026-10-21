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
`%LOCALAPPDATA%\Programs\archivist` on Windows) after checking the download
(see [Verifying a release](#verifying-a-release)), and adds the Claude Code
skill when `~/.claude/skills` exists. `install.sh` needs `curl`, `tar` and
`openssl`, which macOS and most Linux systems include. On Linux it also
prints the install command for `bubblewrap` and `socat` when either is
missing; `archivist connect` uses them. A Homebrew install is upgraded with `brew upgrade`
instead (only while the cask is still installed). With a pairing code from the Mosaic workspace it also connects this
machine; see [One-step connect](#one-step-connect).

### Homebrew (macOS)

```sh
brew install mosaic-finance-inc/tap/archivist
```

Installs the universal macOS binary as a Homebrew cask and places the Claude
Code skill in `~/.claude/skills/archivist/`.

### GitHub Releases (macOS, Linux, Windows)

Download the archive for your platform, the checksum file and its signature
from https://github.com/mosaicss/archivist/releases, verify, and put the
binary on your PATH. For example, Linux amd64:

```sh
VER=0.2.35
BASE=https://github.com/mosaicss/archivist/releases/download/v${VER}
curl -fsSLO ${BASE}/archivist_v${VER}_linux_amd64.tar.gz
curl -fsSLO ${BASE}/archivist_v${VER}_SHA256SUMS
curl -fsSLO ${BASE}/archivist_v${VER}_SHA256SUMS.sig
# archivist-release-1.pem: internal/release/keys/ in this repository
openssl dgst -sha256 -verify archivist-release-1.pem \
  -signature archivist_v${VER}_SHA256SUMS.sig archivist_v${VER}_SHA256SUMS
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

### Verifying a release

Each release's `archivist_v<version>_SHA256SUMS` lists the SHA-256 of every
archive and the skill bundle, and `archivist_v<version>_SHA256SUMS.sig` is its
signature (RSA, SHA-256, PKCS#1 v1.5) by the Archivist release key. The public
key is in [`internal/release/keys/`](internal/release/keys/) and is also built
into `install.sh`, `install.ps1` and the `archivist` binary.

- `install.sh` and `install.ps1` check the signature (with `openssl`, or .NET
  on Windows), then each download against `SHA256SUMS`, before anything is
  installed.
- `archivist update` and `archivist update --skill` do the same with the key
  built into the running binary. `archivist update` also keeps the installed
  version when the latest release is older, and leaves an executable binary.
- The Homebrew cask checks the skill bundle against the hash written into it
  at release time.
- The release files also carry GitHub build provenance:
  `gh attestation verify archivist_v<version>_linux_amd64.tar.gz -R mosaicss/archivist`.

`ARCHIVIST_RELEASE_BASE_URL` (both install scripts) and `ARCHIVIST_BASE_URL`
(the binary) must be `https://` URLs; plain `http://` is accepted only for
`127.0.0.1`, `::1` or `localhost`, for local testing.

## Authenticating

Agent access is part of a Mosaic Pro account (plans:
https://mosaic-finance.com/en/pricing/). Signed in on Mosaic, open your avatar
menu, then Manage account, then API keys, and add a key. Copy the `ak_...`
token and save it once:

```sh
archivist auth login --token ak_<your-token>
```

The CLI verifies the token against the server and saves it. On a computer
that does not run `archivist connect`, it goes to your operating system's
keychain (macOS Keychain, the Secret Service on Linux desktops, Windows
Credential Manager; item `archivist`, account the credentials path below).
Where no keychain can be used (a Linux machine without a session bus or
without a Secret Service on it, or a keychain that does not answer within 30
seconds), or once `~/.archivist/credentials` exists (a symlink from a
dotfiles setup included), it writes `~/.archivist/credentials` (mode 0600)
instead, and that file stays the store. A computer that runs `archivist
connect` (the background service is installed or `~/.archivist/connect`
exists) keeps its key in `~/.archivist/credentials` (0600), which connect
sessions do not open: `auth login` and `--pair` save there, and `--install`
or a connect start moves a key from the keychain there. Archivist asks the
keychain only after it saved a key there (it notes that in
`~/.archivist/keychain`, a file with no secret in it), so a computer that
never did is never asked to unlock it. `archivist auth
status` shows which credential is active and where it came from (`keychain`
or `file`); `archivist auth logout` deletes the saved key from both. A shell
that cannot reach the keychain (for example SSH into a Mac whose login
keychain is locked, cron, or `sudo -u`) does not find a key saved there: in
such a shell set `ARCHIVIST_TOKEN` or pass `--token`.

For CI and scripting, `ARCHIVIST_TOKEN=ak_...` overrides the saved key, and
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
`read section`, `toc`, `filings`, `screen` (its `rows`) and `find` also fit each page to 24,000 bytes
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
| `filings <symbol>` | A company's filings, newest first, each with the share move measured around it (`reaction`). `--formtype`, `--date-from`, `--date-to`, `--limit 1-25` (default 10), `--min-reaction-z 0-50` (only filings whose move is at least that many standard deviations either way; 0 drops unmeasured filings; adds a `MOVE (z)` table column), `--cursor`; an empty list exits 0 |
| `screen` | Screen companies on the nightly snapshot: `--min KEY=VALUE` and `--max KEY=VALUE` (repeatable, for example `--max pe=15 --min market_cap_usd_m=500`), `--industry`, `--sector`, `--exchange`, `--market`, `--country` (comma separated), `--currency` (needed for a figure in each company's own reporting unit), `--include-inactive`, `--as-of`, `--sort-by`, `--sort-dir asc\|desc`, `--fields`, `--limit 1-50` (default 25), `--cursor`. The server checks every key and value; a bad bound shape exits 2 before any request |
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

Tools (9): `search`, `read_passage`, `read_section`, `toc`, `filings`,
`screen`, `find`, `companies_search`, `companies_get`. Tool names join the verb path
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
`companies_search`, `read_passage`, `read_section`, `toc`, `filings`,
`screen` and `find`. `companies_get` falls back to the full company catalog, which task
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
archivist connect --pair ABCDE-FGHJK   # redeem the code for a new ak_ key and save it in ~/.archivist/credentials
archivist connect --install            # run connect as a background user service
archivist connect --status             # report the service, its ceiling, the saved key, its account and its browsers
```

A first install (no background service yet) asks one question on the
terminal, the same way pairing asks:

```text
Start Archivist when your computer starts? (recommended) [Y/n]
```

Enter, `y` or `yes` installs the background service as below. `n` or `no`
installs nothing more: the installer prints "Archivist will not start by
itself. Run 'archivist connect' when you want to use your agents from
Mosaic." and exits 0. Ctrl-C stops the install (exit 1). Where no terminal
can answer (an agent shell that captures the output, CI), the answer is yes
and the installer says so in one line ("Starting Archivist when your
computer starts (the default)."). An update, or a re-pair with the service
already installed, reinstalls it without asking. `--no-service` (`-NoService`
on Windows) still means no service.

Before it saves anything, `--pair` names the account the code belongs to and
has it confirmed. On a terminal it asks:

```text
This computer will connect to you@example.com. Continue? [Y/n]
```

Enter, `y` or `yes` saves the key; any other answer, Ctrl-C or the end of
input cancels: nothing is saved, the new key is revoked and the command exits
1. The question is asked on the terminal itself (`/dev/tty`, on Windows the
console), so it works when the command is piped into `bash`.

The question is asked only when the output shows on a terminal too. Where no
terminal is available, or nobody sees it (an agent shell that captures the
output, for example), `--pair` opens
a Mosaic page in this computer's browser instead and waits for it. A browser
signed in to the account the code came from connects the computer with no
click; a browser signed in to another account, or not signed in, does not,
and nothing is saved. If the browser cannot be opened, `--pair` prints the
link to open by hand. Ctrl-C while it waits cancels the pairing; after five
minutes without an answer it stops with nothing saved. With neither a
terminal nor a browser, `--pair` exits 1 before it sends the code, so the
code stays usable: run the command in a terminal. No flag skips the
confirmation. `ARCHIVIST_PAIR_CONFIRM=browser` chooses the browser page even
on a terminal, and `BROWSER` names the program that opens it (the first
entry of the list).

`--pair` saves the key in `~/.archivist/credentials` (0600), where connect
keeps its key; connect sessions do not open that file. An earlier key kept in
the keychain is removed from it. `archivist connect --install`, and connect
itself when it starts, move a key saved in the keychain (with `auth login`)
into that file, and remove a keychain item left beside it (tried again at
every start until it is gone). A desktop keyring stays locked until you log in, so a
background service started at boot (lingering) whose key is still in the
keychain logs "waiting for the keyring" and connects after your login.

`--pair` accepts the code in any case, with or without the hyphen or spaces,
and never prints the key (only its masked form). An unknown, used or expired
code exits 4: get a new code in Mosaic. The redeem request is sent once and
never retried. If `ARCHIVIST_TOKEN` is set it still takes precedence in that
terminal, and `--pair` warns about it. Pairing again replaces the saved key
and then revokes the earlier `ak_` key (`DELETE /account/cli-tokens/current`,
sent with that key), so it stops working; if that call fails, `--pair` says
the earlier key is still active (masked) so you can revoke it in Mosaic.
When the background service is installed, `--pair` says to run
`archivist connect --install` so the service restarts with the new key.
`--status` shows the saved key's account (`Account:`), or says the key is no
longer accepted or that Mosaic could not be reached; its exit code does not
depend on that line. Where the key goes to the file, a
`~/.archivist/credentials` symlink (dotfiles) is kept: the key is written to
the file it points to, which is created when it does not exist yet.

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
when a service is installed).

`archivist connect --uninstall` stops and removes the service and cleans the
computer up: on Linux it turns lingering off again when Archivist turned it
on (recorded in `~/.archivist/connect/linger` when an install found it off
and turned it on). Lingering you had on before stays on; when no record says
who turned it on (a service installed by an earlier release), it stays on
and the command prints "Lingering stays on. Run 'loginctl disable-linger' if
nothing else needs it.". A paused computer's pause is cleared in Mosaic
first, so the workspace stops showing it paused. Then it revokes a saved `ak_` key
(`DELETE /account/cli-tokens/current`, sent with that key) and removes it
from the keychain and `~/.archivist/credentials`, and it removes
`~/.archivist/connect` (session records, logs, Codex homes) and the session
work folders. The saved `ak_` key is revoked also when you saved it with
`archivist auth login --token`, so it then stops working everywhere you use
it (other computers that use the same key need a new one). A key saved
with `archivist auth login` that is not an `ak_` key is left alone. When the revoke fails the files are still removed, and
the command says "Remove this computer's key in Mosaic settings." and exits
1. With nothing installed it says "Background connect is not installed." and
cleans up the same way.

### Pause

Pause stops your agents on this computer without uninstalling anything:

```sh
archivist connect --pause    # sessions stop; no relay connection until resumed
archivist connect --resume   # connect again (within about 2 seconds)
```

The workspace offers the same Pause, and while paused shows "Paused on this
computer" with Resume. Paused, connect holds no relay connection: it ends
its sessions (they resume from where they were after Resume), revokes their
task tokens and keeps the pause in `~/.archivist/connect/paused`, which
survives restarts. It tells the Mosaic relay it is paused and, every 10
seconds, asks it over HTTPS whether you chose Resume in the workspace (a
computer that was asleep or off then still resumes when it wakes, and
starting Archivist on another computer never resumes this one). `--status`
shows a `Paused:` line. A relay that does not know Pause yet keeps the
computer paused here only (the workspace shows it offline) until
`archivist connect --resume`.

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
about 5 seconds to write its pid first. `--pair`, `--trust`, `--install`,
`--uninstall`, `--status`, `--pause` and `--resume` are used one at a time.

#### Windows

In PowerShell (designed to need no admin rights; nothing asks for elevation):

```powershell
& ([scriptblock]::Create((irm https://github.com/mosaicss/archivist/releases/latest/download/install.ps1))) -Pair ABCDE-FGHJK
```

`install.ps1` installs or updates `archivist.exe` and `archivistw.exe` in
`%LOCALAPPDATA%\Programs\archivist` (a running exe is renamed aside, never
overwritten), adds that folder to your user PATH, then pairs, installs and
reports background connect like `install.sh` (the first install asks the
same start question). Options: `-Pair CODE`,
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
`ARCHIVIST_INSTALL_VERSION` installs a given tag (v0.2.35 or later: earlier
releases have no `SHA256SUMS.sig`; for those run that release's own script,
`https://github.com/mosaicss/archivist/releases/download/<tag>/install.sh` or
`.../install.ps1`), and `ARCHIVIST_RELEASE_BASE_URL` points the script at another release host (for
testing; `https://`, or `http://` to a loopback host). Each released script is
stamped with its own tag.

### Commands from the browser are signed

Each browser you use Mosaic in has its own signing key (a WebCrypto ECDSA
P-256 key that never leaves the browser). The commands that let an agent do
something on this computer are signed in the browser: starting a session,
sending a message, changing the permission mode, allowing a tool call, and
adding another browser. `archivist connect` runs them only when the key is
one this computer knows and the signed content matches the command: this
computer, the session, the agent, the text you sent, the search filter and
attached filing lines Mosaic puts in front of that text, the mode, and, for
an approval, the exact card you answered. Each signature carries a counter
and is accepted once. Stopping a session, interrupting a turn and denying a
tool call need no signature. A command that does not pass is not run and
the session shows why; a tool call whose allow did not pass is denied
instead, with no session rule kept.

- **This computer.** Each computer has its own id, made the first time it
  is needed and kept in `browsers.json` (pairing again keeps it; a new id
  comes only with a new file). `archivist connect` announces it with the
  browser list, and every signed command names the computer it is for; a
  command for another computer is not run.
- **Search filters and attached filings.** The browser signs the exact
  lines Mosaic adds in front of your message (search filters, an attached
  filing) together with the message, so a message runs only with the lines
  the browser sent.
- **Delivery.** `archivist connect` asks the relay for signed commands
  (`signing=1` on its sockets). A relay that does not know that request
  refuses the connection (HTTP 400, `INVALID_QUERY`); `archivist connect`
  then connects without it for that attempt and logs that once. Every new
  connection asks again, so signed commands resume as soon as the relay
  knows the request.
- **Filing links on approval cards.** Links to filing sources (regulator,
  exchange and filing vendor document URLs) are removed from an approval
  card before it is sent and signed, the same way Mosaic removes them from
  agent answers: a command such as `curl https://www.sec.gov/...` shows as
  `curl (source link removed)`; a link whose host name holds a character
  outside ASCII is removed from the card too. Allowing it still runs the
  command the agent asked for, unchanged.

The browsers this computer knows are listed in
`~/.archivist/connect/browsers.json` (0600; at most 32, the oldest dropped).
A browser is added in one of three ways:

- the pairing command Mosaic shows carries the browser's code after the
  pairing code (`archivist connect --pair ABCDE-FGHJK.<code>`): the code is
  checked first (a malformed one exits 2 and nothing is sent), only the
  pairing code is sent to Mosaic, and once the key is saved the browser is
  added;
- `archivist connect --trust <code>`, with the code Mosaic shows in the
  browser to add (prints "This browser can now use this computer.");
- Connect your computer in Mosaic, from a browser this computer already
  knows, with the new browser's code.

Pairing with a different account empties the list first; pairing again with
the same account keeps it. Nothing a server sends adds a browser. A running
`archivist connect` tells the relay which browsers it knows when it connects
and again within one ping (20 seconds) of a change, so Mosaic can show the
one time step to add a new browser. `--status` shows how many browsers this
computer knows (`Browsers:`). Used counters are kept in
`~/.archivist/connect/browser-counters.json`; if either file cannot be read,
signed commands are not run until it is fixed (pairing again rewrites an
unreadable list; an unreadable counters file has to be removed). Session-bound mode (`--session`, a Mosaic cloud sandbox)
does not use the list.

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
at least one of: Claude Code 2.1.299 or newer logged in with a claude.ai
subscription (`claude auth status` shows `authMethod: claude.ai`), or Codex
0.163.0 or newer logged in with ChatGPT (`codex login status`). API key logins
are refused. On Linux, Claude Code sessions also need two packages, bubblewrap
and socat (for example `sudo apt install bubblewrap socat`); `connect --check`
and the startup log say so when either is missing, and Codex is unaffected. Newer versions are reported, not blocked; a harness that is not
usable when `archivist connect` starts is reported as unavailable and its
sessions are refused (restart after logging in).

### Permission modes, model and effort

Each session runs in one of four permission modes, chosen in the workspace
when it starts and changeable while it runs. Low to high:

| Mode | Label | What it means | Claude Code | Codex |
|---|---|---|---|---|
| `read_only` | Read only | Reads and research run; anything that would change something is denied without asking | `--permission-mode plan` | `never`, profile `archivist-read` (read only, no network) |
| `ask` | Ask every time | Every call that needs permission is a card | `--permission-mode default` | `untrusted`, profile `archivist-write` (writes in the session folder, no network) |
| `auto_edits` | Auto edits | File edits run (Claude Code also runs simple file commands such as mkdir, mv, cp and rm inside the session folder); other commands ask | `--permission-mode acceptEdits` | `untrusted`, profile `archivist-write`; the daemon accepts file changes inside the session folder |
| `full_auto` | Full auto | The harness runs without asking, but anything it still asks about (for example deleting a critical folder) is shown as a card | `--permission-mode bypassPermissions` | `never`, profile `archivist-full` (writes in the session folder, network on) |

The Codex profiles apply on macOS and Linux. On Windows Codex keeps the
sandbox modes of earlier releases (`read-only`, `workspace-write`, and
`danger-full-access` for `full_auto`) in its unelevated Windows sandbox.

The Mosaic tools (`search`, `companies_search`, `companies_get`,
`read_passage`, `read_section`, `toc`, `filings`, `screen`, `find` and
`publish_artifact`) never ask, in any mode (`read_only` included) and on both
harnesses: the mode governs only the tools that touch your computer (shell
commands, file edits and writes). The daemon
enforces the mode itself on every approval request that reaches it,
whatever the harness version does: Mosaic tools are allowed, `read_only`
denies everything else without a card, and every other mode shows a card,
`full_auto` included (Claude Code keeps some prompts even in bypass
permissions, such as removing a critical directory). In `auto_edits` the
daemon accepts a Codex file change without a card only when it asks for no
extra root and every path, resolved through symbolic links, is inside the
session folder; Codex commands always ask. These rules come before answers
remembered for the session ("allow for session" does not let a `read_only`
session change anything). Every Claude Code launch also passes
`"useAutoModeDuringPlan":false` in `--settings`, so plan mode never lets the
auto mode classifier approve shell commands.

### What a session can open

Each session works in its own folder,
`~/.archivist/connect/work/archivist-connect-*` (0700). Before a session
runs there again (a resume after a restart, or `--resume-from`), the daemon
checks that the folder is a directory, not a link, that it sits in that
folder (or, for a session started by an earlier release, in the system
temporary directory) and, on macOS and Linux, that it belongs to you with
mode 0700; otherwise the session does not run there. When `archivist
connect` starts, session folders that no active session uses are removed.

At every launch the daemon computes a list of locations a session does not
open, and passes it to each harness's own sandbox and permission settings:

- `~/.ssh`, `~/.aws`, `~/.config/gcloud` (and `$XDG_CONFIG_HOME/gcloud`),
  `~/.azure`, `~/.kube`, `~/.docker/config.json`, `~/.gnupg`, `~/.netrc`,
  `~/.npmrc`, `~/.pypirc`;
- the Claude Code and Codex logins: everything in `~/.claude` (and
  `CLAUDE_CONFIG_DIR`) except the session's own conversation data,
  `projects/<session folder name>`, where Claude Code keeps large tool
  results it reads back (for the Read and Edit tools, the other project
  folders present when the session starts; Claude Code's shell sandbox
  closes all of `projects` but the session's own, later ones included;
  Codex sessions lock these folders whole); `~/.claude.json`; `~/.codex` (and your
  `CODEX_HOME`; when the Codex program itself is installed in there, the
  folder holding it stays readable);
- browser profiles and password managers (Chrome, Chromium, Brave, Edge,
  Firefox, Arc, Safari, 1Password, Bitwarden, KeePassXC, the keychains and
  keyrings of each OS; on Linux also under `$XDG_CONFIG_HOME`);
- everything in `~/.archivist` except the session's own folder: the saved
  key, the session records, task tokens, Codex homes, logs and the other
  session folders present when the session starts. Claude Code's shell
  sandbox also closes the whole work folder except the session's own, so
  its shell commands miss folders created later too.

Claude Code plan files (Read only mode) are written to `.plans` in the
session folder.

Each entry is listed as written and with symbolic links resolved. Claude
Code gets it through `--settings`, as a file in the session's private run
folder (`~/.archivist/connect/run/<session>/claude-settings.json`, 0600;
`--setting-sources ""` still loads none of your settings files): on macOS and Linux its shell sandbox (`sandbox.enabled`, required,
no unsandboxed retry, Bash still asks per mode, network as before) denies
reading the list, and Read and Edit deny rules cover the same paths in every
mode; on native Windows, which has no Claude Code shell sandbox, the deny
rules apply and every Bash or PowerShell command asks. Codex on macOS and
Linux runs in the permission profiles above, each denying the list. The
daemon also answers any approval whose input names a listed location with a
deny (Codex: decline), without a card, in every mode. The archivist MCP
server and the harnesses themselves run outside these sandboxes and read
their own files as before.

A Claude Code session whose start reports an MCP server other than
`archivist`, or a plugin that is not one of Claude Code's built in ones,
ends before anything is sent.

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
at the next turn (every `turn/start` carries the current policy and profile;
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

- runs `claude -p` in stream-json mode in its session folder (above), with
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
  "Allow for session" is kept as a narrow rule in Claude Code's own rule
  style, and the card says what it covers: for Bash, the command prefix (the
  program and its subcommand word, such as `ls` for `ls -la` or `git status`
  for `git status`, matched on whole words, so `ls` never covers `lsof`), or
  the exact command when it has shell operators, redirections, substitutions
  or an environment assignment, starts with a wrapper, interpreter or a
  command that edits files or reaches the network (such as `sudo`, `bash`,
  `python3`, `xargs`, `sed`, `rm`, `curl`), or has a runner word such as
  `run`, `exec` or `x` after the program; a prefix never covers a `find` that
  runs commands, deletes or writes files, or an option that runs another
  program (such as `git -c`, `tar -I`, `rsync -e`, in any spelling). For Edit
  and Write it is the absolute file path (one rule for both tools; a path with
  a `..` element is kept as that exact call; letter case is compared as
  written), and for Read, Glob,
  Grep and any other tool every later call of that tool. "Deny for session"
  applies to every later call of that tool. Both live in the daemon's memory,
  so restarting `archivist connect` clears them;
- denies, without a card, an approval whose card would have to be cut down to
  be sent: a shell command over 16000 bytes, or a call too large for one relay
  message. Claude is told to run it in smaller steps. The daemon never sends a
  cut down approval card, so an answer never covers input the card's data
  left out;
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

- runs one `codex app-server --stdio --strict-config` in its session
  folder, with the same environment allowlist plus `CODEX_HOME` set to a
  private per-session home, `~/.archivist/connect/codex-home/<session>/`
  (0700). That home holds only `auth.json` as a link to your login
  (`$CODEX_HOME/auth.json` if `CODEX_HOME` is an absolute path, else
  `~/.codex/auth.json`), never a copy; Codex refreshes the login in place
  through the link. Your Codex config, `AGENTS.md`, rules, hooks, plugins,
  skills, memories, MCP servers, notifications and project trust are not
  loaded and never written; parent `OPENAI_*` and `CODEX_*` variables never
  pass;
- fixes the rest with `-c` overrides: ChatGPT login only, the `openai`
  provider, the three permission profiles of the modes (macOS and Linux:
  `archivist-read` read only, `archivist-write` writing only the session
  folder with `/tmp` read only, both without network, and `archivist-full`
  the same with network; each denies the locked
  locations above; `TMPDIR` points at `<session dir>/.tmp`; on Windows the
  `workspace-write` sandbox of earlier releases, `read-only` for `read_only`
  and `danger-full-access` for `full_auto`), a core
  shell environment for commands, web search off (`web_search="live"` with
  web search on), history off,
  no project root markers, and the archivist MCP server (task mode tools,
  task token as above; Mosaic read tools are approved without asking). The
  approval policy and permission profile (from the session's mode,
  reviewed by you; on Windows the sandbox) and model are set per thread and
  on every turn; `--codex-model` defaults
  to Codex's default model and `--codex-effort` to Codex's default effort;
- refuses the session (error plus a failed status, Codex stopped, nothing
  else sent) unless Codex reports the session home, a ChatGPT account, the
  `openai` provider, the approval policy of the session's mode reviewed by
  the user, the mode's permission profile as the active one (on Windows the
  sandbox of the mode: for `workspace-write` without network, extra roots,
  `/tmp` or `$TMPDIR`, TMPDIR pointing inside the session directory; for
  `read-only`: without network), the launch's `web_search` value set by its
  `-c` flags (`config/read`), no instruction files, and the archivist MCP
  server ready with no other MCP server. A later
  switch away from the ChatGPT login ends the session the same way;
- turns command and file change approvals into workspace cards. Your answer
  maps one to one onto Codex's choices: allow once is accept, allow for
  session is Codex's accept for session (the identical command, or the same
  file, runs again without asking for the rest of the session), deny once (or
  no answer within 60 seconds) is decline, and deny for session is cancel
  (deny and end the turn). An archivist tool approval, should Codex ask for
  one, is accepted without a card in every mode. archivist connect never
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
Like the other Mosaic tools it never asks, in any mode (pre-allowed in
Claude's `--allowedTools`; Codex's archivist server default `approve` with no
per tool override), and it stays confined to the session directory: it
refuses, and uploads nothing, for a path outside the session directory or containing
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
(0700), the session folders in `~/.archivist/connect/work/`; after a restart, the next message resumes the same Claude session.
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
goreleaser check                  # v2.18.2
goreleaser build --snapshot --clean
```

CI and the release workflow install exactly these versions from their
release tarballs, checked against pinned SHA-256 digests
(`scripts/install-release-tools.sh`).

## Release process

Releases are cut by pushing a semver tag on a merged, CI green commit of
`main`:

```sh
git tag -a v0.2.35 <merged commit> -m v0.2.35
git push origin v0.2.35
```

Releases are immutable and the `v*` tag ruleset admits one tag push per
version: a failed run spends the version, and the fix ships as the next
patch. Before tagging:

- add the user facing notes as `.github/release-notes/v<version>.md` in the
  release commit;
- rehearse the build, sign and publish steps of `release.yml` locally
  (a scratch clone with a local tag and no remote, a throwaway signing key
  and a stand in `gh`), since the workflow cannot be tried on GitHub without
  spending a version;
- check that no other tag points at the release commit
  (`git tag --points-at <commit>` is empty): a second tag, such as a release
  candidate, on the same commit is not a supported release.

`.github/workflows/release.yml` runs three jobs, each with only the
permissions and secrets it uses, and every action pinned by commit SHA. Every
step runs under `bash -eo pipefail`.

1. `build` (read only, no secrets): checks the tag, verifies the Go modules,
   and runs goreleaser v2.18.2 for the pushed tag (`GORELEASER_CURRENT_TAG`)
   without publishing: 5 platform binaries plus a universal macOS binary
   (built with `-trimpath` and the commit's date),
   `archivist_v<version>_SHA256SUMS`,
   `archivist_v<version>_skill-bundle.tar.gz`, the stamped installers, the
   changelog and the Homebrew cask. The release notes are the committed
   notes for the tag, when there are any, then goreleaser's changelog.
2. `sign` (the `release` environment, which admits `v*` tags only and holds
   the signing key): checks the files against `SHA256SUMS`, writes
   `archivist_v<version>_SHA256SUMS.sig`, verifies it with the committed public
   key, and records SLSA build provenance for every release file.
3. `publish`: verifies again, creates the GitHub Release with every asset
   (9 files) and the release notes, then pushes `Casks/archivist.rb` to
   `mosaic-finance-inc/homebrew-tap` (needs the `HOMEBREW_TAP_GITHUB_TOKEN`
   secret). A prerelease tag (with a suffix) is marked as a prerelease and
   leaves the cask alone. A rerun keeps an existing release only when it is
   published with this run's `SHA256SUMS`; a draft left by a failed run is
   deleted first, then the failed jobs are run again.

macOS signing and notarization are not part of the workflow.

## Architecture

This binary is a thin client over Mosaic's chat-api: the `/research` routes for
search and reading, `/account` routes for credentials and usage. The monorepo
reference is `reference/archivist/cli.md`.

## License

[Apache 2.0](./LICENSE). Copyright 2026 Mosaic Finance Inc.
