package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	// lastActive tracks when each window was last seen as active.
	// Prevents flashing during spinner redraws.
	lastActive   = make(map[string]time.Time)
	lastActiveMu sync.Mutex
	activeGrace  = 10 * time.Second

	// statusState tracks per-window status with hysteresis.
	// A new status must be seen for 2 consecutive cycles before being applied,
	// preventing flicker from brief child processes or spinner redraws.
	statusState   = make(map[string]*windowState)
	statusStateMu sync.Mutex
)

type windowState struct {
	applied string // status currently shown in tmux
	pending string // candidate status seen last cycle
	count   int    // consecutive cycles pending has been seen
	unread  bool   // agent finished work while window was unfocused
}

const stabilityThreshold = 1 // cycles a new status must hold before applying

func main() {
	for {
		updateAllPanes()
		time.Sleep(2 * time.Second)
	}
}

type paneInfo struct {
	window  string
	pid     int
	focused bool
}

// Unread tracking: detect when agent finishes work while user isn't looking.
var (
	windowWasWorking = make(map[string]bool)
	windowFocused    = make(map[string]bool)
)

func listPanes() []paneInfo {
	out, err := exec.Command("tmux", "list-panes", "-a",
		"-F", "#{session_name}:#{window_index} #{pane_pid} #{window_active}").Output()
	if err != nil {
		return nil
	}
	var panes []paneInfo
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 3 {
			continue
		}
		pid, err := strconv.Atoi(fields[1])
		if err != nil {
			continue
		}
		panes = append(panes, paneInfo{window: fields[0], pid: pid, focused: fields[2] == "1"})
	}
	return panes
}

func updateAllPanes() {
	panes := listPanes()
	if len(panes) == 0 {
		return
	}

	childMap := buildChildMap()
	seenWindows := make(map[string]bool)

	// Group panes by window — pick the most significant status per window.
	type windowSummary struct {
		status  string
		focused bool
	}
	summaries := make(map[string]*windowSummary)

	for _, p := range panes {
		seenWindows[p.window] = true
		rawStatus := getStatus(p.window, p.pid, childMap)
		prev, exists := summaries[p.window]
		if !exists {
			summaries[p.window] = &windowSummary{status: rawStatus, focused: p.focused}
		} else {
			prev.focused = prev.focused || p.focused
			if statusPriority(rawStatus) > statusPriority(prev.status) {
				prev.status = rawStatus
			}
		}
	}

	// Apply unread logic per window, then set status.
	for window, s := range summaries {
		rawStatus := s.status
		focused := s.focused
		wasWorking := windowWasWorking[window]
		isWorking := isWorkingStatus(rawStatus)

		// Transition: working → idle while unfocused → mark unread
		if wasWorking && !isWorking && !focused && rawStatus != "" {
			markUnread(window)
		}
		// User focused the window → clear unread
		if focused {
			clearUnread(window)
		}
		// Agent started working again → clear unread
		if isWorking {
			clearUnread(window)
		}

		windowFocused[window] = focused
		windowWasWorking[window] = isWorking

		// Replace 💤 with 📬 if unread
		effectiveStatus := rawStatus
		if !isWorking && rawStatus != "" && isUnread(window) {
			if strings.HasSuffix(rawStatus, "💤") {
				effectiveStatus = strings.TrimSuffix(rawStatus, "💤") + "📬"
			}
		}

		setWindowStatus(window, effectiveStatus)
	}

	// Clean up stale entries
	lastActiveMu.Lock()
	for w := range lastActive {
		if !seenWindows[w] {
			delete(lastActive, w)
		}
	}
	lastActiveMu.Unlock()
	statusStateMu.Lock()
	for w := range statusState {
		if !seenWindows[w] {
			delete(statusState, w)
		}
	}
	statusStateMu.Unlock()
	for w := range windowWasWorking {
		if !seenWindows[w] {
			delete(windowWasWorking, w)
		}
	}
	for w := range windowFocused {
		if !seenWindows[w] {
			delete(windowFocused, w)
		}
	}
}

func isWorkingStatus(status string) bool {
	return status != "" && !strings.HasSuffix(status, "💤")
}

func statusPriority(status string) int {
	if isWorkingStatus(status) {
		return 2
	}
	if status != "" {
		return 1
	}
	return 0
}

func markUnread(window string) {
	statusStateMu.Lock()
	defer statusStateMu.Unlock()
	ws, ok := statusState[window]
	if !ok {
		ws = &windowState{}
		statusState[window] = ws
	}
	ws.unread = true
}

func clearUnread(window string) {
	statusStateMu.Lock()
	defer statusStateMu.Unlock()
	if ws, ok := statusState[window]; ok {
		ws.unread = false
	}
}

func isUnread(window string) bool {
	statusStateMu.Lock()
	defer statusStateMu.Unlock()
	if ws, ok := statusState[window]; ok {
		return ws.unread
	}
	return false
}

// setWindowStatus applies hysteresis: a new status must be seen for
// stabilityThreshold consecutive cycles before the tmux tab is updated.
func setWindowStatus(window, status string) {
	statusStateMu.Lock()
	defer statusStateMu.Unlock()

	ws, ok := statusState[window]
	if !ok {
		ws = &windowState{}
		statusState[window] = ws
	}

	// Already showing this status — nothing to do
	if status == ws.applied {
		ws.pending = ""
		ws.count = 0
		return
	}

	// New candidate status
	if status == ws.pending {
		ws.count++
	} else {
		ws.pending = status
		ws.count = 1
	}

	// Only apply once stable
	if ws.count < stabilityThreshold {
		return
	}

	ws.applied = status
	ws.pending = ""
	ws.count = 0

	if status != "" {
		exec.Command("tmux", "rename-window", "-t", window, status).Run()
	} else {
		exec.Command("tmux", "set-option", "-t", window, "automatic-rename", "on").Run()
	}
}

