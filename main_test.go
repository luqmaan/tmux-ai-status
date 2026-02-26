package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParsePPIDFromStat(t *testing.T) {
	tests := []struct {
		name string
		stat string
		want int
	}{
		{"simple comm", "123 (bash) S 100 123 123 0 -1 ...", 100},
		{"comm with spaces", "456 (Web Content) S 200 456 456 0 -1 ...", 200},
		{"comm with parens", "789 (foo (bar)) S 300 789 789 0 -1 ...", 300},
		{"empty", "", 0},
		{"truncated", "1 (init) S", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parsePPIDFromStat(tt.stat)
			if got != tt.want {
				t.Errorf("parsePPIDFromStat(%q) = %d, want %d", tt.stat, got, tt.want)
			}
		})
	}
}

func TestClassifyChildren(t *testing.T) {
	tests := []struct {
		names []string
		want  string
	}{
		{[]string{"gcc"}, "🔨"},
		{[]string{"make", "cc1"}, "🔨"},
		{[]string{"rustc"}, "🔨"},
		{[]string{"jest"}, "🧪"},
		{[]string{"pytest"}, "🧪"},
		{[]string{"npm"}, "📦"},
		{[]string{"pip"}, "📦"},
		{[]string{"git"}, "🔀"},
		{[]string{"curl"}, "🌐"},
		{[]string{"wget"}, "🌐"},
		{[]string{"python3"}, "⚙️"},
		{[]string{"sh"}, "⚙️"},
		{[]string{"rustc", "cargo"}, "🔨"},
		{[]string{"git", "curl"}, "🔀"},
		{[]string{"GCC"}, "🔨"},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.names, "+"), func(t *testing.T) {
			got := classifyChildren(tt.names)
			if got != tt.want {
				t.Errorf("classifyChildren(%v) = %q, want %q", tt.names, got, tt.want)
			}
		})
	}
}

func TestContainsAny(t *testing.T) {
	if !containsAny("hello world", "hello") {
		t.Error("should contain hello")
	}
	if containsAny("hello world", "foo", "bar") {
		t.Error("should not contain foo or bar")
	}
	if !containsAny("hello world", "foo", "world") {
		t.Error("should contain world")
	}
}

func TestCollectDescendants(t *testing.T) {
	childMap := map[int][]int{
		1: {2, 3}, 2: {4}, 3: {5, 6}, 6: {7},
	}
	got := collectDescendants(1, childMap)
	want := map[int]bool{2: true, 3: true, 4: true, 5: true, 6: true, 7: true}
	if len(got) != len(want) {
		t.Fatalf("got %d items, want %d", len(got), len(want))
	}
	for _, pid := range got {
		if !want[pid] {
			t.Errorf("unexpected pid %d", pid)
		}
	}
}

func TestCollectDescendants_Empty(t *testing.T) {
	got := collectDescendants(1, map[int][]int{1: {}})
	if len(got) != 0 {
		t.Errorf("expected empty, got %v", got)
	}
}

func TestGetStatus_NoAgent(t *testing.T) {
	myPID := os.Getpid()
	status := getStatus("fake:0", myPID, map[int][]int{myPID: {}})
	if status != "" {
		t.Errorf("expected empty status, got %q", status)
	}
}

func TestBuildChildMap_ContainsSelf(t *testing.T) {
	m := buildChildMap()
	found := false
	for _, c := range m[os.Getppid()] {
		if c == os.Getpid() {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("PID %d not found under PPID %d", os.Getpid(), os.Getppid())
	}
}

func TestReadComm_Self(t *testing.T) {
	if readComm(os.Getpid()) == "" {
		t.Error("readComm for self should not be empty")
	}
}

func TestReadCmdline_Self(t *testing.T) {
	if readCmdline(os.Getpid()) == "" {
		t.Error("readCmdline for self should not be empty")
	}
}

func TestReadComm_InvalidPID(t *testing.T) {
	if readComm(999999999) != "" {
		t.Error("should be empty for invalid PID")
	}
}

func TestReadCmdline_InvalidPID(t *testing.T) {
	if readCmdline(999999999) != "" {
		t.Error("should be empty for invalid PID")
	}
}

func TestFindAgent_NoChildren(t *testing.T) {
	pid, name := findAgent(100, map[int][]int{100: {}})
	if pid != 0 || name != "" {
		t.Errorf("expected no agent, got pid=%d name=%q", pid, name)
	}
}

func TestFindAgent_DoesNotExceedGrandchildren(t *testing.T) {
	childMap := map[int][]int{100: {200}, 200: {300}, 300: {400}}
	pid, _ := findAgent(100, childMap)
	if pid != 0 {
		t.Error("should not find agent at great-grandchild level")
	}
}

func TestReadPPID_Self(t *testing.T) {
	if readPPID(os.Getpid()) != os.Getppid() {
		t.Errorf("readPPID(self) = %d, want %d", readPPID(os.Getpid()), os.Getppid())
	}
}

func TestListPanes_NoCrash(t *testing.T) {
	_ = listPanes()
}

func TestReadCmdline_NullBytes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cmdline")
	os.WriteFile(path, []byte("/usr/bin/node\x00/path/to/claude\x00--flag\x00"), 0644)
	data, _ := os.ReadFile(path)
	result := strings.ReplaceAll(string(data), "\x00", " ")
	if !strings.Contains(result, "claude") {
		t.Errorf("should contain claude, got %q", result)
	}
}

