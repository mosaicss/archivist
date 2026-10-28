#!/usr/bin/env bash
# Replays scripts/install.sh against a local fake release (Linux only).
#
#   scripts/test-install.sh [path/to/archivist]
#
# Builds archivist from this checkout (or takes the given binary), lays out
# fake releases <base>/download/<tag>/ (archive, skill bundle, SHA256SUMS and
# its signature by a throwaway test key), serves them on 127.0.0.1 together
# with fake pairing routes, and runs a copy of install.sh whose release key
# block holds the test key, under a throwaway HOME with stub systemctl and
# loginctl first on PATH. Pairing always takes the browser confirmation
# (Story 94.15; BROWSER is a stand in that confirms the pending pairing, as
# one Confirm click would) and the replay needs no terminal. Nothing touches
# the real ~/.archivist, ~/.config or service manager.
#
# Asserts: install + pair + unit written (with no terminal to ask, the start
# question takes its default, said in one line); a re-pair saves the new key and
# revokes the earlier one (masked only), and --no-service still reinstalls a
# service that is already installed; an update without --pair reinstalls the
# service; a stale brew channel with no installed cask installs directly; a
# failed service install after pairing prints the resume step and exits
# non-zero; a service that is not running after pairing points at the
# Service line and the rerun without --pair (Story 94.14), and a re-pair
# prints no restart hint; the skill bundle is installed when ~/.claude/skills exists. Each
# of these exits non-zero with nothing installed and no temporary directory
# left: an archive that does not match its checksum; checksums without a
# valid signature; a release without the signature; one
# signed by another key; the unmodified install.sh (release key) against the
# test signed release; a skill bundle that does not match; a plain http://
# release URL that is not loopback (refused before any download); and no
# openssl on PATH. On Linux without bwrap or socat the install prints one
# line naming the missing packages and the apt-get command (both missing, or
# only socat), none when both are present, and the exit code stays 0. Story
# 94.6: connect --uninstall revokes the saved key (DELETE
# /account/cli-tokens/current with it), turns off the lingering Archivist
# turned on (stub loginctl disable-linger), and removes the unit, the
# credentials, ~/.archivist/connect and the session work folders.

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
command -v openssl >/dev/null 2>&1 || fail "openssl is required"

# --- binary ------------------------------------------------------------------
mkdir -p "$WORK/build"
if [ "$#" -ge 1 ]; then
  cp "$1" "$WORK/build/archivist"
else
  (cd "$ROOT" && go build -o "$WORK/build/archivist" ./cmd/archivist)
fi
chmod 0755 "$WORK/build/archivist"

# --- test keys and the installer under test -------------------------------------
# Throwaway keys: test.key signs the fake releases, other.key is not trusted.
mkdir -p "$WORK/keys"
for k in test other; do
  openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:3072 -out "$WORK/keys/$k.key" 2>/dev/null \
    || fail "openssl could not create a test key"
done
openssl pkey -in "$WORK/keys/test.key" -pubout -out "$WORK/keys/test.pub" 2>/dev/null

# install.sh with the lines between its release key markers replaced by
# RELEASE_KEY='<test public key>'.
if [ "$(grep -cx '# BEGIN ARCHIVIST RELEASE KEY' "$ROOT/scripts/install.sh")" != 1 ] \
  || [ "$(grep -cx '# END ARCHIVIST RELEASE KEY' "$ROOT/scripts/install.sh")" != 1 ]; then
  fail "install.sh has no single release key block"
fi
awk -v keyfile="$WORK/keys/test.pub" '
  $0 == "# BEGIN ARCHIVIST RELEASE KEY" {
    print
    key = ""
    while ((getline line < keyfile) > 0) { key = key (key == "" ? "" : "\n") line }
    printf "RELEASE_KEY=\047%s\047\n", key
    skip = 1
    next
  }
  $0 == "# END ARCHIVIST RELEASE KEY" { skip = 0 }
  !skip { print }
