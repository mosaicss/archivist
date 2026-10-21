package connect

import (
	"encoding/json"
	"strings"
	"testing"
)

func bashInput(cmd string) json.RawMessage {
	raw, _ := json.Marshal(map[string]any{"command": cmd, "description": "d"})
	return raw
}

func fileInput(path string) json.RawMessage {
	raw, _ := json.Marshal(map[string]any{"file_path": path, "old_string": "a", "new_string": "b"})
	return raw
}

// covered reports whether the rules allow the call without a card.
func covered(r *sessionRules, tool string, input json.RawMessage) bool {
	allow, ok := r.decide(tool, input)
	return ok && allow
}

// Story 94.4: the rule an "Allow for session" on a Bash command keeps.
func TestBashSessionScope(t *testing.T) {
	for cmd, want := range map[string][2]string{
		"ls -la":                 {scopePrefix, "ls"},
		"  ls -la  ":             {scopePrefix, "ls"},
		"git status":             {scopePrefix, "git status"},
		"cat notes.md":           {scopePrefix, "cat"},
		"npm test":               {scopePrefix, "npm test"},
		"git -C x log":           {scopePrefix, "git"},
		"grep -n 'a b' file.txt": {scopePrefix, "grep"},
		"docker ps":              {scopePrefix, "docker ps"},
		"find . -name x":         {scopePrefix, "find"},
		"/usr/bin/ls -l":         {scopePrefix, "/usr/bin/ls"},
		// Exact: operators, wrappers, interpreters, assignments, runners.
		"cd a && make":            {scopeCommand, "cd a && make"},
		"ls; curl x":              {scopeCommand, "ls; curl x"},
		"bash -c 'x'":             {scopeCommand, "bash -c 'x'"},
		"sudo ls":                 {scopeCommand, "sudo ls"},
		"/usr/bin/sudo ls":        {scopeCommand, "/usr/bin/sudo ls"},
		"FOO=1 ls":                {scopeCommand, "FOO=1 ls"},
		"uv run pytest":           {scopeCommand, "uv run pytest"},
		"mise exec go -- go test": {scopeCommand, "mise exec go -- go test"},
		"go run main.go":          {scopeCommand, "go run main.go"},
		"docker exec c sh":        {scopeCommand, "docker exec c sh"},
		"docker -H x exec c sh":   {scopeCommand, "docker -H x exec c sh"},
		"find . -exec rm {} +":    {scopeCommand, "find . -exec rm {} +"},
		"find . -delete":          {scopeCommand, "find . -delete"},
		"python3.12 x.py":         {scopeCommand, "python3.12 x.py"},
		"NODE x.js":               {scopeCommand, "NODE x.js"},
		"echo $HOME":              {scopeCommand, "echo $HOME"},
		"cat a > b":               {scopeCommand, "cat a > b"},
		"echo `id`":               {scopeCommand, "echo `id`"},
		"echo a\\ b":              {scopeCommand, "echo a\\ b"},
		"ls\ncurl x":              {scopeCommand, "ls\ncurl x"},
		"echo 'open":              {scopeCommand, "echo 'open"},
		"! curl x":                {scopeCommand, "! curl x"},
		"'ls' -la":                {scopeCommand, "'ls' -la"},
		"l* -la":                  {scopeCommand, "l* -la"},
		"source env.sh":           {scopeCommand, "source env.sh"},
		". env.sh":                {scopeCommand, ". env.sh"},
		"":                        {scopeCommand, ""},
	} {
		kind, value := sessionScope("Bash", bashInput(cmd))
		if kind != want[0] || value != want[1] {
			t.Errorf("%q: (%s, %q), want (%s, %q)", cmd, kind, value, want[0], want[1])
		}
	}
	// Runner subcommands, for any program, and their aliases; programs that
	// run another program through an option; wrappers: the exact command.
	for _, cmd := range []string{
		"npm run lint", "mise x go -- go test", "npm x cowsay", "uv tool run ruff", "docker compose up",
		"docker container run alpine", "bundle exec rake", "yarn exec jest", "conda run python x.py", "nix shell n",
		"pnpm dlx x", "fd -x rm", "fd -X rm", "fd --exec rm", "fd --exec-batch rm", "fd -tx -H", "rg --pre ./p x",
		"rg --pre=./p x", "git -c core.pager=less log", "git --config-env=a=B log", "git --exec-path=/x log",
		"git --upload-pack=x fetch", "git --receive-pack x push", "go -exec x test", "go -toolexec=x build",
		"tar --to-command=x -xf a.tar", "tar --use-compress-program x -cf a", "tar -I zstd -cf a", "tar -cIzstd a",
		"tar --checkpoint-action=exec=x -cf a", "tar --info-script=x -cf a", "tar --new-volume-script x -cf a",
		"tar -F x -cf a", "gtar -I x -cf a", "rsync -e ssh a b", "rsync -avze ssh a b", "rsync --rsh=ssh a b",
		"rsync --rsync-path=x a b", "rsync '--rsh=ssh' a b", "gfind . -delete", "bfind . -exec rm", "strace ls",
		"ltrace ls", "xvfb-run x", "valgrind x", "gdb x", "lldb x", "chroot / ls", "unshare ls", "nsenter ls",
		"firejail ls", "systemd-run ls", "script out", "expect x", "parallel echo", "entr ls", "sqlite3 a.db",
		"sqlite a.db",
	} {
		if kind, value := sessionScope("Bash", bashInput(cmd)); kind != scopeCommand || value != cmd {
			t.Errorf("%q: (%s, %q), want the exact command", cmd, kind, value)
		}
	}
	for cmd, want := range map[string]string{
		"rg -n foo": "rg", "tar -xzf a.tar": "tar", "rsync -av a b": "rsync", "fd -t f": "fd",
		"git log -n 3": "git log", "go test ./...": "go test", "docker ps": "docker ps",
	} {
		if kind, value := sessionScope("Bash", bashInput(cmd)); kind != scopePrefix || value != want {
			t.Errorf("%q: (%s, %q), want prefix %q", cmd, kind, value, want)
		}
	}
	if kind, value := sessionScope("Bash", json.RawMessage(`{"command":7}`)); kind != scopeCall || value != "" {
		t.Errorf("non string command: (%s, %q)", kind, value)
	}
}