// --- classifyPaneContent tests ---

func TestClassifyPaneContent_Active(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{"esc to interrupt", "some output\n  (esc to interrupt)\n❯ \n"},
		{"claude thinking", "· Thinking… (5s · esc to interrupt)\n❯ \n"},
		{"codex planning", "• Planning try removal patch (5m 42s • esc to interrupt)\n› \n"},
		{"spinner no esc", "✢ Transfiguring… (thought for 6s)\n❯ \n"},
		{"brewing no esc", "· Brewing… (2s)\n❯ \n"},
		{"leavening", "· Leavening… (54s · ↑ 1.0k tokens · thought for 28s)\n❯ \n"},
		{"unknown future verb", "✻ Zymurgying… (3s)\n❯ \n"},
		{"three dots", "· Pondering... (1s)\n❯ \n"},
		{"accomplishing", "· Accomplishing… (1m 13s · ↓ 1.3k tokens · thought for 20s)\n❯ \n"},
		{"bare spinner no parens", "* Perusing…\n\n──────\n❯ \n"},
		{"bare spinner three dots", "· Thinking...\n❯ \n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !classifyPaneContent(tt.content) {
				t.Errorf("expected active for %q", tt.name)
			}
		})
	}
}

func TestClassifyPaneContent_Idle(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{"claude idle", "output\n\n❯ \n──────\n  🟢 19%\n  ⏵⏵ bypass permissions on\n"},
		{"codex idle", "Done.\n\n› Explain this codebase\n\n  gpt-5.3-codex · 87% left\n"},
		{"codex worked", "─ Worked for 1m 51s ──────\n• Deployed.\n› \n"},
		{"codex cogitated", "✻ Cogitated for 1m 27s\n❯ \n"},
		{"prose contains ing dots", "Discussion summary...\nI am discussing...\n› Explain this codebase\n"},
		{"empty", ""},
		{"plain shell", "$ ls\nfile1\n$ \n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if classifyPaneContent(tt.content) {
				t.Errorf("expected idle for %q", tt.name)
			}
		})
	}
}

