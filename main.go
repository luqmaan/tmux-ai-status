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
	window   string
	pid      int
	focused  bool
	activity bool
}

// Unread tracking: detect when agent finishes work while user isn't looking.
var (
	windowWasWorking = make(map[string]bool)
	windowFocused    = make(map[string]bool)
	windowSeen       = make(map[string]bool)
	windowPromptSig  = make(map[string]string)
	windowDoneSig    = make(map[string]string)
)

func listPanes() []paneInfo {
	out, err := exec.Command("tmux", "list-panes", "-a",
		"-F", "#{session_name}:#{window_index} #{pane_pid} #{window_active} #{window_activity_flag}").Output()
	if err != nil {
		return nil
	}
	var panes []paneInfo
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 4 {
			continue
		}
		pid, err := strconv.Atoi(fields[1])
		if err != nil {
			continue
		}
		panes = append(panes, paneInfo{
			window:   fields[0],
			pid:      pid,
			focused:  fields[2] == "1",
			activity: fields[3] == "1",
		})
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
		status   string
		focused  bool
		activity bool
	}
	summaries := make(map[string]*windowSummary)

	for _, p := range panes {
		seenWindows[p.window] = true
		rawStatus := getStatus(p.window, p.pid, childMap)
		prev, exists := summaries[p.window]
		if !exists {
			summaries[p.window] = &windowSummary{status: rawStatus, focused: p.focused, activity: p.activity}
		} else {
			prev.focused = prev.focused || p.focused
			prev.activity = prev.activity || p.activity
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
		seenBefore := windowSeen[window]
		promptSig := ""
		doneSig := ""
		if !isWorking && rawStatus != "" {
			promptSig, doneSig = paneSignals(window)
		}
		prevPromptSig := windowPromptSig[window]
		prevDoneSig := windowDoneSig[window]

		// Mark unread only for meaningful events:
		// - working -> idle completion while unfocused
		// - new completion/prompt signature after initial baseline
		if shouldMarkUnread(
			wasWorking,
			focused,
			isWorking,
			rawStatus,
			seenBefore,
			promptSig,
			prevPromptSig,
			doneSig,
			prevDoneSig,
		) {
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
		windowSeen[window] = true
		windowPromptSig[window] = promptSig
		windowDoneSig[window] = doneSig

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
	for w := range windowSeen {
		if !seenWindows[w] {
			delete(windowSeen, w)
		}
	}
	for w := range windowPromptSig {
		if !seenWindows[w] {
			delete(windowPromptSig, w)
		}
	}
	for w := range windowDoneSig {
		if !seenWindows[w] {
			delete(windowDoneSig, w)
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

func shouldMarkUnread(
	wasWorking, focused, isWorking bool,
	rawStatus string,
	seenBefore bool,
	promptSig, prevPromptSig, doneSig, prevDoneSig string,
) bool {
	if focused || isWorking || rawStatus == "" {
		return false
	}
	if wasWorking {
		return true
	}
	if !seenBefore {
		return false
	}
	if doneSig != "" && doneSig != prevDoneSig {
		return true
	}
	if promptSig != "" && promptSig != prevPromptSig {
		return true
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

	var childSignals []string
	for _, d := range descendants {
		comm := strings.ToLower(readComm(d))
		cmdline := strings.ToLower(readCmdline(d))
		if isAgentLikeProcess(comm, cmdline) {
			continue
		}
		signal := cmdline
		if signal == "" {
			signal = comm
		}
		if signal == "" {
			continue
		}
		childSignals = append(childSignals, signal)
	}

	if len(childSignals) > 0 {
		childStatus := classifyChildren(childSignals)
		if childStatus == "⚙️" {
			// Unknown live child process still means active work.
			return prefix + "🧠"
		}
		return prefix + childStatus
	}

	// If no child process is active, prompt means idle/waiting.
	if paneNeedsAttention(window) {
		return prefix + "💤"
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

func paneSignals(window string) (promptSig, doneSig string) {
	out, err := exec.Command("tmux", "capture-pane", "-t", window, "-p").Output()
	if err != nil {
		return "", ""
	}
	content := string(out)
	return classifyPaneAttentionSignature(content), classifyPaneCompletionSignature(content)
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
	lines := strings.Split(content, "\n")
	checked := 0
	for i := len(lines) - 1; i >= 0 && checked < 12; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		checked++

		// Explicit completion markers mean the run is done.
		if isCompletionLine(line) {
			return false
		}
		if hasActiveMarker(line) {
			return true
		}
	}
	return false
}

func hasSpinnerMarker(line string) bool {
	return strings.HasPrefix(line, "· ") ||
		strings.HasPrefix(line, "• ") ||
		strings.HasPrefix(line, "✢ ") ||
		strings.HasPrefix(line, "✻ ") ||
		strings.HasPrefix(line, "* ")
}

func hasActiveMarker(line string) bool {
	if strings.Contains(line, "esc to interrupt") {
		return true
	}
	if !hasSpinnerMarker(line) {
		return false
	}
	// Claude/Codex spinner verbs: "Thinking…", "Brewing...", "Perusing…", etc.
	return strings.Contains(line, "ing\u2026") || strings.Contains(line, "ing...")
}

// classifyPaneNeedsAttention returns true when the pane appears to be
// waiting for user input (prompt visible) rather than actively working.
func classifyPaneNeedsAttention(content string) bool {
	return classifyPaneAttentionSignature(content) != ""
}

func classifyPaneAttentionSignature(content string) string {
	if classifyPaneContent(content) {
		return ""
	}
	lines := strings.Split(content, "\n")
	checked := 0
	for i := len(lines) - 1; i >= 0 && checked < 12; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		checked++
		if strings.HasPrefix(line, "› ") || line == "›" {
			return "codex:" + line
		}
		if strings.HasPrefix(line, "❯ ") || line == "❯" {
			return "claude:" + line
		}
	}
	return ""
}

func classifyPaneCompletionSignature(content string) string {
	lines := strings.Split(content, "\n")
	checked := 0
	for i := len(lines) - 1; i >= 0 && checked < 20; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		checked++
		if isCompletionLine(line) {
			return line
		}
	}
	return ""
}

func isCompletionLine(line string) bool {
	return strings.HasPrefix(line, "─ Worked for ") ||
		line == "Done." || strings.HasPrefix(line, "Done. ") ||
		line == "All set." || strings.HasPrefix(line, "All set. ")
}

func isPromptWithText(line string) bool {
	if strings.HasPrefix(line, "›") {
		return strings.TrimSpace(strings.TrimPrefix(line, "›")) != ""
	}
	if strings.HasPrefix(line, "❯") {
		return strings.TrimSpace(strings.TrimPrefix(line, "❯")) != ""
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

	if containsAny(
		joined,
		"make", "gcc", "g++", "cc1", "rustc", "javac", "tsc", "webpack", "vite", "esbuild", "rollup",
		"coordinator/cli.ts build", " next build", "npm run build", "pnpm run build", "yarn build", "go build", "cargo build",
	) {
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

func isAgentLikeProcess(comm, cmdline string) bool {
	if comm == "" && cmdline == "" {
		return true
	}
	if strings.Contains(comm, "codex") || strings.Contains(comm, "claude") || comm == "node" {
		if cmdline == "" || strings.Contains(cmdline, "codex") || strings.Contains(cmdline, "claude") {
			return true
		}
	}
	if strings.Contains(cmdline, "codex") || strings.Contains(cmdline, "claude") {
		return true
	}
	return false
}

func containsAny(s string, substrs ...string) bool {
	for _, sub := range substrs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