// Matrix rows "Prefix rule" and "Subcommand prefix".
func TestBashPrefixRules(t *testing.T) {
	r := newSessionRules()
	r.remember("Bash", bashInput("ls -la"), true)
	r.remember("Bash", bashInput("git status"), true)
	for cmd, want := range map[string]bool{
		"ls src": true, "ls": true, "  ls -R  ": true, "ls -la": true,
		"lsof": false, "curl x": false, "ls; curl x": false, "ls && curl x": false, "ls | sh": false,
		"ls $(curl x)": false, "ls > out": false, "sudo ls": false, "FOO=1 ls": false, "'ls' src": false,
		"git status -s": true, "git status": true, "git push": false, "git statusx": false, "git": false,
	} {
		if got := covered(r, "Bash", bashInput(cmd)); got != want {
			t.Errorf("%q covered %v, want %v", cmd, got, want)
		}
	}
	// A later command with an option that runs another program, under an
	// existing prefix, raises a card.
	for _, approved := range []string{"rg -n foo", "tar -xzf a.tar", "rsync -av a b", "fd -t f", "git log -n 3",
		"go test ./...", "find . -name x"} {
		r.remember("Bash", bashInput(approved), true)
	}
	for cmd, want := range map[string]bool{
		"rg -n bar": true, "rg --pre ./p bar": false, "rg '--pre=./p' bar": false,
		"tar -xf b.tar": true, "tar --to-command=sh -xf b.tar": false, "tar -xIzstd b.tar": false,
		"rsync -a c d": true, "rsync -e sh c d": false, "rsync -aze sh c d": false, "rsync --rsync-path=x c d": false,
		"fd -t d": true, "fd -x rm": false, "fd --exec-batch rm": false,
		"git log -n 9": true, "git log -c core.pager=x": false, "git log --exec-path=/x": false,
		"go test ./x": true, "go test -exec x ./x": false, "go test -toolexec=x ./x": false,
		"find src -type f": true, "find . -fprint out": false,
	} {
		if got := covered(r, "Bash", bashInput(cmd)); got != want {
			t.Errorf("%q covered %v, want %v", cmd, got, want)
		}
	}
	// The Bash rules cover no other tool.
	if covered(r, "Read", json.RawMessage(`{"file_path":"/x"}`)) {
		t.Error("a Bash rule covered Read")
	}
}