' "$ROOT/scripts/install.sh" > "$WORK/install.sh"
grep -qF "$(sed -n 2p "$WORK/keys/test.pub")" "$WORK/install.sh" || fail "the test key was not patched in"
INSTALLER="$WORK/install.sh"

# --- fake releases -------------------------------------------------------------
# make_release TAG MODE. MODE good: archive, skill bundle, SHA256SUMS and its
# signature by the test key. mismatch: the archive does not match
# SHA256SUMS. unsigned: SHA256SUMS (matching the archive) differs from the
# signed one. nosig: no signature. otherkey: signed by other.key.
# badskill: the skill bundle does not match SHA256SUMS.
make_release() {
  local tag="$1" mode="$2" ver dir archive sums skill
  ver="${tag#v}"
  dir="$WORK/rel/download/$tag"
  archive="archivist_v${ver}_linux_${ARCH}.tar.gz"
  sums="archivist_v${ver}_SHA256SUMS"
  skill="archivist_v${ver}_skill-bundle.tar.gz"
  mkdir -p "$dir" "$WORK/skill-$tag"
  tar -czf "$dir/$archive" -C "$WORK/build" archivist
  printf -- '---\nname: archivist\n---\n' > "$WORK/skill-$tag/SKILL.md"
  tar -czf "$dir/$skill" -C "$WORK/skill-$tag" SKILL.md
  (cd "$dir" && sha256sum "$archive" "$skill" > "$sums")
  local key="$WORK/keys/test.key"
  [ "$mode" = otherkey ] && key="$WORK/keys/other.key"
  [ "$mode" = nosig ] || openssl dgst -sha256 -sign "$key" -out "$dir/$sums.sig" "$dir/$sums"
  case "$mode" in
    mismatch) printf 'changed' >> "$dir/$archive" ;;
    badskill) printf 'changed' >> "$dir/$skill" ;;
    unsigned)
      printf 'extra\n' > "$WORK/build/EXTRA"
      tar -czf "$dir/$archive" -C "$WORK/build" archivist EXTRA
      rm -f "$WORK/build/EXTRA"
      (cd "$dir" && sha256sum "$archive" "$skill" > "$sums")
      ;;
  esac
}
make_release v9.9.9 good
make_release v9.9.8 mismatch
make_release v9.9.7 unsigned
make_release v9.9.6 nosig
make_release v9.9.5 otherkey
make_release v9.9.4 badskill

# --- fake server: static release files plus the pairing routes ----------------
# Story 94.3: a browser mode redeem answers 202 pending; the BROWSER stand in
# below confirms it (as a Confirm click in a browser signed in to the code's
# account would) and the poll then brings the key once. Any other redeem gets
# the 426 Mosaic answers after the cutoff (Story 94.15). The self revoke and
# the key list answer too, so a re-pair can revoke the key it replaced.
cat > "$WORK/server.py" <<'PY'
import json, os, secrets, sys, threading
from http.server import SimpleHTTPRequestHandler, ThreadingHTTPServer

root, log_path, port_file = sys.argv[1], sys.argv[2], sys.argv[3]
# Story 94.3: pending browser confirmations by confirm_id.
pending, lock = {}, threading.Lock()

def key_for(code):
    return "ak_replay_" + code.lower() + "x" * 24