func TestClassifyPaneNeedsAttention(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    bool
	}{
		{
			name: "codex waiting at prompt",
			content: "Done.\n\n› Run /review on my current changes\n\n" +
				"  gpt-5.3-codex · 87% left\n",
			want: true,
		},
		{
			name: "claude waiting at prompt",
			content: "All set.\n\n❯ \n──────\n" +
				"  🟢 19%\n",
			want: true,
		},
		{
			name:    "active spinner is not attention",
			content: "· Thinking… (5s · esc to interrupt)\n❯ \n",
			want:    false,
		},
		{
			name:    "plain output",
			content: "$ ls\nfile1\n$ \n",
			want:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifyPaneNeedsAttention(tt.content); got != tt.want {
				t.Errorf("classifyPaneNeedsAttention(%q) = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}

// --- Debounce / grace period tests ---

func TestIsPaneActive_GracePeriod(t *testing.T) {
	window := "test:99"

	// Seed as recently active
	lastActiveMu.Lock()
	lastActive[window] = time.Now()
	lastActiveMu.Unlock()

	// isPaneActive calls tmux which won't have this window,
	// so capture-pane fails → content check returns false.
	// But grace period should still return true.
	result := isPaneActive(window)
	if !result {
		t.Error("should return true during grace period even if capture fails")
	}

	// Clean up
	lastActiveMu.Lock()
	delete(lastActive, window)
	lastActiveMu.Unlock()
}

func TestIsPaneActive_GraceExpired(t *testing.T) {
	window := "test:98"

	// Seed as active long ago (past grace period)
	lastActiveMu.Lock()
	lastActive[window] = time.Now().Add(-activeGrace - time.Second)
	lastActiveMu.Unlock()

	result := isPaneActive(window)
	if result {
		t.Error("should return false after grace period expires")
	}

	// Clean up in case
	lastActiveMu.Lock()
	delete(lastActive, window)
	lastActiveMu.Unlock()
}

func TestIsPaneActive_NoHistory(t *testing.T) {
	window := "test:97"

	// No history, capture will fail → should be false
	lastActiveMu.Lock()
	delete(lastActive, window)
	lastActiveMu.Unlock()

	result := isPaneActive(window)
	if result {
		t.Error("should return false with no history and no content")
	}
}

// --- Unread tracking tests ---

func TestIsWorkingStatus(t *testing.T) {
	tests := []struct {
		status string
		want   bool
	}{
		{"🧠", true},
		{"🔨", true},
		{"⚙️", true},
		{"x 🧠", true},
		{"x 🔨", true},
		{"c 🧠", true},
		{"💤", false},
		{"c 💤", false},
		{"x 💤", false},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.status, func(t *testing.T) {
			if got := isWorkingStatus(tt.status); got != tt.want {
				t.Errorf("isWorkingStatus(%q) = %v, want %v", tt.status, got, tt.want)
			}
		})
	}
}

func TestStatusPriority(t *testing.T) {
	if statusPriority("🧠") <= statusPriority("💤") {
		t.Error("working should have higher priority than idle")
	}
	if statusPriority("💤") <= statusPriority("") {
		t.Error("idle should have higher priority than empty")
	}
}

func TestUnreadMarkAndClear(t *testing.T) {
	window := "test:unread1"

	// Clean state
	statusStateMu.Lock()
	delete(statusState, window)
	statusStateMu.Unlock()

	if isUnread(window) {
		t.Error("should not be unread initially")
	}

	markUnread(window)
	if !isUnread(window) {
		t.Error("should be unread after marking")
	}

	clearUnread(window)
	if isUnread(window) {
		t.Error("should not be unread after clearing")
	}

	// Clean up
	statusStateMu.Lock()
	delete(statusState, window)
	statusStateMu.Unlock()
}

func TestUnreadReplacesIdle(t *testing.T) {
	// When unread, 💤 should become 📬
	window := "test:unread2"

	statusStateMu.Lock()
	delete(statusState, window)
	statusStateMu.Unlock()

	markUnread(window)

	status := "💤"
	if isUnread(window) && strings.HasSuffix(status, "💤") {
		status = strings.TrimSuffix(status, "💤") + "📬"
	}
	if status != "📬" {
		t.Errorf("expected 📬, got %q", status)
	}

	// Codex variant
	status = "x 💤"
	if isUnread(window) && strings.HasSuffix(status, "💤") {
		status = strings.TrimSuffix(status, "💤") + "📬"
	}
	if status != "x 📬" {
		t.Errorf("expected x 📬, got %q", status)
	}

	// Clean up
	statusStateMu.Lock()
	delete(statusState, window)
	statusStateMu.Unlock()
}

func TestShouldMarkUnread(t *testing.T) {
	tests := []struct {
		name          string
		wasWorking    bool
		activity      bool
		focused       bool
		isWorking     bool
		rawStatus     string
		paneAttention bool
		want          bool
	}{
		{
			name:       "working to idle unfocused",
			wasWorking: true, rawStatus: "x 💤", want: true,
		},
		{
			name:     "tmux activity flag",
			activity: true, rawStatus: "x 💤", want: true,
		},
		{
			name:          "pay attention prompt",
			paneAttention: true, rawStatus: "x 💤", want: true,
		},
		{
			name:          "focused clears attention",
			focused:       true,
			paneAttention: true,
			rawStatus:     "x 💤",
			want:          false,
		},
		{
			name:          "still working",
			isWorking:     true,
			paneAttention: true,
			rawStatus:     "x 🧠",
			want:          false,
		},
		{
			name:      "empty status",
			rawStatus: "",
			want:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shouldMarkUnread(
				tt.wasWorking,
				tt.activity,
				tt.focused,
				tt.isWorking,
				tt.rawStatus,
				tt.paneAttention,
			)
			if got != tt.want {
				t.Errorf("shouldMarkUnread() = %v, want %v", got, tt.want)
			}
		})
	}
}

// --- Benchmarks ---

func BenchmarkBuildChildMap(b *testing.B) {
	for i := 0; i < b.N; i++ {
		buildChildMap()
	}
}

func BenchmarkClassifyPaneContent(b *testing.B) {
	content := "· Brewing… (1m 20s · ↓ 1.8k tokens)\n  (esc to interrupt)\n❯ \n"
	for i := 0; i < b.N; i++ {
		classifyPaneContent(content)
	}
}
