#!/usr/bin/env bash
# Replays scripts/install.sh against a local fake release (Linux only).
#
#   scripts/test-install.sh [path/to/archivist]
#
# Builds archivist from this checkout (or takes the given binary), lays out
# a fake release <base>/download/<tag>/ (archive plus SHA256SUMS), serves it
# on 127.0.0.1 together with a fake POST /agent-pairing/redeem, and runs
# install.sh under a throwaway HOME with stub systemctl and loginctl first on
# PATH. Nothing touches the real ~/.archivist, ~/.config or service manager.
#
# Asserts: install + pair + unit written; a re-pair saves the new key and
# notes the earlier one (masked only), and --no-service still reinstalls a
# service that is already installed; an update without --pair reinstalls the
# service; a stale brew channel with no installed cask installs directly; a
# failed service install after pairing prints the resume step and exits
# non-zero; a tampered archive exits 1 with nothing installed.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/archivist-install-test.XXXXXX")"
SERVER_PID=""
cleanup() {
  if [ -n "$SERVER_PID" ]; then kill "$SERVER_PID" 2>/dev/null || true; fi
  rm -rf "$WORK"
}
trap cleanup EXIT

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}
pass() {
  printf 'ok: %s\n' "$*"
}

[ "$(uname -s)" = "Linux" ] || { echo "test-install.sh runs on Linux only; skipped"; exit 0; }
case "$(uname -m)" in
  x86_64|amd64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) fail "unsupported architecture $(uname -m)" ;;
esac
command -v python3 >/dev/null 2>&1 || fail "python3 is required"

# --- binary ------------------------------------------------------------------
mkdir -p "$WORK/build"
if [ "$#" -ge 1 ]; then
  cp "$1" "$WORK/build/archivist"
else
  (cd "$ROOT" && go build -o "$WORK/build/archivist" ./cmd/archivist)
fi
chmod 0755 "$WORK/build/archivist"

# --- fake releases -------------------------------------------------------------
# v9.9.9 is good; v9.9.8's archive does not match its SHA256SUMS.
make_release() {
  local tag="$1" tamper="$2" ver dir archive
  ver="${tag#v}"
  dir="$WORK/rel/download/$tag"
  archive="archivist_v${ver}_linux_${ARCH}.tar.gz"
  mkdir -p "$dir"
  tar -czf "$dir/$archive" -C "$WORK/build" archivist
  (cd "$dir" && sha256sum "$archive" > "archivist_v${ver}_SHA256SUMS")
  if [ "$tamper" = 1 ]; then
    printf 'tampered' >> "$dir/$archive"
  fi
}
make_release v9.9.9 0
make_release v9.9.8 1

# --- fake server: static release files plus the redeem route ------------------
cat > "$WORK/server.py" <<'PY'
import json, os, sys
from http.server import SimpleHTTPRequestHandler, ThreadingHTTPServer

root, log_path, port_file = sys.argv[1], sys.argv[2], sys.argv[3]

class Handler(SimpleHTTPRequestHandler):
    def __init__(self, *a, **kw):
        super().__init__(*a, directory=root, **kw)

    def do_POST(self):
        length = int(self.headers.get("content-length", 0))
        body = json.loads(self.rfile.read(length) or b"{}")
        with open(log_path, "a") as f:
            f.write(json.dumps({"path": self.path, "auth": self.headers.get("Authorization"), "body": body}) + "\n")
        if self.path == "/agent-pairing/redeem" and body.get("code") in ("ABCDE12345", "ABCDE12346"):
            status, out = 200, {"key": "ak_replay_" + body["code"].lower() + "x" * 24, "key_id": "ak_id_replay",
                                "name": "archivist replay 2026-10-04 00:00:00Z ab12", "tier": "pro"}
        else:
            status, out = 400, {"error": "This pairing code is not valid or has expired.", "code": "PAIRING_CODE_INVALID"}
        data = json.dumps(out).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def log_message(self, *a):
        pass

server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
with open(port_file, "w") as f:
    f.write(str(server.server_address[1]))
server.serve_forever()
PY
python3 "$WORK/server.py" "$WORK/rel" "$WORK/redeem.log" "$WORK/port" &
SERVER_PID=$!
for _ in $(seq 1 50); do
  [ -s "$WORK/port" ] && break
  sleep 0.1