class Handler(SimpleHTTPRequestHandler):
    def __init__(self, *a, **kw):
        super().__init__(*a, directory=root, **kw)

    def log(self, body):
        with open(log_path, "a") as f:
            f.write(json.dumps({"method": self.command, "path": self.path, "auth": self.headers.get("Authorization"), "body": body}) + "\n")

    def answer(self, status, out=None):
        data = b"" if out is None else json.dumps(out).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_POST(self):
        length = int(self.headers.get("content-length", 0))
        body = json.loads(self.rfile.read(length) or b"{}")
        self.log(body)
        if self.path == "/agent-pairing/redeem" and body.get("code") in ("ABCDE12345", "ABCDE12346"):
            if body.get("confirm") == "browser":
                confirm_id, poll_token = secrets.token_urlsafe(32), secrets.token_urlsafe(32)
                with lock:
                    pending[confirm_id] = {"poll_token": poll_token, "code": body["code"], "state": "pending"}
                port = self.server.server_address[1]
                return self.answer(202, {"pending": True, "email": "replay@example.test", "confirm_id": confirm_id,
                                         "poll_token": poll_token,
                                         "confirm_url": "http://127.0.0.1:%d/connect/confirm#%s" % (port, confirm_id),
                                         "expires_in_seconds": 60, "poll_interval_seconds": 1})
            return self.answer(426, {"error": "This version of Archivist can no longer pair.", "code": "PAIRING_UPDATE_REQUIRED"})
        if self.path == "/test/confirm":
            # The BROWSER stand in: the confirm page of a browser signed in to the code's account.
            with lock:
                p = pending.get(body.get("confirm_id"))
                if p and p["state"] == "pending":
                    p["state"] = "confirmed"
            return self.answer(204)
        if self.path == "/agent-pairing/pending/poll":
            with lock:
                p = pending.get(body.get("confirm_id"))
                if not p or p["poll_token"] != body.get("poll_token"):
                    return self.answer(400, {"error": "This pairing code is not valid or has expired.", "code": "PAIRING_CODE_INVALID"})
                state = p["state"]
                if state == "confirmed":
                    p["state"] = "completed"
            if state == "confirmed":
                return self.answer(200, {"state": "confirmed", "key": key_for(p["code"]), "key_id": "ak_id_replay",
                                         "name": "archivist replay 2026-10-04 00:00:00Z ab12", "tier": "pro",
                                         "email": "replay@example.test"})
            return self.answer(200, {"state": "pending" if state == "pending" else "expired"})
        if self.path == "/agent-pairing/pending/decline":
            return self.answer(204)
        return self.answer(400, {"error": "This pairing code is not valid or has expired.", "code": "PAIRING_CODE_INVALID"})

    def do_DELETE(self):
        self.log(None)
        if self.path == "/account/cli-tokens/current" and (self.headers.get("Authorization") or "").startswith("Bearer ak_"):
            return self.answer(204)
        return self.answer(404, {"error": "Not found", "code": "NOT_FOUND"})

    def do_GET(self):
        if self.path == "/account/cli-tokens":
            return self.answer(200, {"user_email": "replay@example.test", "tier": "pro", "tokens": []})
        return super().do_GET()

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
# They log every call; STUB_FAIL_ENABLE=1 makes `systemctl --user enable` fail
# and STUB_INACTIVE=1 makes `systemctl --user is-active` say not running.
mkdir -p "$WORK/stubs"
cat > "$WORK/stubs/systemctl" <<'SH'
#!/bin/sh
echo "systemctl $*" >> "$STUB_LOG"
case "$*" in
  *" enable "*) [ "${STUB_FAIL_ENABLE:-0}" = 1 ] && { echo "Failed to enable unit: stub refusal" >&2; exit 1; } ;;
  *" is-active "*) [ "${STUB_INACTIVE:-0}" = 1 ] && exit 3 ;;
esac
exit 0
SH
cat > "$WORK/stubs/loginctl" <<'SH'
#!/bin/sh
echo "loginctl $*" >> "$STUB_LOG"
exit 0
SH
# The browser: confirms the pending pairing named by the URL's fragment.
cat > "$WORK/stubs/browser-confirm" <<'SH'
#!/usr/bin/env python3
import json, sys, urllib.parse, urllib.request
url = urllib.parse.urlsplit(sys.argv[1])
with open(sys.argv[0] + ".log", "a") as f:
    f.write(url.path + "\n")
