# Security Policy

## Reporting a Vulnerability

Email **security@mosaic-finance.com** with:

- A description of the issue and where it lives (command, endpoint, package, or file).
- Reproduction steps or a proof of concept if you have one.
- Any impact assessment you can offer (what an attacker gains).

You can also report privately through GitHub: open the repository's **Security** tab and choose **Report a vulnerability** (private vulnerability reporting).

Please do not open public GitHub issues for security reports.

## Response SLA

| Stage | Commitment |
|---|---|
| Acknowledge receipt | within 48 hours |
| Triage verdict (real, duplicate, or not applicable) | within 7 days |
| Fix shipped or risk formally accepted | within 30 days |

## Supported Versions

| Version | Supported |
|---|---|
| Latest 0.2.x release | Yes |
| Older releases | No. Upgrade via `archivist update` or your install channel. |

## Scope

This policy covers the Archivist CLI binary, its install scripts, and the distribution channels in this repository (install scripts, Homebrew tap, GitHub Releases, `go install`). Reports about the Mosaic web platform and its APIs are welcome at the same address.

Release integrity: every release publishes `archivist_v<version>_SHA256SUMS` and its signature `archivist_v<version>_SHA256SUMS.sig`, made by the release workflow with the Archivist release key. The public key is in `internal/release/keys/` and is built into `install.sh`, `install.ps1` and the binary; the install scripts, `archivist update` and `archivist update --skill` install nothing unless the signature verifies and each download matches `SHA256SUMS`. To check a download by hand: `openssl dgst -sha256 -verify archivist-release-1.pem -signature archivist_v<version>_SHA256SUMS.sig archivist_v<version>_SHA256SUMS`, then `sha256sum -c` against that file. GitHub build provenance stays available with `gh attestation verify <file> -R mosaicss/archivist`. The download and API URL overrides (`ARCHIVIST_RELEASE_BASE_URL`, `ARCHIVIST_BASE_URL`) accept `https://` only, or `http://` to a loopback host for local testing.

A note on tokens: on a computer that runs `archivist connect`, the Archivist API key (`ak_` prefix) is kept at `~/.archivist/credentials` with permissions 0600, which connect sessions do not open; `connect --pair` and `auth login` save it there and `connect --install` or a connect start moves a key from the keychain there. A key saved with `archivist auth login` on a computer that does not run connect goes to the operating system keychain (macOS Keychain, the Secret Service on Linux, Windows Credential Manager; item `archivist`), or to that file where no keychain can be used or the file already exists. The key never appears in a command line, a log or another file; every keychain call is time limited (5 seconds to read or delete, 30 seconds to save so an unlock prompt can be answered), only one keychain request is open at a time, on Linux the keychain is used only when a session bus with a Secret Service already runs, and the keychain is asked only after Archivist saved a key there (noted in `~/.archivist/keychain`, which holds no secret). `archivist connect --uninstall` revokes the saved `ak_` key and removes it, together with `~/.archivist/connect` and the session work folders. If you believe a token has leaked, revoke it immediately from your account settings at mosaic-finance.com (Manage account, API keys) and mint a fresh one.

`archivist connect` (preview) also keeps short-lived session task tokens (`mst_` prefix, at most 15 minutes, search and read scopes only) on disk while a session runs: `~/.archivist/connect/run/<session>/task-token` and the `mcp.json` that names it, both 0600 in a 0700 directory. They are removed when the session stops or the daemon exits, and the daemon revokes each task token at that point (and when it rotates one). Session records in `~/.archivist/connect/sessions/` hold no tokens. In a connected Claude Code session, "Allow for session" approves later calls only within a narrow rule the daemon derives from the approved call, in Claude Code's own rule style: for Bash, the command prefix (program and subcommand word, whole words only) when the command is plain, otherwise that exact command (shell operators, redirections, substitutions, environment assignments, wrappers, interpreters, commands that edit files or reach the network, and runner words such as `run` or `exec` anywhere after the program are always exact, and a prefix never covers a `find` that runs commands, deletes or writes files, or an option that runs another program); for Edit and Write, that absolute file path, shared by both (a path with a `..` element is kept as that exact call, and letter case is compared as written, also on Windows); for Read, Glob, Grep and other tools, every later call of the tool. The workspace card states what the answer covers. "Deny for session" refuses every later call of that tool. Read only overrides every session answer and denies anything that would change something. These rules are kept in the daemon's memory only, so restarting `archivist connect` clears them. An approval whose card would have to be cut down to be sent (a shell command over 16000 bytes, or a call too large for one relay message) is denied without a card, so an answer never covers input the card's data left out. The Mosaic tools, `publish_artifact` included, never ask in any mode; `publish_artifact` uploads only regular `.pdf`, `.txt`, `.md`, `.csv` or `.json` files of at most 10 MiB from inside the session directory. Codex sessions map these answers differently (below).

Permission modes and the ceiling: each session runs in a permission mode chosen in the workspace (Read only, Ask every time, Auto edits, Full auto), capped by `archivist connect --max-permission`, whose default is `auto_edits`; the workspace can never raise it. `--max-permission full_auto` lets a session run Claude Code with `bypassPermissions` and Codex with `danger-full-access`, which means network access and no sandbox. A Full auto session steered by text it reads (for example from a filing) could then read local credentials, such as the linked Codex `auth.json`, `~/.archivist/credentials` (where the key is kept in a file) or the session's task token, and send them anywhere, without an approval card. Only the prompts a harness still raises in that mode reach you as cards. Set this ceiling only on a machine or account without sensitive files, or for sessions you watch.

Codex sessions in `archivist connect` run with a private Codex home per session, `~/.archivist/connect/codex-home/<session>/` (0700). Its `auth.json` is a symbolic link to your own Codex login file, so Codex reads and refreshes your ChatGPT login in place; the daemon never copies, reads or prints its contents. Removing the session home removes only the link. The home also holds that session's Codex history (rollouts and local databases) and is deleted when the session stops, fails, or is found inactive when `archivist connect` starts. Codex approvals map onto Codex's own choices: "Allow for session" is Codex's accept for session, which Codex applies to the identical command or the same file for the rest of that session, and "Deny for session" is cancel (deny and end the turn). Codex runs the archivist tools without asking (the server default approval mode `approve`), and should it still ask for one, the daemon accepts it without a card in every mode, Read only included. archivist connect never sends Codex rule amendments or "always" MCP approvals, so nothing is written to your Codex config or rules.

Known limit of Codex sessions: Codex runs commands it classes as known safe (for example `cat`, `ls`, `grep`) without asking, even under the `untrusted` approval policy, and its workspace-write sandbox (Ask every time and Auto edits) restricts writes (to the session directory; `/tmp` and `$TMPDIR` are excluded, `TMPDIR` points inside the session directory) and network, not reads. Text a session reads (for example from a filing) could therefore steer Codex into reading a local file such as a credential file and showing it in the session, with no approval card. Codex 0.160 has no setting to restrict sandbox reads; run sessions you do not watch only on a machine account without sensitive files.
