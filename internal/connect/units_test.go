package connect

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mosaicss/archivist/internal/client"
	"github.com/mosaicss/archivist/internal/mosaicevent"
)

// ─── child environment ──────────────────────────────────────────────────────

func TestBuildChildEnvAllowlist(t *testing.T) {
	parent := []string{
		"HOME=/h", "PATH=/bin", "USER=u", "LOGNAME=u", "SHELL=/bin/sh", "LANG=C", "TERM=xterm", "TMPDIR=/t",
		"LC_ALL=C", "XDG_CONFIG_HOME=/x",
		"ANTHROPIC_API_KEY=k", "ANTHROPIC_BASE_URL=u", "CLAUDE_CODE_OAUTH_TOKEN=o", "CLAUDE_CONFIG_DIR=/c",
		"OPENAI_API_KEY=k", "CODEX_HOME=/c", "ARCHIVIST_TOKEN=ak_x", "HERDR_ENV=1", "AWS_SECRET_ACCESS_KEY=s",
		"GITHUB_TOKEN=g", "NODE_OPTIONS=--require x", "LD_PRELOAD=/evil.so", "=broken", "NOEQUALS",
	}
	env, err := BuildChildEnv(parent, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := "HOME,LANG,LC_ALL,LOGNAME,PATH,SHELL,TERM,TMPDIR,USER,XDG_CONFIG_HOME"
	if got := strings.Join(EnvKeys(env), ","); got != want {
		t.Fatalf("child keys %s, want %s", got, want)
	}
	for _, bad := range []string{"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "ARCHIVIST_TOKEN", "GITHUB_TOKEN"} {
		if _, err := BuildChildEnv(nil, map[string]string{bad: "v"}); err == nil {
			t.Errorf("override %s accepted", bad)
		}
	}
	// Deny prefixes beat allow prefixes even for override keys that look allowed.
	if EnvAllowed("CLAUDE_HOME") || EnvAllowed("XDG_X") == false {
		t.Fatal("deny/allow precedence wrong")
	}
	env, err = BuildChildEnv(parent, map[string]string{"TMPDIR": "/session"})
	if err != nil || !contains(env, "TMPDIR=/session") {
		t.Fatalf("override TMPDIR: %v %v", env, err)
	}
}

// CLAUDE_CONFIG_DIR passes only as an absolute path to an existing
// directory, and never as an override; every other CLAUDE_ key stays denied.
func TestBuildChildEnvClaudeConfigDir(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cases := map[string]bool{
		dir:                          true,
		"relative/dir":               false,
		"":                           false,
		filepath.Join(dir, "absent"): false,
		file:                         false,
	}
	for v, want := range cases {
		env, err := BuildChildEnv([]string{"HOME=/h", "CLAUDE_CONFIG_DIR=" + v, "CLAUDE_CODE_OAUTH_TOKEN=o", "CLAUDE_HOME=/x"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got := contains(env, "CLAUDE_CONFIG_DIR="+v); got != want {
			t.Errorf("%q: passed %v, want %v (%v)", v, got, want, env)
		}
		for _, kv := range env {
			if strings.HasPrefix(kv, "CLAUDE_") && !strings.HasPrefix(kv, "CLAUDE_CONFIG_DIR=") {
				t.Errorf("denied key passed: %s", kv)
			}
		}
	}
	if _, err := BuildChildEnv(nil, map[string]string{"CLAUDE_CONFIG_DIR": dir}); err == nil {
		t.Fatal("CLAUDE_CONFIG_DIR override accepted")
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// ─── redaction ──────────────────────────────────────────────────────────────

func TestScrub(t *testing.T) {
	ticket := "eyJ2IjoxLCJzdWIiOiJ1c2VyX3gifQ." + strings.Repeat("A", 43)
	in := strings.Join([]string{
		"auth Bearer abc.def-ghi",
		"key ak_live_0123456789abcdef",
		"legacy mc_pat_secretsecret",
		"task mst_0f8fad5b-d9cb-469f-a165-70867728950e.AbCdEfGhIjKlMnOpQrStUvWxYz0123456789_-abcde",
		"anthropic sk-ant-api03-XYZ_123",
		"ticket " + ticket,
	}, "\n")
	out := Scrub(in)
	for _, secret := range []string{"abc.def-ghi", "0123456789abcdef", "secretsecret", "AbCdEfGh", "XYZ_123", ticket} {
		if strings.Contains(out, secret) {
			t.Errorf("scrub kept %q:\n%s", secret, out)
		}
	}
	if !strings.Contains(out, "[redacted fp:") {
		t.Errorf("archivist credentials lost their fingerprint:\n%s", out)
	}
}

// ─── closed command set ─────────────────────────────────────────────────────

const unitSID = "0f8fad5b-d9cb-469f-a165-70867728950e"

func TestDecodeClosedSet(t *testing.T) {
	ok := []struct {
		socket Socket
		raw    string
		kind   string
	}{
		{UserSocket, `{"kind":"start_session","correlationId":"c1","sessionId":"` + unitSID + `","agent":"claude","prompt":"hi"}`, "start_session"},
		{UserSocket, `{"kind":"presence","online":true,"agents":[]}`, "presence"},
		{UserSocket, `{"kind":"error","code":"UNKNOWN_ACK"}`, "error"},
		{UserSocket, `{"kind":"approval_response","correlationId":"resolved:r1","sessionId":"` + unitSID + `","approvalId":"a","decision":"deny","reason":"timeout","terminalReceipt":true}`, "approval_response"},
		{SessionSocket, `{"kind":"user_message","correlationId":"c2","sessionId":"` + unitSID + `","text":"hi"}`, "user_message"},
		{SessionSocket, `{"kind":"interrupt","correlationId":"c3","sessionId":"` + unitSID + `"}`, "interrupt"},
		{SessionSocket, `{"kind":"stop_session","correlationId":"c4","sessionId":"` + unitSID + `"}`, "stop_session"},
		{SessionSocket, `{"kind":"approval_response","correlationId":"resolved:r2","sessionId":"` + unitSID + `","approvalId":"a","decision":"allow","reason":"user","scope":"allow_always"}`, "approval_response"},
		{SessionSocket, `{"kind":"ack","correlationId":"run:1","seq":4,"duplicate":false}`, "ack"},
	}
	for _, c := range ok {
		in, err := Decode(c.socket, []byte(c.raw))
		if err != nil || in.Kind != c.kind {
			t.Errorf("%s: %v %+v", c.raw, err, in)
		}
	}
	refused := []struct {
		socket Socket
		raw    string
		ackCID string
	}{
		{UserSocket, `{"kind":"exec","correlationId":"c1","sessionId":"` + unitSID + `","binary":"/bin/sh"}`, "c1"},
		{UserSocket, `{"kind":"start_session","correlationId":"c1","sessionId":"` + unitSID + `","agent":"claude","prompt":"hi","binary":"/bin/sh"}`, "c1"},
		{UserSocket, `{"kind":"start_session","correlationId":"c1","sessionId":"` + unitSID + `","agent":"claude","prompt":"hi","args":["-x"]}`, "c1"},
		{UserSocket, `{"kind":"start_session","correlationId":"c1","sessionId":"` + unitSID + `","agent":"claude","prompt":"hi","env":{"A":"b"}}`, "c1"},
		{UserSocket, `{"kind":"start_session","correlationId":"c1","sessionId":"` + unitSID + `","agent":"/usr/bin/claude","prompt":"hi"}`, "c1"},
		{UserSocket, `{"kind":"start_session","correlationId":"c1","sessionId":"` + unitSID + `","agent":"claude","prompt":""}`, "c1"},
		{UserSocket, `{"kind":"start_session","correlationId":"c1","sessionId":"not-a-uuid","agent":"claude","prompt":"hi"}`, "c1"},
		{UserSocket, `{"kind":"start_session","correlationId":"resolved:c1","sessionId":"` + unitSID + `","agent":"claude","prompt":"hi"}`, "resolved:c1"},
		{UserSocket, `{"kind":"user_message","correlationId":"c1","sessionId":"` + unitSID + `","text":"hi"}`, "c1"},
		{UserSocket, `{"kind":"approval_response","correlationId":"resolved:r","sessionId":"` + unitSID + `","approvalId":"a","decision":"allow","reason":"user"}`, "resolved:r"},
		{SessionSocket, `{"kind":"start_session","correlationId":"c1","sessionId":"` + unitSID + `","agent":"claude","prompt":"hi"}`, "c1"},
		{SessionSocket, `{"kind":"user_message","correlationId":"c1","sessionId":"` + unitSID + `","text":"hi","model":"opus"}`, "c1"},
		{SessionSocket, `{"kind":"interrupt","correlationId":"c1","sessionId":"` + unitSID + `","signal":"KILL"}`, "c1"},
		{SessionSocket, `{"kind":"approval_response","correlationId":"r","sessionId":"` + unitSID + `","approvalId":"a","decision":"allow","reason":"user"}`, "r"},
		{SessionSocket, `{"kind":"approval_response","correlationId":"resolved:r","sessionId":"` + unitSID + `","approvalId":"a","decision":"allow","reason":"user","scope":"reject_always"}`, "resolved:r"},
		{SessionSocket, `{"kind":"approval_response","correlationId":"resolved:r","sessionId":"` + unitSID + `","approvalId":"a","decision":"maybe","reason":"user"}`, "resolved:r"},
		{SessionSocket, `{"kind":"user_message","correlationId":7,"sessionId":"` + unitSID + `","text":"hi"}`, ""},
		{SessionSocket, `{"correlationId":"c1"}`, "c1"},
		{SessionSocket, `[1,2]`, ""},
		{SessionSocket, `{"kind":"user_message"} trailing`, ""},
		{SessionSocket, `{"kind":"stop_session","correlationId":"c1","sessionId":"` + unitSID + `"}}`, ""},
		{SessionSocket, `{"kind":"stop_session","correlationId":"c1","sessionId":"` + unitSID + `"}]`, ""},
		{SessionSocket, `{"kind":"stop_session","correlationId":"c1","sessionId":"` + unitSID + `"} {}`, ""},
		{UserSocket, `{"kind":"presence","online":null,"agents":[]}`, ""},
		{UserSocket, `{"kind":"presence","online":true,"agents":null}`, ""},
		{SessionSocket, `{"kind":"ack","correlationId":"run:1","seq":null,"duplicate":false}`, "run:1"},
		{SessionSocket, `{"kind":"ack","correlationId":"run:1","seq":4,"duplicate":null}`, "run:1"},
		{UserSocket, `{"kind":"approval_response","correlationId":"resolved:r","sessionId":"` + unitSID + `","approvalId":"a","decision":"allow","reason":"user","terminalReceipt":null}`, "resolved:r"},
		{SessionSocket, `{"kind":"approval_response","correlationId":"resolved:r","sessionId":"` + unitSID + `","approvalId":"a","decision":"allow","reason":"user","scope":null}`, "resolved:r"},
	}
	for _, c := range refused {
		in, err := Decode(c.socket, []byte(c.raw))
		var de *DecodeError
		if err == nil || !errors.As(err, &de) {
			t.Errorf("%s accepted: %+v", c.raw, in)
			continue
		}
		if de.CorrelationID != c.ackCID {
			t.Errorf("%s: ack id %q, want %q", c.raw, de.CorrelationID, c.ackCID)
		}
	}
}

func TestCommandAckShapes(t *testing.T) {
	if got := string(commandAck(SessionSocket, "resolved:x", unitSID)); got != `{"kind":"command_ack","correlationId":"resolved:x"}` {
		t.Errorf("session ack %s", got)
	}
	if got := string(commandAck(UserSocket, "resolved:x", unitSID)); got != `{"kind":"command_ack","correlationId":"resolved:x","sessionId":"`+unitSID+`"}` {
		t.Errorf("user ack %s", got)
	}
	caps := string(capabilitiesFrame([]Capability{{Agent: "claude", Version: "2.1.280", Available: true, LoggedIn: true}}))
	if caps != `{"kind":"capabilities","agents":[{"agent":"claude","version":"2.1.280","available":true,"loggedIn":true}]}` {
		t.Errorf("capabilities %s", caps)
	}
	if capabilityVersion(" 1.2.3\x07 ") != "1.2.3" || capabilityVersion("") != "unknown" || len(capabilityVersion(strings.Repeat("9", 80))) != 64 {
		t.Error("capability version sanitising")
	}
}

// ─── shaping and outbox ─────────────────────────────────────────────────────

func TestCoalescer(t *testing.T) {
	var c Coalescer
	var out []Chunk
	out = append(out, c.Add(Chunk{"type": "text-start", "id": "a"})...)
	for _, d := range []string{"he", "ll", "o"} {
		out = append(out, c.Add(Chunk{"type": "text-delta", "id": "a", "delta": d})...)
	}
	out = append(out, c.Add(Chunk{"type": "reasoning-delta", "id": "a", "delta": "x"})...)
	out = append(out, c.Add(Chunk{"type": "text-end", "id": "a"})...)
	out = append(out, c.Flush()...)
	var types []string
	for _, o := range out {
		types = append(types, fmt.Sprint(o["type"], ":", o["delta"]))
	}
	if strings.Join(types, ",") != "text-start:<nil>,text-delta:hello,reasoning-delta:x,text-end:<nil>" {
		t.Fatalf("coalesced %v", types)
	}
}

func newTestOutbox(t *testing.T) *Outbox {
	p, err := mosaicevent.NewSet()
	if err != nil {
		t.Fatal(err)
	}
	return NewOutbox(p, NewLogger(io.Discard))
}

func TestOutboxValidatesAndKeepsBytes(t *testing.T) {
	o := newTestOutbox(t)
	if n := o.Emit(Chunk{"type": "text-delta", "id": "a", "delta": "hi"}); n != 1 {
		t.Fatalf("valid chunk queued %d", n)
	}
	// Invalid chunks never reach the outbox.
	for _, bad := range []Chunk{
		{"type": "text-delta", "delta": "missing id"},
		{"type": "no-such-type"},
		{"type": "data-session-status", "data": map[string]any{"sessionId": unitSID, "status": "exploded"}},
	} {
		if n := o.Emit(bad); n != 0 {
			t.Errorf("invalid chunk queued: %v", bad)
		}
	}
	entries := o.After(0)
	if len(entries) != 1 {
		t.Fatalf("entries %d", len(entries))
	}
	var env map[string]any
	if err := json.Unmarshal(entries[0].raw, &env); err != nil {
		t.Fatal(err)
	}
	if len(env) != 8 || env["origin"] != "daemon" || env["schemaVersion"] != "mosaic-event/1" ||
		env["correlationId"] != o.RunID()+":1" || env["seq"] != float64(1) {
		t.Fatalf("envelope %v", env)
	}
	first := string(entries[0].raw)
	if again := o.After(0); string(again[0].raw) != first {
		t.Fatal("resend bytes differ")
	}
	if !o.Ack(entries[0].cid) || o.Len() != 0 {
		t.Fatal("ack did not remove the event")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if !o.WaitDrained(ctx) {
		t.Fatal("drained not signalled")
	}
}

func TestOutboxFrameGuard(t *testing.T) {
	o := newTestOutbox(t)
	big := strings.Repeat("é", 100_000) // 200 KB of 2-byte runes
	if n := o.Emit(Chunk{"type": "text-delta", "id": "a", "delta": big}); n < 4 {
		t.Fatalf("large delta split into %d events", n)
	}
	var joined strings.Builder
	for _, e := range o.After(0) {
		if len(e.raw) > maxFrameBytes {
			t.Fatalf("frame %d bytes", len(e.raw))
		}
		var env struct {
			Payload map[string]any `json:"payload"`
		}
		_ = json.Unmarshal(e.raw, &env)
		joined.WriteString(env.Payload["delta"].(string))
	}
	if joined.String() != big {
		t.Fatal("split delta does not reassemble")
	}
	o2 := newTestOutbox(t)
	if n := o2.Emit(Chunk{"type": "tool-output-available", "toolCallId": "t", "output": strings.Repeat("x", 200_000)}); n != 1 {
		t.Fatal("large tool output not truncated into one event")
	}
	raw := string(o2.After(0)[0].raw)
	if len(raw) > maxFrameBytes || !strings.Contains(raw, "truncated by archivist connect") {
		t.Fatalf("tool output frame %d bytes", len(raw))
	}
	// Many small values exceed the relay value bound and are stringified.
	many := make([]any, 25_000)
	for i := range many {
		many[i] = 1
	}
	o3 := newTestOutbox(t)
	if n := o3.Emit(Chunk{"type": "tool-output-available", "toolCallId": "t", "output": many}); n != 1 {
		t.Fatal("value-bound output not reduced")
	}
	// A non-reducible oversized chunk is dropped, never sent.
	o4 := newTestOutbox(t)
	if n := o4.Emit(Chunk{"type": "start", "messageId": strings.Repeat("m", 70_000), "messageMetadata": map[string]any{"schemaVersion": mosaicevent.Version}}); n != 0 {
		t.Fatal("oversized start sent")
	}
}

// emittedEnvelopes decodes the outbox's queued envelopes.
func emittedEnvelopes(t *testing.T, o *Outbox) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, e := range o.After(0) {
		var env map[string]any
		if err := json.Unmarshal(e.raw, &env); err != nil {
			t.Fatal(err)
		}
		out = append(out, env)
	}
	return out
}

func TestOutboxStampsV2OnlyOnAuthPrompts(t *testing.T) {
	o := newTestOutbox(t)
	deadline := time.UnixMilli(1_791_000_000_000)
	for _, c := range []Chunk{
		{"type": "data-session-status", "data": map[string]any{"sessionId": unitSID, "status": "starting"}},
		authPromptChunk("codex", "login-1", "https://auth.openai.com/codex/device", "enter ABCD-1234", "ABCD-1234", deadline),
		authPromptChunk("claude", "claude-signin-x", "https://claude.com/cai/oauth/authorize?x=1", "send the code", "", deadline),
		{"type": "start", "messageId": "m", "messageMetadata": map[string]any{"schemaVersion": mosaicevent.Version}},
		{"type": "text-delta", "id": "a", "delta": "hi"},
	} {
		if n := o.Emit(c); n != 1 {
			t.Fatalf("%v queued %d", c["type"], n)
		}
	}
	var got []string
	for _, env := range emittedEnvelopes(t, o) {
		got = append(got, env["type"].(string)+"="+env["schemaVersion"].(string))
	}
	want := "data-session-status=mosaic-event/1,data-auth-prompt=mosaic-event/2,data-auth-prompt=mosaic-event/2," +
		"start=mosaic-event/1,text-delta=mosaic-event/1"
	if strings.Join(got, ",") != want {
		t.Fatalf("stamps %v", got)
	}
	envs := emittedEnvelopes(t, o)
	codex := envs[1]["payload"].(map[string]any)["data"].(map[string]any)
	if codex["code"] != "ABCD-1234" || codex["expiresAt"] != float64(1_791_000_000_000) || codex["message"] != "enter ABCD-1234" {
		t.Fatalf("codex prompt %v", codex)
	}
	claude := envs[2]["payload"].(map[string]any)["data"].(map[string]any)
	if _, ok := claude["code"]; ok || claude["expiresAt"] != float64(1_791_000_000_000) {
		t.Fatalf("claude prompt %v", claude)
	}
	// v2-only content on a v1-stamped event is refused, never sent.
	if n := o.Emit(Chunk{"type": "data-session-status", "data": map[string]any{"sessionId": unitSID, "status": "lost"}}); n != 0 {
		t.Fatal("a v1 event with a v2 status was queued")
	}
	// An auth prompt the v2 contract refuses is dropped.
	bad := authPromptChunk("codex", "login-2", "", "m", "", deadline)
	bad["data"].(map[string]any)["code"] = "has space"
	if n := o.Emit(bad); n != 0 {
		t.Fatal("an invalid auth prompt was queued")
	}
}

func TestAuthPromptOmitsCodeTheContractRefuses(t *testing.T) {
	deadline := time.Now().Add(5 * time.Minute)
	long := strings.Repeat("A", 64)
	for _, c := range []struct {
		code string
		keep bool
	}{
		{"FAKE-1234", true},
		{long, true},
		{"!~", true},
		{"", false},
		{long + "A", false},
		{"ABCD 1234", false},
		{"ABCD\t1234", false},
		{"ABCD-1234\n", false},
		{"ÄBCD-1234", false},
		{"ABCD\x7f", false},
		{"ABCD\x00", false},
	} {
		chunk := authPromptChunk("codex", "login", "https://auth.openai.com/codex/device", "enter the code", c.code, deadline)
		data := chunk["data"].(map[string]any)
		if _, ok := data["code"]; ok != c.keep {
			t.Errorf("code %q kept=%v, want %v", c.code, ok, c.keep)
		}
		if data["expiresAt"] != deadline.UnixMilli() || data["message"] != "enter the code" {
			t.Errorf("code %q: prompt %v", c.code, data)
		}
		// Every prompt the daemon builds is a valid v2 event.
		if n := newTestOutbox(t).Emit(chunk); n != 1 {
			t.Errorf("code %q: prompt not queued", c.code)
		}
	}
}

// ─── state ──────────────────────────────────────────────────────────────────

func TestStorePermissionsAndRecords(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "connect")
	st, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	rec := &SessionRecord{SessionID: unitSID, Agent: "claude", Status: "active", CreatedAt: 1}
	for i := 0; i < maxHandled+10; i++ {
		rec.MarkHandled(fmt.Sprint("c", i))
	}
	if len(rec.Handled) != maxHandled || rec.HasHandled("c0") || !rec.HasHandled(fmt.Sprint("c", maxHandled+9)) {
		t.Fatal("handled window")
	}
	if err := st.Save(rec); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(filepath.Join(dir, "sessions", unitSID+".json"))
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("record mode %v", info.Mode())
	}
	for _, d := range []string{dir, filepath.Join(dir, "sessions"), filepath.Join(dir, "run")} {
		info, _ := os.Stat(d)
		if info.Mode().Perm() != 0o700 {
			t.Fatalf("%s mode %v", d, info.Mode())
		}
	}
	got, err := st.Load(unitSID)
	if err != nil || got.Agent != "claude" {
		t.Fatalf("Load %v %v", got, err)
	}
	if _, err := st.Load("../../etc/passwd"); err == nil {
		t.Fatal("path traversal accepted")
	}
	if _, err := st.Load("1f8fad5b-d9cb-469f-a165-70867728950e"); !isNotExist(err) {
		t.Fatalf("missing record: %v", err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "sessions", unitSID+".json"))
	if strings.Contains(string(data), "mst_") || strings.Contains(string(data), "ak_") {
		t.Fatal("state holds a token")
	}
}

// ─── detection ──────────────────────────────────────────────────────────────

func fakeRunner(version, auth string) Runner {
	return func(_ context.Context, env []string, dir, bin string, args ...string) ([]byte, error) {
		switch strings.Join(args, " ") {
		case "--version":
			return []byte(version), nil
		case "auth status --json":
			return []byte(auth), nil
		}
		return nil, errors.New("unexpected")
	}
}

func TestDetect(t *testing.T) {
	look := func(found ...string) LookPath {
		return func(file string) (string, error) {
			for _, f := range found {
				if f == file {
					return "/bin/" + file, nil
				}
			}
			return "", errors.New("not found")
		}
	}
	sub := `{"loggedIn":true,"authMethod":"claude.ai","subscriptionType":"max"}`
	cases := []struct {
		name, version, auth, problem string
		found                        []string
	}{
		{"usable", "2.1.288 (Claude Code)", sub, "", []string{"claude"}},
		{"missing", "", "", "missing", nil},
		{"too old", "2.1.279 (Claude Code)", sub, "version", []string{"claude"}},
		{"logged out", "2.1.280 (Claude Code)", `{"loggedIn":false}`, "auth", []string{"claude"}},
		{"api key login", "2.1.280 (Claude Code)", `{"loggedIn":true,"authMethod":"api-key"}`, "auth", []string{"claude"}},
		{"garbage version", "Claude Code", sub, "version", []string{"claude"}},
	}
	for _, c := range cases {
		d := Detect(context.Background(), look(c.found...), fakeRunner(c.version, c.auth), nil, "/")
		if d.Claude.ProblemCode != c.problem {
			t.Errorf("%s: problem %q (%s), want %q", c.name, d.Claude.ProblemCode, d.Claude.Problem, c.problem)
		}
	}
	d := Detect(context.Background(), look("claude", "codex"), fakeRunner("codex-cli 0.160.0", sub), nil, "/")
	caps := d.Capabilities()
	if len(caps) != 2 || caps[1] != (Capability{Agent: "codex", Version: "0.160.0"}) {
		t.Fatalf("capabilities %+v", caps)
	}
}

// ─── relay helpers ──────────────────────────────────────────────────────────

func TestTicketSubjectAndBackoff(t *testing.T) {
	claims := base64.RawURLEncoding.EncodeToString([]byte(`{"v":1,"sub":"user_abc","sid":"user:user_abc","iat":1,"exp":2}`))
	if sub, err := ticketSubject(claims + "." + strings.Repeat("A", 43)); err != nil || sub != "user_abc" {
		t.Fatalf("subject %q %v", sub, err)
	}
	if _, err := ticketSubject("nodot"); err == nil {
		t.Fatal("malformed ticket accepted")
	}
	var b backoff
	for i := 0; i < 12; i++ {
		d := b.next()
		base := 250 * time.Millisecond << min(i, 7)
		if base > 30*time.Second {
			base = 30 * time.Second
		}
		if d < base*3/4 || d > base*5/4 {
			t.Fatalf("step %d: %v outside ±25%% of %v", i, d, base)
		}
	}
	b.reset()
	if d := b.next(); d > 313*time.Millisecond {
		t.Fatalf("reset backoff %v", d)
	}
}

func TestClassifyMint(t *testing.T) {
	cases := []struct {
		err   error
		fatal string
		gone  bool
	}{
		{&client.APIError{Status: 404, Code: "FEATURE_DISABLED"}, "FEATURE_DISABLED", false},
		{&client.APIError{Status: 401}, "UNAUTHORIZED", false},
		{&client.APIError{Status: 403, Code: "PARENT_KEY_REQUIRED"}, "PARENT_KEY_REQUIRED", false},
		{&client.APIError{Status: 409, Code: "SESSION_INACTIVE"}, "", true},
		{&client.APIError{Status: 404, Code: "SESSION_NOT_FOUND"}, "", true},
		{&client.ExitCodeError{Code: 5, APICode: "SERVER_ERROR"}, "", false},
		{errors.New("dial tcp: connection refused"), "", false},
	}
	for _, c := range cases {
		fatal, gone := classifyMint(c.err, true)
		code := ""
		if fatal != nil {
			code = fatal.Code
		}
		if code != c.fatal || gone != c.gone {
			t.Errorf("%v: fatal %q gone %v", c.err, code, gone)
		}
	}
	if fatal, _ := classifyMint(&client.APIError{Status: http.StatusConflict}, false); fatal != nil {
		t.Error("user scope 409 must be transient")
	}
}

// ─── claude invocation ──────────────────────────────────────────────────────

func TestClaudeArgsAreFixed(t *testing.T) {
	cfg := ClaudeConfig{Bin: "/usr/bin/claude", Model: "claude-sonnet-5", Effort: "low", SettingSources: "", Executable: "/opt/archivist"}
	args := claudeArgs(cfg, "/state/run/x/mcp.json", "/state/run/x/mosaic-guidance.md", "/tmp/cwd", "abc")
	joined := strings.Join(args, " ")
	for _, banned := range []string{"--permission-mode", "--bare", "--dangerously-skip-permissions"} {
		if strings.Contains(joined, banned) {
			t.Fatalf("banned flag %s", banned)
		}
	}
	if args[len(args)-2] != "--add-dir" || args[len(args)-1] != "/tmp/cwd" {
		t.Fatalf("--add-dir must be last: %v", args)
	}
	// Story 78.31: the research guidance file, right before --add-dir; never
	// the inline or replacing system prompt flags.
	if args[len(args)-4] != "--append-system-prompt-file" || args[len(args)-3] != "/state/run/x/mosaic-guidance.md" {
		t.Fatalf("guidance flag must precede --add-dir: %v", args)
	}
	for _, a := range args {
		if a == "--system-prompt" || a == "--system-prompt-file" || a == "--append-system-prompt" {
			t.Fatalf("unexpected system prompt flag %s", a)
		}
	}
	for i, a := range args {
		if a == "--setting-sources" && args[i+1] != "" {
			t.Fatalf("setting sources %q", args[i+1])
		}
	}
	if !strings.Contains(joined, "--resume=abc") || !strings.Contains(joined, "--model claude-sonnet-5 --effort low") {
		t.Fatalf("args %s", joined)
	}
	raw, err := mcpConfig(cfg, "/state/run/x/task-token", nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"mcpServers":{"archivist":{"args":["mcp","serve","--token-file","/state/run/x/task-token"],"command":"/opt/archivist","env":{}}}}` {
		t.Fatalf("mcp config %s", raw)
	}
	raw, _ = mcpConfig(cfg, "/t", []string{"--publish-session", "s1", "--publish-dir", "/cwd"})
	if !strings.Contains(string(raw), `"args":["mcp","serve","--token-file","/t","--publish-session","s1","--publish-dir","/cwd"]`) {
		t.Fatalf("mcp config with publish %s", raw)
	}
	cfg.BaseURL = "http://127.0.0.1:1"
	raw, _ = mcpConfig(cfg, "/t", nil)
	if !strings.Contains(string(raw), `"env":{"ARCHIVIST_BASE_URL":"http://127.0.0.1:1"}`) {
		t.Fatalf("mcp env %s", raw)
	}
	if problem := initProblem(claudeFrame{APIKeySource: "none", PermissionMode: "default"}); problem != "" {
		t.Fatal(problem)
	}
	for _, f := range []claudeFrame{{APIKeySource: "ANTHROPIC_API_KEY", PermissionMode: "default"},
		{APIKeySource: "none", PermissionMode: "auto"}, {}} {
		if initProblem(f) == "" {
			t.Errorf("init %+v passed", f)
		}
	}
}

func TestDetectCodexLogin(t *testing.T) {
	look := func(file string) (string, error) { return "/bin/" + file, nil }
	sub := `{"loggedIn":true,"authMethod":"claude.ai","subscriptionType":"max"}`
	owner := t.TempDir()
	if err := os.WriteFile(filepath.Join(owner, "auth.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	var codexEnvs [][]string
	runner := func(loginErr error, loginOut string) Runner {
		return func(_ context.Context, env []string, _ string, bin string, args ...string) ([]byte, error) {
			if strings.HasSuffix(bin, "codex") {
				codexEnvs = append(codexEnvs, env)
			}
			switch strings.Join(args, " ") {
			case "--version":
				if strings.HasSuffix(bin, "codex") {
					return []byte("codex-cli 0.160.0"), nil
				}
				return []byte("2.1.280 (Claude Code)"), nil
			case "auth status --json":
				return []byte(sub), nil
			case "login status":
				return []byte(loginOut), loginErr
			}
			return nil, errors.New("unexpected")
		}
	}
	cases := []struct {
		err       error
		out       string
		loggedIn  bool
		available bool
	}{
		{nil, "Logged in using ChatGPT\n", true, true},                      // codex-cli 0.160.0 (stderr, read combined)
		{nil, "", false, false},                                             // nothing printed: not a ChatGPT login
		{nil, "Logged in using an API key - sk-proj-***ABCD", false, false}, // API key login: not usable
		{nil, "WARNING: proceeding, even though we could not create PATH aliases\nLogged in using ChatGPT", true, true}, // warning line first
		{errors.New("exit status 1"), "Not logged in", false, false},
		{nil, "Not logged in", false, false},
		{errors.New("signal: killed"), "", false, false},
	}
	for _, c := range cases {
		codexEnvs = nil
		run := runner(c.err, c.out)
		d := DetectWith(context.Background(), DetectOptions{LookPath: look, Run: run, RunCombined: run,
			Env: []string{"HOME=/h"}, Dir: "/", CodexHome: owner})
		caps := d.Capabilities()
		if d.Codex.LoggedIn != c.loggedIn || caps[1].LoggedIn != c.loggedIn || caps[1].Available != c.available {
			t.Errorf("%v/%q: codex %+v caps %+v", c.err, c.out, d.Codex, caps[1])
		}
		if strings.Contains(c.out, "API key") && !strings.Contains(d.Codex.Problem, "only drives a ChatGPT login") {
			t.Errorf("API key login problem %q", d.Codex.Problem)
		}
		if c.loggedIn && d.Codex.Login != "Logged in using ChatGPT" {
			t.Errorf("login line %q", d.Codex.Login)
		}
		if len(codexEnvs) != 2 {
			t.Fatalf("codex probes %d", len(codexEnvs))
		}
		for _, env := range codexEnvs {
			if !contains(env, "CODEX_HOME="+owner) {
				t.Errorf("codex probe env %v lacks the owner CODEX_HOME", env)
			}
		}
	}
	// An old version with an API key login reports the version first, and
	// the key never reaches Login or Problem.
	old := func(_ context.Context, _ []string, _ string, bin string, args ...string) ([]byte, error) {
		if strings.Join(args, " ") == "--version" {
			return []byte("codex-cli 0.159.2"), nil
		}
		return []byte("Logged in using an API key - sk-proj-AbCdEfGhIjKlMnOpQrStUvWx"), nil
	}
	d0 := DetectWith(context.Background(), DetectOptions{LookPath: look, Run: old, RunCombined: old, Dir: "/", CodexHome: owner})
	if !strings.Contains(d0.Codex.Problem, "older than the supported floor") || strings.Contains(d0.Codex.Login+d0.Codex.Problem, "AbCdEfGh") {
		t.Errorf("old version with API key: problem %q login %q", d0.Codex.Problem, d0.Codex.Login)
	}
	// Without an owner home (Detect) Codex is reported but never available.
	d := Detect(context.Background(), look, runner(nil, "Logged in using ChatGPT"), nil, "/")
	if caps := d.Capabilities(); caps[1].Available || !caps[1].LoggedIn {
		t.Errorf("no owner home: %+v", caps[1])
	}
}

// Story 78.31: an oversized tool output keeps as much as fits the 64 KiB frame
// (not a 16 KB preview), and a Codex MCP result carrying its payload twice drops
// the text copy of structuredContent before anything is cut.
func TestToolOutputFitsTheFrame(t *testing.T) {
	payloadOf := func(o *Outbox) any {
		var env struct {
			Payload map[string]any `json:"payload"`
		}
		raw := o.After(0)[0].raw
		if len(raw) > maxFrameBytes {
			t.Fatalf("frame %d bytes", len(raw))
		}
		_ = json.Unmarshal(raw, &env)
		return env.Payload["output"]
	}

	// A large research result (chat-api allows 100k characters), quotes escaped twice.
	passages := make([]any, 0, 200)
	for i := range 200 {
		passages = append(passages, map[string]any{"id": fmt.Sprintf("c%03d", i), "cite_as": fmt.Sprintf("[cite:1.%d]", i+1),
			"snippet": strings.Repeat(`passage "text" `, 30)})
	}
	body, _ := json.Marshal(map[string]any{"results": passages})
	o := newTestOutbox(t)
	if o.Emit(Chunk{"type": "tool-output-available", "toolCallId": "t", "output": []any{map[string]any{"type": "text", "text": string(body)}}}) != 1 {
		t.Fatal("large tool output not sent")
	}
	cut, _ := payloadOf(o).(string)
	if !strings.Contains(cut, "truncated by archivist connect") || len(cut) < 40_000 {
		t.Fatalf("kept %d bytes; want the frame filled, far past the old 16000 byte preview", len(cut))
	}

	// Codex: content text equal to structuredContent; dropping the copy makes it fit uncut.
	structured := map[string]any{"results": passages[:60]}
	text, _ := json.Marshal(structured)
	if len(text) < maxFrameBytes/2 || 2*len(text) < maxFrameBytes {
		t.Fatalf("fixture %d bytes does not need the dedup", len(text))
	}
	o2 := newTestOutbox(t)
	out := map[string]any{"content": []any{map[string]any{"type": "text", "text": string(text)}}, "structuredContent": structured}
	if o2.Emit(Chunk{"type": "tool-output-available", "toolCallId": "t", "output": out}) != 1 {
		t.Fatal("codex output not sent")
	}
	got, _ := payloadOf(o2).(map[string]any)
	if got == nil || len(got["content"].([]any)) != 0 || got["structuredContent"] == nil {
		t.Fatalf("duplicate text not dropped: %T", payloadOf(o2))
	}
	if raw := string(o2.After(0)[0].raw); strings.Contains(raw, "truncated by archivist connect") {
		t.Fatal("a deduplicated output that fits was cut")
	}

	// Text that differs from structuredContent is kept.
	if _, changed := dropDuplicateText(map[string]any{"content": []any{map[string]any{"type": "text", "text": `{"other":1}`}},
		"structuredContent": map[string]any{"results": []any{}}}); changed {
		t.Fatal("non duplicate text dropped")
	}
}