done
[ -s "$WORK/port" ] || fail "fake release server did not start"
BASE="http://127.0.0.1:$(cat "$WORK/port")"

# --- stub service managers -----------------------------------------------------
# They log every call; STUB_FAIL_ENABLE=1 makes `systemctl --user enable` fail.
mkdir -p "$WORK/stubs"
cat > "$WORK/stubs/systemctl" <<'SH'
#!/bin/sh
echo "systemctl $*" >> "$STUB_LOG"
case "$*" in
  *" enable "*) [ "${STUB_FAIL_ENABLE:-0}" = 1 ] && { echo "Failed to enable unit: stub refusal" >&2; exit 1; } ;;
esac
exit 0
SH
cat > "$WORK/stubs/loginctl" <<'SH'
#!/bin/sh
echo "loginctl $*" >> "$STUB_LOG"
exit 0
SH
chmod 0755 "$WORK/stubs/systemctl" "$WORK/stubs/loginctl"

# run_install HOME VERSION ARGS...: install.sh in a clean environment.
run_install() {
  local home="$1" version="$2"
  shift 2
  mkdir -p "$home"
  env -i \
    HOME="$home" \
    PATH="${EXTRA_PATH:+$EXTRA_PATH:}$WORK/stubs:/usr/local/bin:/usr/bin:/bin" \
    TMPDIR="$WORK" \
    LANG=C.UTF-8 \
    STUB_LOG="$WORK/calls.log" \
    STUB_FAIL_ENABLE="${STUB_FAIL_ENABLE:-0}" \
    ARCHIVIST_RELEASE_BASE_URL="$BASE" \
    ARCHIVIST_INSTALL_VERSION="$version" \
    ARCHIVIST_BASE_URL="$BASE" \
    bash "$ROOT/scripts/install.sh" "$@"
}

# --- 1. install + pair + unit --------------------------------------------------
H1="$WORK/home1"
: > "$WORK/calls.log"
set +e
OUT="$(run_install "$H1" v9.9.9 --pair 'abcde 12345' 2>&1)"
CODE=$?
set -e
[ "$CODE" = 0 ] || fail "install + pair exited $CODE:
$OUT"
[ -x "$H1/.local/bin/archivist" ] || fail "binary not installed"
CRED="$H1/.archivist/credentials"
[ "$(cat "$CRED")" = "ak_replay_abcde12345$(printf 'x%.0s' $(seq 1 24))" ] || fail "credentials not saved"
[ "$(stat -c %a "$CRED")" = 600 ] || fail "credentials mode $(stat -c %a "$CRED")"
UNIT="$H1/.config/systemd/user/archivist-connect.service"
[ -f "$UNIT" ] || fail "unit not written"
grep -q 'connect --service' "$UNIT" || fail "unit does not run connect --service"
if grep -q 'ARCHIVIST_TOKEN\|ak_replay' "$UNIT"; then fail "unit carries a credential"; fi
grep -q 'systemctl --user enable --now archivist-connect.service' "$WORK/calls.log" || fail "service not enabled"
grep -q 'loginctl --no-ask-password enable-linger' "$WORK/calls.log" || fail "linger not attempted with --no-ask-password"
grep -q '"code": "ABCDE12345"' "$WORK/redeem.log" || fail "redeem did not send the normalised code"
if grep -q '"auth": "' "$WORK/redeem.log"; then fail "redeem sent Authorization"; fi
case "$OUT" in *ak_replay_abcde12345x*) fail "the key was printed" ;; esac
case "$OUT" in *"Paired as archivist replay"*) ;; *) fail "no pairing line:
$OUT" ;; esac
pass "install, pair and unit written"

# --- 2. --no-service with the service already installed still reinstalls -----
: > "$WORK/calls.log"
set +e
OUT="$(run_install "$H1" v9.9.9 --pair ABCDE-12346 --no-service 2>&1)"
CODE=$?
set -e
[ "$CODE" = 0 ] || fail "--no-service re-pair exited $CODE:
$OUT"
grep -q 'systemctl --user enable --now archivist-connect.service' "$WORK/calls.log" \
  || fail "--no-service did not restart the installed service"