func buildChildMap() map[int][]int {
	m := make(map[int][]int)
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return m
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		ppid := readPPID(pid)
		if ppid > 0 {
			m[ppid] = append(m[ppid], pid)
		}
	}
	return m
}

func readPPID(pid int) int {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0
	}
	return parsePPIDFromStat(string(data))
}

func parsePPIDFromStat(stat string) int {
	i := strings.LastIndex(stat, ")")
	if i < 0 || i+2 >= len(stat) {
		return 0
	}
	fields := strings.Fields(stat[i+2:])
	if len(fields) < 2 {
		return 0
	}
	ppid, _ := strconv.Atoi(fields[1])
	return ppid
}

func getStatus(window string, panePID int, childMap map[int][]int) string {
	agentPID, agentName := findAgent(panePID, childMap)
	if agentPID == 0 {
		return ""
	}

	prefix := "c "
	if agentName == "codex" {
		prefix = "x "
	}

	descendants := collectDescendants(agentPID, childMap)

	var childNames []string
	for _, d := range descendants {
		name := readComm(d)
		if name == "" || name == "node" || name == "claude" || name == "codex" {
			continue
		}
		childNames = append(childNames, name)
	}

	if len(childNames) > 0 {
		childStatus := classifyChildren(childNames)
		// Codex/Claude can have long-lived helper/background children.
		// If status would be generic "⚙️" but pane is clearly waiting for input,
		// show idle so unread/attention state can surface.
		if childStatus == "⚙️" && paneNeedsAttention(window) {
			return prefix + "💤"
		}
		return prefix + childStatus
	}

	if isPaneActive(window) {
		return prefix + "🧠"
	}
	return prefix + "💤"
}

func paneNeedsAttention(window string) bool {
	out, err := exec.Command("tmux", "capture-pane", "-t", window, "-p").Output()
	if err != nil {
		return false
	}
	return classifyPaneNeedsAttention(string(out))
}

// isPaneActive captures the pane content and checks for activity indicators.
// Uses a grace period to prevent flashing during spinner redraws.
func isPaneActive(window string) bool {
	now := time.Now()
	active := false

	out, err := exec.Command("tmux", "capture-pane", "-t", window, "-p").Output()
	if err == nil {
		active = classifyPaneContent(string(out))
	}

	lastActiveMu.Lock()
	defer lastActiveMu.Unlock()

	if active {
		lastActive[window] = now
		return true
	}

	// Not detected as active right now — check grace period
	if last, ok := lastActive[window]; ok {
		if now.Sub(last) < activeGrace {
			return true
		}
		delete(lastActive, window)
	}
	return false
}

// classifyPaneContent returns true if the pane content indicates active work.
func classifyPaneContent(content string) bool {
	if strings.Contains(content, "esc to interrupt") {
		return true
	}
	// Claude's thinking spinner: "· Leavening… (54s · ...)" or "✢ Transfiguring…" or "* Perusing…"
	// Match "ing…" or "ing..." — covers both with and without time parenthetical.
	if strings.Contains(content, "ing\u2026") || strings.Contains(content, "ing...") {
		return true
	}
	return false
}

// classifyPaneNeedsAttention returns true when the pane appears to be
// waiting for user input (prompt visible) rather than actively working.
func classifyPaneNeedsAttention(content string) bool {
	if classifyPaneContent(content) {
		return false
	}

	lines := strings.Split(content, "\n")
	checked := 0
	for i := len(lines) - 1; i >= 0 && checked < 8; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		checked++
		if strings.HasPrefix(line, "› ") || line == "›" {
			return true
		}
		if strings.HasPrefix(line, "❯ ") || line == "❯" {
			return true
		}
	}
	return false
}

func findAgent(panePID int, childMap map[int][]int) (int, string) {
	for _, child := range childMap[panePID] {
		cmdline := readCmdline(child)
		lower := strings.ToLower(cmdline)
		if strings.Contains(lower, "claude") {
			return child, "claude"
		}
		if strings.Contains(lower, "codex") {
			return child, "codex"
		}
		for _, gc := range childMap[child] {
			cmdline = readCmdline(gc)
			lower = strings.ToLower(cmdline)
			if strings.Contains(lower, "claude") {
				return gc, "claude"
			}
			if strings.Contains(lower, "codex") {
				return gc, "codex"
			}
		}
	}
	return 0, ""
}

func collectDescendants(pid int, childMap map[int][]int) []int {
	var result []int
	queue := append([]int{}, childMap[pid]...)
	for len(queue) > 0 {
		p := queue[0]
		queue = queue[1:]
		result = append(result, p)
		queue = append(queue, childMap[p]...)
	}
	return result
}

func readCmdline(pid int) string {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return ""
	}
	return strings.ReplaceAll(string(data), "\x00", " ")
}

func readComm(pid int) string {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func classifyChildren(names []string) string {
	joined := strings.ToLower(strings.Join(names, "\n"))

	if containsAny(joined, "make", "gcc", "g++", "cc1", "rustc", "javac", "tsc", "webpack", "vite", "esbuild", "rollup") {
		return "🔨"
	}
	if containsAny(joined, "jest", "vitest", "pytest", "mocha", "phpunit", "rspec") {
		return "🧪"
	}
	if containsAny(joined, "npm", "yarn", "pnpm", "pip", "apt", "brew", "pacman") {
		return "📦"
	}
	if containsAny(joined, "git") {
		return "🔀"
	}
	if containsAny(joined, "curl", "wget") {
		return "🌐"
	}
	return "⚙️"
}

func containsAny(s string, substrs ...string) bool {
	for _, sub := range substrs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
