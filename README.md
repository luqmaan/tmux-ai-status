# tmux-ai-status

A lightweight daemon that shows what your AI coding agent (Claude Code, Codex CLI) is doing right in your tmux tab names.

```
 1:🧠  2:🔨  3:💤  4:cx ⚙️  5:zsh
```

| Emoji | Meaning |
|-------|---------|
| 🧠 | Agent is thinking (spinner visible) |
| 💤 | Agent is idle / waiting for input |
| 📬 | Agent finished work while you were in another tab (unread) |
| 🔨 | Building (make, gcc, rustc, webpack, vite, ...) |
| 🧪 | Testing (jest, pytest, vitest, mocha, ...) |
| 📦 | Installing packages (npm, pip, apt, ...) |
| 🔀 | Git operation |
| 🌐 | Network request (curl, wget) |
| ⚙️ | Other subprocess |

Codex CLI tabs get a `cx` prefix (e.g., `cx 🧠`).

When no agent is detected, the tab reverts to the default process name (e.g., `zsh`).

## How it works

Every 2 seconds the daemon:

1. Lists all tmux panes and their shell PIDs
2. Walks `/proc` to build a process tree, finding Claude/Codex agent processes (children or grandchildren of the shell)
3. Inspects the agent's descendant processes to classify what tool is running
4. If no subprocess is running, captures the tmux pane content to detect activity spinners (`Thinking…`, `Brewing…`, etc.) — distinguishing active thinking from idle
5. Updates the tmux tab name with the appropriate emoji

**Anti-flicker:** Two mechanisms prevent tab name flickering:
- **Grace period (10s):** Once a pane is detected as active, it stays marked active for 10 seconds even if a spinner redraw causes a momentary blank frame
- **Hysteresis (2 cycles / 4s):** A new status must be observed for 2 consecutive polling cycles before the tab name is actually updated, filtering out transient states

## Requirements

- Linux (reads `/proc` filesystem)
- tmux
- Go 1.22+ (build only)

## Install

```bash
# Clone
git clone https://github.com/donkeysrus/tmux-ai-status.git
cd tmux-ai-status

# Build & install
make install
```

This compiles the binary and copies it to `~/.local/bin/`.

### tmux configuration

Add to your `~/.tmux.conf`:

```tmux
# Show AI agent status in tab names
set-hook -g session-created 'run-shell -b "pgrep -f tmux-ai-status >/dev/null || ~/.local/bin/tmux-ai-status &"'
```

Then reload tmux config:

```bash
tmux source-file ~/.tmux.conf
```

Or start it manually for a one-off test:

```bash
~/.local/bin/tmux-ai-status &
```

## Supported agents

- [Claude Code](https://docs.anthropic.com/en/docs/claude-code) — detected by `claude` in the process cmdline
- [Codex CLI](https://github.com/openai/codex) — detected by `codex` in the process cmdline

## Uninstall

```bash
pkill -f tmux-ai-status
rm ~/.local/bin/tmux-ai-status
```

Remove the `set-hook` line from `~/.tmux.conf`.

## License

MIT