req = urllib.request.Request("%s://%s/test/confirm" % (url.scheme, url.netloc),
                             data=json.dumps({"confirm_id": url.fragment}).encode(),
                             headers={"Content-Type": "application/json"}, method="POST")
urllib.request.urlopen(req, timeout=10).read()
SH
chmod 0755 "$WORK/stubs/systemctl" "$WORK/stubs/loginctl" "$WORK/stubs/browser-confirm"

# run_install HOME VERSION ARGS...: the installer under test in a clean
# environment. RUN_PATH replaces PATH, RELEASE_URL the release base URL and
# RUN_INSTALLER the script.
run_install() {
  local home="$1" version="$2"
  shift 2
  mkdir -p "$home"
  env -i \
    HOME="$home" \
    PATH="${RUN_PATH:-${EXTRA_PATH:+$EXTRA_PATH:}$WORK/stubs:/usr/local/bin:/usr/bin:/bin}" \
    TMPDIR="$WORK" \
    LANG=C.UTF-8 \
    STUB_LOG="$WORK/calls.log" \
    STUB_FAIL_ENABLE="${STUB_FAIL_ENABLE:-0}" \
    STUB_INACTIVE="${STUB_INACTIVE:-0}" \
    ARCHIVIST_RELEASE_BASE_URL="${RELEASE_URL:-$BASE}" \
    ARCHIVIST_INSTALL_VERSION="$version" \
    ARCHIVIST_BASE_URL="$BASE" \
    BROWSER="$WORK/stubs/browser-confirm" \
    "$BASH" "${RUN_INSTALLER:-$INSTALLER}" "$@"
}

# assert_refused CASE HOME CODE OUT MESSAGE: exit 1, MESSAGE printed, nothing
# installed and no temporary directory left.
assert_refused() {
  local name="$1" home="$2" code="$3" out="$4" message="$5"
  [ "$code" = 1 ] || fail "${name} exited ${code}:
${out}"
  case "$out" in *"$message"*) ;; *) fail "${name}: no '${message}' message:
${out}" ;; esac
  [ ! -e "$home/.local" ] || fail "${name} left ~/.local"
  [ ! -e "$home/.archivist" ] || fail "${name} left ~/.archivist"
  [ ! -e "$home/.claude/skills/archivist" ] || fail "${name} installed the skill"
  if ls "$WORK"/archivist-install.* >/dev/null 2>&1; then fail "${name} left a temporary directory"; fi
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
grep -q '"confirm": "browser"' "$WORK/redeem.log" || fail "redeem did not ask for the browser confirmation"
grep -q '/agent-pairing/pending/poll' "$WORK/redeem.log" || fail "the pending pairing was not polled"
grep -q '^/connect/confirm$' "$WORK/stubs/browser-confirm.log" || fail "the confirm page was not opened"
case "$OUT" in *"This computer will connect to replay@example.test. Click Confirm on the Mosaic page"*) ;; *) fail "no account line:
$OUT" ;; esac
case "$OUT" in *"Open this link in a browser signed in to that account, on this or any other device: http://127.0.0.1:"*"/connect/confirm#"*) ;; *) fail "the confirm link was not printed:
$OUT" ;; esac
if grep '"path": "/agent-pairing/' "$WORK/redeem.log" | grep -q '"auth": "'; then fail "pairing sent Authorization"; fi
case "$OUT" in *ak_replay_abcde12345x*) fail "the key was printed" ;; esac
case "$OUT" in *"Paired as archivist replay"*) ;; *) fail "no pairing line:
$OUT" ;; esac
case "$OUT" in *"Download verified."*) ;; *) fail "no verified line:
$OUT" ;; esac
case "$OUT" in *"Starting Archivist when your computer starts (the default)."*) ;; *) fail "no default start line:
$OUT" ;; esac
case "$OUT" in *"Service:  "*) ;; *) fail "connect --status did not run:
$OUT" ;; esac
pass "install, pair and unit written; no terminal takes the start default"

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
case "$OUT" in *"Start Archivist when"*|*"(the default)"*) fail "an installed service was asked about again:
$OUT" ;; esac
OLD_KEY="ak_replay_abcde12345$(printf 'x%.0s' $(seq 1 24))"
NEW_KEY="ak_replay_abcde12346$(printf 'x%.0s' $(seq 1 24))"
[ "$(cat "$CRED")" = "$NEW_KEY" ] || fail "re-pair did not save the new key"
case "$OUT" in *"The key saved here before (ak_replay_...xxx) no longer works."*) ;; *) fail "earlier key not revoked:
$OUT" ;; esac
grep '"method": "DELETE", "path": "/account/cli-tokens/current"' "$WORK/redeem.log" | grep -q "\"auth\": \"Bearer $OLD_KEY\"" \
  || fail "the earlier key did not revoke itself"