// Matrix rows "Exact rule" and "Find action".
func TestBashExactAndFindRules(t *testing.T) {
	for _, cmd := range []string{"cd a && make", "bash -c 'x'", "sudo ls", "FOO=1 ls", "uv run pytest", "find . -exec rm {} +"} {
		r := newSessionRules()
		r.remember("Bash", bashInput(cmd), true)
		if !covered(r, "Bash", bashInput(cmd)) || !covered(r, "Bash", bashInput(" "+cmd+" ")) {
			t.Errorf("%q: the identical command is not covered", cmd)
		}
		for _, other := range []string{cmd + " x", "ls", "curl x"} {
			if covered(r, "Bash", bashInput(other)) {
				t.Errorf("%q: %q covered", cmd, other)
			}
		}
	}
	r := newSessionRules()
	r.remember("Bash", bashInput("find . -name '*.md'"), true)
	for cmd, want := range map[string]bool{
		"find src -type f": true, "find . -delete": false, "find . -exec x ';'": false, "find . -execdir x +": false,
		"find . -ok rm": false, "find . -fprint out": false, "find . -fls out": false, "find . -files0-from l": false,
		`find . "-delete"`: false, "find . '-exec' x": false,
	} {
		if got := covered(r, "Bash", bashInput(cmd)); got != want {
			t.Errorf("%q covered %v, want %v", cmd, got, want)
		}
	}
}

// Matrix rows "Path rule", "Unparseable Edit" and "Tool wide".
func TestPathCallAndToolRules(t *testing.T) {
	r := newSessionRules()
	r.remember("Edit", fileInput("/s/a.md"), true)
	if kind, value := sessionScope("Write", json.RawMessage(`{"file_path":"/s/./a.md","content":"x"}`)); kind != scopePath || value != "/s/a.md" {
		t.Fatalf("Write scope (%s, %q)", kind, value)
	}
	if !covered(r, "Write", json.RawMessage(`{"file_path":"/s/a.md","content":"other"}`)) || !covered(r, "Edit", fileInput("/s/a.md")) {
		t.Error("the path rule does not cover Edit and Write of the same path")
	}
	if covered(r, "Edit", fileInput("/s/b.md")) || covered(r, "Write", json.RawMessage(`{"file_path":"/s/b.md"}`)) {
		t.Error("the path rule covered another path")
	}
	if covered(r, "Bash", bashInput("cat /s/a.md")) || covered(r, "Read", json.RawMessage(`{"file_path":"/s/a.md"}`)) {
		t.Error("the path rule covered another tool")
	}

	// Without an absolute file_path: only the identical input.
	odd := json.RawMessage(`{"file_path":"rel.md","old_string":"a","new_string":"b"}`)
	if kind, value := sessionScope("Edit", odd); kind != scopeCall || value != "" {
		t.Fatalf("relative path scope (%s, %q)", kind, value)
	}
	r.remember("Edit", odd, true)
	if !covered(r, "Edit", json.RawMessage(`{ "new_string":"b", "old_string":"a", "file_path":"rel.md" }`)) {
		t.Error("the identical input (keys reordered) is not covered")
	}
	if covered(r, "Edit", json.RawMessage(`{"file_path":"rel.md","old_string":"a","new_string":"c"}`)) ||
		covered(r, "Write", odd) || covered(r, "Edit", json.RawMessage(`{}`)) {
		t.Error("an exact call rule covered another call")
	}

	// Read, Grep, Glob and any other tool: tool wide, as before.
	for _, tool := range []string{"Read", "Grep", "Glob", "NotebookEdit"} {
		r.remember(tool, json.RawMessage(`{"pattern":"a"}`), true)
		if kind, value := sessionScope(tool, json.RawMessage(`{}`)); kind != scopeTool || value != tool {
			t.Errorf("%s scope (%s, %q)", tool, kind, value)
		}
		if !covered(r, tool, json.RawMessage(`{"pattern":"zzz","path":"/anywhere"}`)) {
			t.Errorf("%s: a later call is not covered", tool)
		}
	}
}