OLD_KEY="ak_replay_abcde12345$(printf 'x%.0s' $(seq 1 24))"
NEW_KEY="ak_replay_abcde12346$(printf 'x%.0s' $(seq 1 24))"
[ "$(cat "$CRED")" = "$NEW_KEY" ] || fail "re-pair did not save the new key"
case "$OUT" in *"Note: the key saved here before (ak_replay_...xxx) is still active."*) ;; *) fail "no earlier key note:
$OUT" ;; esac
case "$OUT" in *"$OLD_KEY"*|*"$NEW_KEY"*) fail "a full key was printed" ;; esac
pass "re-pair saves the new key, notes the earlier one masked, and --no-service restarts the installed service"

# --- 2b. an update without --pair reinstalls the installed service ------------
: > "$WORK/calls.log"
REDEEMS_BEFORE="$(wc -l < "$WORK/redeem.log")"
set +e
OUT="$(run_install "$H1" v9.9.9 2>&1)"
CODE=$?
set -e
[ "$CODE" = 0 ] || fail "update only exited $CODE:
$OUT"
grep -q 'systemctl --user enable --now archivist-connect.service' "$WORK/calls.log" \
  || fail "update only did not rerun connect --install"
[ "$(wc -l < "$WORK/redeem.log")" = "$REDEEMS_BEFORE" ] || fail "update only called redeem"
[ "$(cat "$CRED")" = "$NEW_KEY" ] || fail "update only changed the saved key"
pass "update without --pair reruns connect --install"

# --- 2c. a stale brew channel with no installed cask installs directly --------
H5="$WORK/home5"
mkdir -p "$H5/.archivist" "$WORK/brewstub"
printf 'brew\n' > "$H5/.archivist/install-channel"
cat > "$WORK/brewstub/brew" <<'SH'
#!/bin/sh
echo "brew $*" >> "$STUB_LOG"
case "$*" in
  "list --cask "*) exit 1 ;;
esac
echo "unexpected brew call: $*" >&2
exit 3
SH
chmod 0755 "$WORK/brewstub/brew"
: > "$WORK/calls.log"
set +e
OUT="$(EXTRA_PATH="$WORK/brewstub" run_install "$H5" v9.9.9 2>&1)"
CODE=$?
set -e
[ "$CODE" = 0 ] || fail "stale brew channel exited $CODE:
$OUT"
grep -q '^brew list --cask archivist$' "$WORK/calls.log" || fail "the cask was not checked"
if grep -q '^brew upgrade' "$WORK/calls.log"; then fail "brew upgrade ran for a missing cask"; fi
[ -x "$H5/.local/bin/archivist" ] || fail "stale brew channel did not install directly"
[ "$(tr -d '[:space:]' < "$H5/.archivist/install-channel")" = curl-sh ] || fail "channel file not rewritten to curl-sh"
pass "stale brew channel without the cask installs directly and records curl-sh"

# --- 3. a failed service install after pairing prints the resume step --------
H3="$WORK/home3"
set +e
OUT="$(STUB_FAIL_ENABLE=1 run_install "$H3" v9.9.9 --pair ABCDE12345 2>&1)"
CODE=$?
set -e
[ "$CODE" != 0 ] || fail "a failed service install exited 0"
[ -s "$H3/.archivist/credentials" ] || fail "pairing was lost"
case "$OUT" in *"connect --install (no new code is needed)"*) ;; *) fail "no resume step:
$OUT" ;; esac
pass "failed service install after pairing prints the resume step"

# --- 4. tampered archive: exit 1, nothing installed ---------------------------
H4="$WORK/home4"
set +e
OUT="$(run_install "$H4" v9.9.8 --pair ABCDE12345 2>&1)"
CODE=$?
set -e
[ "$CODE" = 1 ] || fail "tampered archive exited $CODE:
$OUT"
case "$OUT" in *"checksum mismatch"*) ;; *) fail "no checksum mismatch message:
$OUT" ;; esac
[ ! -e "$H4/.local" ] || fail "tampered install left ~/.local"
[ ! -e "$H4/.archivist" ] || fail "tampered install left ~/.archivist"
if ls "$WORK"/archivist-install.* >/dev/null 2>&1; then fail "temporary directory left behind"; fi
pass "tampered archive aborts with nothing installed"

echo "install.sh replay: all checks passed"