if grep '"method": "DELETE"' "$WORK/redeem.log" | grep -q "$NEW_KEY"; then fail "the new key was revoked"; fi
case "$OUT" in *"$OLD_KEY"*|*"$NEW_KEY"*) fail "a full key was printed" ;; esac
# Story 94.14: the installer reinstalls the service next, so the re-pair's
# own restart hint is not printed.
case "$OUT" in *"to restart background connect with the new key"*) fail "the redundant restart hint was printed:
$OUT" ;; esac
pass "re-pair saves the new key, revokes the earlier one (masked), --no-service restarts the installed service, no restart hint"

# --- 2b. an update without --pair reinstalls the installed service ------------
: > "$WORK/calls.log"
REDEEMS_BEFORE="$(grep -c '"path": "/agent-pairing/redeem"' "$WORK/redeem.log")"
set +e
OUT="$(run_install "$H1" v9.9.9 2>&1)"
CODE=$?
set -e
[ "$CODE" = 0 ] || fail "update only exited $CODE:
$OUT"
grep -q 'systemctl --user enable --now archivist-connect.service' "$WORK/calls.log" \
  || fail "update only did not rerun connect --install"
[ "$(grep -c '"path": "/agent-pairing/redeem"' "$WORK/redeem.log")" = "$REDEEMS_BEFORE" ] || fail "update only called redeem"
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

# --- 3b. a service that is not running after pairing (Story 94.14) ----------
H3B="$WORK/home3b"
set +e
OUT="$(STUB_INACTIVE=1 run_install "$H3B" v9.9.9 --pair ABCDE12345 2>&1)"
CODE=$?
set -e
[ "$CODE" = 1 ] || fail "a service that is not running exited $CODE:
$OUT"
[ -s "$H3B/.archivist/credentials" ] || fail "pairing was lost"
for want in "Service:  installed, not running" "the Service line above says why" \
  "run this again without --pair (no new code is needed)" "curl -fsSL ${BASE}/latest/download/install.sh | bash"; do
  case "$OUT" in *"$want"*) ;; *) fail "not running: no '${want}':
$OUT" ;; esac
done
case "$OUT" in *"last log line"*) fail "not running: a log line is named:
$OUT" ;; esac
set +e
OUT="$(STUB_INACTIVE=1 run_install "$H3B" v9.9.9 2>&1)"
CODE=$?
set -e
[ "$CODE" = 1 ] || fail "an update with a service that is not running exited $CODE:
$OUT"
case "$OUT" in *"Once that is fixed, run this command again."*) ;; *) fail "update not running: no rerun line:
$OUT" ;; esac
case "$OUT" in *"without --pair"*) fail "update not running: asked to drop a --pair it did not have:
$OUT" ;; esac
pass "a service that is not running after pairing points at the Service line and the rerun without --pair"