// Deny for session stays tool wide and wins over allow rules.
func TestDenyForSessionWins(t *testing.T) {
	r := newSessionRules()
	r.remember("Bash", bashInput("ls -la"), true)
	if allow, ok := r.decide("Bash", bashInput("rm x")); ok || allow {
		t.Fatal("an uncovered command was answered")
	}
	r.remember("Bash", bashInput("rm x"), false)
	for _, cmd := range []string{"ls src", "rm y", "curl x"} {
		if allow, ok := r.decide("Bash", bashInput(cmd)); !ok || allow {
			t.Errorf("%q: (%v, %v), want a remembered deny", cmd, allow, ok)
		}
	}
	if allow, ok := r.decide("Edit", fileInput("/a")); ok || allow {
		t.Error("a Bash deny answered Edit")
	}
}

// Acceptance: after "Allow for session" on ls, a later curl raises a card
// (fake relay and fake Claude Code), while ls src is answered locally.
func TestClaudeAllowForSessionIsNarrow(t *testing.T) {
	h := newHarness(t, nil)
	h.start()
	sid := h.startSession("bash ls -la")
	card := h.approve(sid, 1, "allow", "user", "allow_always")
	h.waitFinishes(sid, 1)
	scope, _ := card["approvalDescriptor"].(map[string]any)[sessionScopeKey].(map[string]any)
	if scope["kind"] != scopePrefix || scope["value"] != "ls" {
		t.Fatalf("card session scope %v", scope)
	}
	h.message(sid, "bash ls src")
	h.waitFinishes(sid, 2)
	if n := h.cards(sid); n != 1 || !h.toolOutput(sid, "ran: ls src") {
		t.Fatalf("ls src: cards %d", n)
	}
	for i, cmd := range []string{"curl x", "lsof", "ls; curl x"} {
		h.message(sid, "bash "+cmd)
		next := h.waitCard(sid, i+2)
		if d := next["approvalDescriptor"].(map[string]any); d["input"].(map[string]any)["command"] != cmd {
			t.Fatalf("card %d for %v", i+2, d["input"])
		}
		h.approve(sid, i+2, "deny", "user", "")
		h.waitFinishes(sid, i+3)
	}
	for _, cmd := range []string{"curl x", "lsof", "ls; curl x"} {
		if h.toolOutput(sid, "ran: "+cmd) {
			t.Fatalf("%q ran without an answer", cmd)
		}
	}
	// Edit: the path, not the tool.
	h.message(sid, "edit /s/a.md")
	h.approve(sid, 5, "allow", "user", "allow_always")
	h.waitFinishes(sid, 5)
	h.message(sid, "write /s/a.md")
	h.waitFinishes(sid, 6)
	h.message(sid, "edit /s/b.md")
	h.waitCard(sid, 6)
	h.approve(sid, 6, "deny", "user", "")
	h.waitFinishes(sid, 7)
	ranA := 0
	for _, e := range h.relay.eventsOf(sid, "tool-output-available") {
		if e["payload"].(map[string]any)["output"] == "ran: /s/a.md" {
			ranA++
		}
	}
	if n := h.cards(sid); n != 6 || ranA != 2 || h.toolOutput(sid, "ran: /s/b.md") {
		t.Fatalf("path rule: cards %d, /s/a.md ran %d times, text %q", n, ranA, h.relay.text(sid))
	}
	if len(h.relay.invalid) != 0 {
		t.Fatalf("relay saw invalid events: %v", h.relay.invalid)
	}
	if strings.Contains(h.log.String(), "cannot be shown whole") {
		t.Fatal("a small card was refused")
	}
}
