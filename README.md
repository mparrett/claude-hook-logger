# claude-hook-logger

Logs every [Claude Code](https://claude.com/claude-code) hook event to a rotating
JSONL file. Cursor writes the same hook format, so one log captures both.

```
{"timestamp": "2026-07-25T08:39:59.142605", "hook_data": {"hook_event_name": "PreToolUse", ...}}
```

## Why a binary

The predecessor was a Python script. Python spent ~18 ms on interpreter startup
before running a line of its own code, and `PreToolUse` blocks the agent loop on
every single tool call.

| | median |
|---|---|
| Python (`uv run`) | 30.6 ms |
| claude-hook-logger (Go) | **3.1 ms** |

Trimming Python imports only reached ~22 ms; the startup floor is not reachable from
Python at all.

## Install the binary

Requires Go 1.26+.

```sh
go install github.com/mparrett/claude-hook-logger@latest
```

That puts it at `$(go env GOPATH)/bin/claude-hook-logger` (usually
`~/go/bin/claude-hook-logger`). Pin a tag instead of `@latest` if you want upgrades
to be deliberate: the hooks below run it on every tool call.

From a clone: `go build -ldflags="-s -w" .`

## Usage

```sh
claude-hook-logger            # read one hook payload on stdin and append it (hook mode)
claude-hook-logger --rotate   # force a rotation now, regardless of size
claude-hook-logger --help
```

No dependencies outside the standard library. `gzip` on `PATH` is optional — without
it, rotated archives simply stay uncompressed.

## Wire up the hooks

Point every hook event at the binary in `~/.claude/settings.json`. Absolute path
required:

```json
{
  "hooks": {
    "PreToolUse":       [{"matcher": "*", "hooks": [{"type": "command", "command": "/abs/path/to/claude-hook-logger"}]}],
    "PostToolUse":      [{"matcher": "*", "hooks": [{"type": "command", "command": "/abs/path/to/claude-hook-logger"}]}],
    "UserPromptSubmit": [{"hooks": [{"type": "command", "command": "/abs/path/to/claude-hook-logger"}]}],
    "SessionStart":     [{"matcher": "*", "hooks": [{"type": "command", "command": "/abs/path/to/claude-hook-logger"}]}],
    "SessionEnd":       [{"hooks": [{"type": "command", "command": "/abs/path/to/claude-hook-logger"}]}],
    "Stop":             [{"hooks": [{"type": "command", "command": "/abs/path/to/claude-hook-logger"}]}],
    "SubagentStop":     [{"hooks": [{"type": "command", "command": "/abs/path/to/claude-hook-logger"}]}],
    "Notification":     [{"hooks": [{"type": "command", "command": "/abs/path/to/claude-hook-logger"}]}],
    "PreCompact":       [{"matcher": "*", "hooks": [{"type": "command", "command": "/abs/path/to/claude-hook-logger"}]}]
  }
}
```

Verify with `tail -1 ~/logs/claude-hooks/hook_events.log` after any tool call.

**The binary must exist at that path or every hook fails.** Reinstall after
switching architecture.

## Configuration

All optional.

| Variable | Default | Effect |
|---|---|---|
| `CLAUDE_HOOK_LOG_DIR` | `~/logs/claude-hooks` | Where the log lives |
| `CLAUDE_HOOK_MAX_BYTES` | `50000000` | Rotate once the live log exceeds this |
| `CLAUDE_HOOK_MAX_ARCHIVES` | `10` | Highest archive number kept |

## Behaviour

**Rotation.** At the size threshold: `hook_events.log` → `.1`, `.1` → `.2` (gzipped in
a detached process), `.N.gz` → `.N+1.gz`. `.1` deliberately stays uncompressed so it
can be grepped. Archives past the cap are removed.

`--rotate` forces the same chain shift immediately, ignoring the size threshold, and
waits for gzip so the reported state is final:

```
$ claude-hook-logger --rotate
rotated /Users/you/logs/claude-hooks/hook_events.log -> hook_events.log.1
compressed hook_events.log.2.gz
```

It never reads stdin, and it is a no-op with a clear message when there is no live log
to rotate. The next hook event starts a fresh one.

**Concurrency.** Writes are serialised with `flock`, so concurrent Claude Code and
Cursor sessions never interleave a line.

**Truncation.** In payloads over 32 KB, individual strings over 4 KB are truncated
with a byte-count marker — base64 screenshots were averaging 292 KB each. `tool_input`
is never truncated: it holds the edits and commands worth recovering, whereas results
are reproducible. On a real 400-event sample this cut 10.4 MB to 0.8 MB. Payloads
under 32 KB are compacted and written byte-for-byte, preserving key order.

**Failure.** Exits 1 with a message on stderr, which Claude Code surfaces as a
non-blocking error. A `recover()` guarantees a panic can never exit 2, which
`PreToolUse` would interpret as *block this tool call*.

## Development

```sh
go test ./...
go test -race ./...
```

The tests worth knowing: `TestPrettyPrintedInputStaysOneLine` (indented input must not
produce a multi-line record), `TestGzippedArchivesShiftToo` and
`TestPlainAndGzAtSameNumberDoNotCollide` (the rotation chain's real edge cases),
`TestConcurrentWritesDoNotInterleave`, and `TestLargeIntegersSurviveTruncation`
(19-digit IDs must not become floats).