# --- 4. an archive that does not match its checksum: exit 1, nothing installed
H4="$WORK/home4"
set +e
OUT="$(run_install "$H4" v9.9.8 --pair ABCDE12345 2>&1)"
CODE=$?
set -e
[ "$CODE" = 1 ] || fail "a mismatched archive exited $CODE:
$OUT"
case "$OUT" in *"checksum mismatch"*) ;; *) fail "no checksum mismatch message:
$OUT" ;; esac
[ ! -e "$H4/.local" ] || fail "a mismatched archive left ~/.local"
[ ! -e "$H4/.archivist" ] || fail "a mismatched archive left ~/.archivist"
if ls "$WORK"/archivist-install.* >/dev/null 2>&1; then fail "temporary directory left behind"; fi
pass "an archive that does not match its checksum aborts with nothing installed"

# --- 5. the skill bundle lands in ~/.claude/skills/archivist ------------------
H6="$WORK/home6"
mkdir -p "$H6/.claude/skills"
set +e
OUT="$(run_install "$H6" v9.9.9 2>&1)"
CODE=$?
set -e
[ "$CODE" = 0 ] || fail "install with ~/.claude/skills exited $CODE:
$OUT"
[ -f "$H6/.claude/skills/archivist/SKILL.md" ] || fail "SKILL.md not installed:
$OUT"
pass "the skill bundle is verified and installed"

# --- 6. refusals: nothing installed, no temporary directory --------------------
refuse() {
  local name="$1" version="$2" message="$3" home
  home="$WORK/refuse-$(printf '%s' "$name" | tr -c 'a-z0-9' '-')"
  mkdir -p "$home/.claude/skills"
  set +e
  OUT="$(run_install "$home" "$version" --pair ABCDE12345 2>&1)"
  CODE=$?
  set -e
  assert_refused "$name" "$home" "$CODE" "$OUT" "$message"
  pass "$name is refused with nothing installed"
}
refuse "a release whose checksums lack a valid signature" v9.9.7 "the release checksums could not be verified"
refuse "a release without a signature" v9.9.6 "download failed"
refuse "a release signed by another key" v9.9.5 "the release checksums could not be verified"
refuse "a skill bundle that does not match" v9.9.4 "checksum mismatch for archivist_v9.9.4_skill-bundle.tar.gz"
RUN_INSTALLER="$ROOT/scripts/install.sh" refuse "the release key installer against a test signed release" v9.9.9 \
  "the release checksums could not be verified"

# --- 7. a plain http:// release URL that is not loopback, before any download --
mkdir -p "$WORK/curlstub"
cat > "$WORK/curlstub/curl" <<'SH'
#!/bin/sh
echo "curl $*" >> "$STUB_LOG"
exit 7
SH
chmod 0755 "$WORK/curlstub/curl"
for url in http://example.com/releases http://127.0.0.1.example.com http://localhost@example.com \
  https://user@example.com/releases ftp://example.com HTTP://127.0.0.1 http:///releases; do
  H7="$WORK/home7"
  rm -rf "$H7"
  : > "$WORK/calls.log"
  set +e
  OUT="$(EXTRA_PATH="$WORK/curlstub" RELEASE_URL="$url" run_install "$H7" v9.9.9 2>&1)"
  CODE=$?
  set -e
  assert_refused "release URL ${url}" "$H7" "$CODE" "$OUT" "ARCHIVIST_RELEASE_BASE_URL must"
  if grep -q '^curl' "$WORK/calls.log"; then fail "release URL ${url} reached curl"; fi
done
# Loopback http:// stays allowed (every case above uses it).
pass "non loopback http:// and malformed release URLs are refused before any download"

# --- 7b. an https:// release URL: curl is limited to https, redirects too -----
H7="$WORK/home7"
rm -rf "$H7"
: > "$WORK/calls.log"
set +e
OUT="$(EXTRA_PATH="$WORK/curlstub" RELEASE_URL="https://releases.example.invalid/archivist" run_install "$H7" v9.9.9 2>&1)"
CODE=$?
set -e
assert_refused "the stubbed https:// download" "$H7" "$CODE" "$OUT" "download failed"
grep -q '^curl .*--proto =https --proto-redir =https' "$WORK/calls.log" \
  || fail "curl was not limited to https:
$(cat "$WORK/calls.log")"
if grep -q '=http,https' "$WORK/calls.log"; then fail "curl allowed http for an https:// release URL"; fi
pass "an https:// release URL limits curl and its redirects to https"

# --- 8. no openssl on PATH --------------------------------------------------------
mkdir -p "$WORK/noopenssl"
for tool in uname mktemp rm tr curl awk head sha256sum tar gzip chmod mkdir cp mv dirname cat; do
  ln -s "$(command -v "$tool")" "$WORK/noopenssl/$tool"
done
H8="$WORK/home8"
set +e
OUT="$(RUN_PATH="$WORK/stubs:$WORK/noopenssl" run_install "$H8" v9.9.9 2>&1)"
CODE=$?
set -e
assert_refused "no openssl" "$H8" "$CODE" "$OUT" "openssl is needed to check the download"
pass "no openssl on PATH is refused with nothing installed"

# --- 9. Linux packages for archivist connect: one line when missing ---------
# A PATH with the install's tools and openssl but no bwrap or socat, plus a
# logging apt-get stub (never run).
mkdir -p "$WORK/nopkgs" "$WORK/aptstub" "$WORK/pkgstub"
for tool in uname mktemp rm tr curl awk head sha256sum tar gzip chmod mkdir cp mv dirname cat openssl id; do
  ln -s "$(command -v "$tool")" "$WORK/nopkgs/$tool"
done
for stub in "$WORK/aptstub/apt-get" "$WORK/pkgstub/bwrap" "$WORK/pkgstub/socat"; do
  # shellcheck disable=SC2016 # $* and $STUB_LOG expand when the stub runs
  printf '#!/bin/sh\necho "%s $*" >> "$STUB_LOG"\nexit 0\n' "$(basename "$stub")" > "$stub"
  chmod 0755 "$stub"
done
# install.sh drops sudo when it runs as root.
SUDO="sudo "
[ "$(id -u)" = 0 ] && SUDO=""
PKG_LINE="Archivist connect on Linux also needs bubblewrap and socat. Install with: ${SUDO}apt-get install -y bubblewrap socat"
H9="$WORK/home9"
: > "$WORK/calls.log"
set +e
OUT="$(RUN_PATH="$WORK/stubs:$WORK/aptstub:$WORK/nopkgs" run_install "$H9" v9.9.9 2>&1)"
CODE=$?
set -e
[ "$CODE" = 0 ] || fail "install without bwrap and socat exited $CODE:
$OUT"
[ -x "$H9/.local/bin/archivist" ] || fail "install without bwrap and socat did not install"
case "$OUT" in *"$PKG_LINE"*) ;; *) fail "no package line:
$OUT" ;; esac
[ "$(printf '%s\n' "$OUT" | grep -c 'also needs')" = 1 ] || fail "the package line is not printed once:
$OUT"
if grep -q '^apt-get' "$WORK/calls.log"; then fail "apt-get was run"; fi
pass "missing bwrap and socat print the apt-get line, exit 0"

H10="$WORK/home10"
set +e
OUT="$(RUN_PATH="$WORK/stubs:$WORK/aptstub:$WORK/pkgstub:$WORK/nopkgs" run_install "$H10" v9.9.9 2>&1)"
CODE=$?
set -e
[ "$CODE" = 0 ] || fail "install with bwrap and socat exited $CODE:
$OUT"
case "$OUT" in *"also needs"*) fail "a package line with bwrap and socat present:
$OUT" ;; esac
pass "bwrap and socat present print no package line"

mkdir -p "$WORK/bwrapstub"
cp "$WORK/pkgstub/bwrap" "$WORK/bwrapstub/bwrap"
H11="$WORK/home11"
set +e
OUT="$(RUN_PATH="$WORK/stubs:$WORK/aptstub:$WORK/bwrapstub:$WORK/nopkgs" run_install "$H11" v9.9.9 2>&1)"
CODE=$?
set -e
[ "$CODE" = 0 ] || fail "install without socat exited $CODE:
$OUT"
SOCAT_LINE="Archivist connect on Linux also needs socat. Install with: ${SUDO}apt-get install -y socat"
[ "$(printf '%s\n' "$OUT" | grep -c 'also needs')" = 1 ] || fail "the package line is not printed once:
$OUT"
printf '%s\n' "$OUT" | grep -qxF "$SOCAT_LINE" || fail "no socat only line:
$OUT"
pass "only socat missing prints the socat line, exit 0"

# --- 10. connect --uninstall cleans this computer up (Story 94.6) -------------
# run_bin HOME ARGS...: the installed binary in the replay environment.
run_bin() {
  local home="$1"
  shift
  env -i \
    HOME="$home" \
    PATH="$WORK/stubs:/usr/local/bin:/usr/bin:/bin" \
    TMPDIR="$WORK" \
    LANG=C.UTF-8 \
    STUB_LOG="$WORK/calls.log" \
    ARCHIVIST_BASE_URL="$BASE" \
    "$home/.local/bin/archivist" "$@"
}
# Archivist turned lingering on (the marker install writes; set here so the
# replay does not depend on the host's own linger state).
printf 'archivist\n' > "$H1/.archivist/connect/linger"
mkdir -p "$WORK/archivist-connect-replay1/src"
: > "$WORK/calls.log"
DELETES_BEFORE="$(grep -c '"method": "DELETE"' "$WORK/redeem.log" || true)"
set +e
OUT="$(run_bin "$H1" connect --uninstall 2>&1)"
CODE=$?
set -e
[ "$CODE" = 0 ] || fail "uninstall exited $CODE:
$OUT"
[ "$(grep -c '"method": "DELETE"' "$WORK/redeem.log")" = "$((DELETES_BEFORE + 1))" ] || fail "uninstall did not revoke once"
grep '"method": "DELETE", "path": "/account/cli-tokens/current"' "$WORK/redeem.log" | tail -1 | grep -q "\"auth\": \"Bearer $NEW_KEY\"" \
  || fail "uninstall did not revoke the saved key with itself"
grep -q 'systemctl --user disable --now archivist-connect.service' "$WORK/calls.log" || fail "service not disabled"
grep -q 'loginctl --no-ask-password disable-linger' "$WORK/calls.log" || fail "lingering not turned off"
for left in "$UNIT" "$CRED" "$H1/.archivist/connect" "$WORK/archivist-connect-replay1"; do
  [ ! -e "$left" ] || fail "uninstall left $left"
done
case "$OUT" in *"$NEW_KEY"*) fail "uninstall printed the key" ;; esac
case "$OUT" in *"no longer works"*) ;; *) fail "no revoke line:
$OUT" ;; esac
pass "uninstall revokes the key, turns off lingering and removes the unit, credentials, connect files and work folders"

: > "$WORK/calls.log"
set +e
OUT="$(run_bin "$H1" connect --uninstall 2>&1)"
CODE=$?
set -e
[ "$CODE" = 0 ] || fail "second uninstall exited $CODE:
$OUT"
case "$OUT" in *"Background connect is not installed."*) ;; *) fail "second uninstall:
$OUT" ;; esac
[ "$(grep -c '"method": "DELETE"' "$WORK/redeem.log")" = "$((DELETES_BEFORE + 1))" ] || fail "a second uninstall revoked again"
if grep -q 'loginctl' "$WORK/calls.log"; then fail "a second uninstall touched lingering"; fi
pass "a second uninstall says nothing is installed and revokes nothing"

echo "install.sh replay: all checks passed"
