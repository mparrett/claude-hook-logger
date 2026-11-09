// Command claude-hook-logger appends Claude Code (and Cursor) hook events to a rotating
// JSONL log. It is a port of claude_hook_logger.py, kept because the Python
// version paid ~18ms of interpreter startup on every hook event; this binary
// does the same work in ~3ms. That matters because PreToolUse blocks the agent
// loop on every single tool call.
//
// Behaviour is deliberately identical to the Python version, including the
// rotation chain and its edge cases. See rotateIfNeeded for the one that bit us.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"syscall"
	"time"
	"unicode/utf8"
)

const baseName = "hook_events.log"

// vars, not consts, so tests can shrink them instead of writing 50MB files.
// The env overrides also let end-to-end tests drive rotation through the built
// binary, which package vars alone cannot reach.
var (
	maxBytes    int64 = 50_000_000 // rotate once the live log exceeds this
	maxArchives       = 10         // keep at most this many rotated files (.1 .. .N[.gz])

	// Payloads under this are compacted and written as-is — the fast path, and
	// the overwhelming majority of events. Only whales get decoded.
	truncateOver = 32 * 1024
	// Longest string kept inside an oversized payload. Base64 screenshots ran
	// ~292KB each in the Python-era log; file reads averaged 16KB.
	maxStringBytes = 4 * 1024
)

// keepWhole names subtrees that are never truncated. tool_input holds the edits
// and commands you would actually want to recover from a log; tool results are
// reproducible output and are not worth the disk.
var keepWhole = map[string]bool{"tool_input": true}

func init() {
	if v := os.Getenv("CLAUDE_HOOK_MAX_BYTES"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			maxBytes = n
		}
	}
	if v := os.Getenv("CLAUDE_HOOK_MAX_ARCHIVES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			maxArchives = n
		}
	}
}

// logDir honours an env override so tests never touch the real log.
func logDir() (string, error) {
	if d := os.Getenv("CLAUDE_HOOK_LOG_DIR"); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		// Never fall back to a relative path: that would silently create a log
		// directory inside whatever repo the agent happens to be working in.
		return "", fmt.Errorf("cannot determine home directory: %v", err)
	}
	return filepath.Join(home, "logs", "claude-hooks"), nil
}

var archivePattern = regexp.MustCompile(`^` + regexp.QuoteMeta(baseName) + `\.(\d+)(\.gz)?$`)

type archive struct {
	num    int
	suffix string // "" or ".gz"
	path   string
}

// rotateIfNeeded rotates the log when it exceeds maxBytes, returning the path
// that should be gzipped (outside the lock), or "" if there's nothing to do.
//
// Steady-state chain (.1 stays plain for grepping, older archives gzipped):
//
//	hook_events.log      -> hook_events.log.1
//	hook_events.log.1    -> hook_events.log.2    (returned for async gzip)
//	hook_events.log.N.gz -> hook_events.log.N+1.gz
//
// Keying by (number, suffix) rather than number alone is load-bearing: an
// earlier Python version matched only the plain \d+ pattern, so gzipped files
// were invisible to rotation. They orphaned and accumulated forever, and the
// next rotation's `gzip -f` silently overwrote an existing .2.gz. Shifting each
// chain independently means a stray plain+gz pair at the same number — the only
// state the old bug could leave behind — can't collide.
//
// Note maxArchives bounds the archive NUMBER, not the file count: because the
// plain and gz chains are independent, .N and .N.gz can both exist at a slot, so
// the true ceiling is 2*maxArchives files. In practice the plain chain only ever
// holds .1 (and transiently .2), so this doesn't bite.
func rotateIfNeeded(dir, logPath string) string {
	return rotate(dir, logPath, false)
}

// rotate performs the chain shift. force skips the size check, for `--rotate`.
func rotate(dir, logPath string, force bool) string {
	st, err := os.Stat(logPath)
	if err != nil {
		return ""
	}
	if !force && st.Size() <= maxBytes {
		return ""
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	var archives []archive
	for _, e := range entries {
		m := archivePattern.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		n, err := strconv.Atoi(m[1])
		// Skip .0 and leading-zero forms ("01" also parses as 1). Both would
		// collide with a real slot during the shift and silently destroy data;
		// a hand-created file is the only way they appear.
		if err != nil || n < 1 || m[1] != strconv.Itoa(n) {
			continue
		}
		archives = append(archives, archive{n, m[2], filepath.Join(dir, e.Name())})
	}

	// Age out anything that would exceed the cap after the shift.
	kept := archives[:0]
	for _, a := range archives {
		if a.num+1 > maxArchives {
			os.Remove(a.path)
			continue
		}
		kept = append(kept, a)
	}
	archives = kept

	// Shift every archive up by one, highest number first so a rename never
	// lands on an occupied slot; the .gz suffix rides along.
	sort.Slice(archives, func(i, j int) bool { return archives[i].num > archives[j].num })
	for _, a := range archives {
		os.Rename(a.path, filepath.Join(dir, fmt.Sprintf("%s.%d%s", baseName, a.num+1, a.suffix)))
	}

	// Current log becomes .1 (plain, kept greppable).
	if err := os.Rename(logPath, filepath.Join(dir, baseName+".1")); err != nil {
		return ""
	}

	// Compress the previous log (now at .2) unless a .2.gz already holds that
	// slot — only possible mid-heal from the old bug's leftover state.
	plain2 := filepath.Join(dir, baseName+".2")
	gz2 := filepath.Join(dir, baseName+".2.gz")
	if _, err := os.Stat(plain2); err == nil {
		if _, err := os.Stat(gz2); os.IsNotExist(err) {
			return plain2
		}
	}
	return ""
}

// truncateLarge walks a decoded payload and shortens any string longer than
// maxStringBytes, leaving the keepWhole subtrees alone. It reports whether
// anything changed so the caller can keep the original bytes when it didn't.
func truncateLarge(v any) (any, bool) {
	switch t := v.(type) {
	case string:
		marker := fmt.Sprintf("…[truncated, %d bytes total]", len(t))
		// Truncating a string only slightly over the limit costs more than it
		// saves once the marker is added.
		if len(t) <= maxStringBytes+len(marker) {
			return t, false
		}
		// Cut back to a rune boundary so the kept prefix stays valid UTF-8.
		// Without this the final rune is left dangling and the encoder
		// substitutes U+FFFD.
		cut := maxStringBytes
		for cut > 0 && !utf8.RuneStart(t[cut]) {
			cut--
		}
		return t[:cut] + marker, true
	case map[string]any:
		changed := false
		for k, sub := range t {
			if keepWhole[k] {
				continue
			}
			nv, c := truncateLarge(sub)
			if c {
				t[k], changed = nv, true
			}
		}
		return t, changed
	case []any:
		changed := false
		for i, sub := range t {
			nv, c := truncateLarge(sub)
			if c {
				t[i], changed = nv, true
			}
		}
		return t, changed
	}
	return v, false
}

// shrink returns the bytes to log. Oversized payloads are decoded, truncated and
// re-encoded; everything else is returned untouched. Re-encoding loses original
// key order, which is why it is confined to the payloads where the disk saving
// is worth it.
func shrink(raw []byte) []byte {
	if len(raw) <= truncateOver {
		return raw
	}
	// UseNumber keeps integers exact. Decoding into `any` normally turns every
	// number into a float64, which mangles nanosecond timestamps and 19-digit
	// IDs — precisely the fields worth grepping for.
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return raw
	}
	out, changed := truncateLarge(doc)
	if !changed {
		return raw
	}
	// SetEscapeHTML(false): the default escapes < > &, which would render shell
	// commands in oversized payloads un-greppable — and .1 is kept plain
	// precisely so it can be grepped.
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(out); err != nil {
		return raw
	}
	return bytes.TrimRight(buf.Bytes(), "\n") // Encode appends a newline
}

func run() error {
	raw, err := io.ReadAll(os.Stdin)
	if err != nil {
		return fmt.Errorf("reading stdin: %w", err)
	}

	// Compact rather than merely validate. json.Valid accepts pretty-printed
	// input, and splicing that verbatim would write a multi-line record into a
	// JSONL file — corrupting every downstream reader, including for lines
	// written by other sessions. Compacting also preserves key order, unlike a
	// decode/encode round trip.
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		return fmt.Errorf("parsing JSON input: %w (%d bytes)", err, len(raw))
	}
	raw = shrink(compact.Bytes())

	dir, err := logDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating log dir: %w", err)
	}
	logPath := filepath.Join(dir, baseName)

	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("opening log: %w", err)
	}

	// Exclusive lock so concurrent sessions (Claude Code and Cursor both write
	// here) never interleave a line. Rotation happens under the lock; gzip runs
	// after it is released.
	//
	// Known race, carried over deliberately: if rotateIfNeeded renames the file,
	// this handle still points at the old inode, so the entry lands in .log.1
	// instead of the fresh log. Single-writer in practice, and harmless.
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return fmt.Errorf("locking log: %w", err)
	}

	toGzip := rotateIfNeeded(dir, logPath)

	// Python's datetime.now().isoformat(): local time, microseconds, no offset.
	ts := time.Now().Format("2006-01-02T15:04:05.000000")
	var buf []byte
	buf = append(buf, `{"timestamp": "`...)
	buf = append(buf, ts...)
	buf = append(buf, `", "hook_data": `...)
	buf = append(buf, raw...)
	buf = append(buf, "}\n"...)
	_, writeErr := f.Write(buf)
	closeErr := f.Close() // releases the flock

	if writeErr != nil {
		return fmt.Errorf("writing log: %w", writeErr)
	}
	if closeErr != nil {
		return fmt.Errorf("closing log: %w", closeErr)
	}

	// Detached, exactly as the Python version did. Compressing a 50MB file
	// in-process would block the hook — and therefore the agent — for seconds.
	// Start() must be checked: on a failure (no gzip on PATH) cmd.Process is
	// nil, and touching it would panic. A panic exits 2, which PreToolUse reads
	// as "block this tool call".
	if toGzip != "" {
		if _, err := os.Stat(toGzip); err == nil {
			cmd := exec.Command("gzip", "-f", toGzip)
			cmd.Stdout, cmd.Stderr = nil, nil
			_ = cmd.Start() // best effort; a missing gzip just leaves .2 plain
		}
	}
	return nil
}

// rotateNow forces a rotation from the command line, independent of size. Unlike
// the hook path it waits for gzip, so the reported state is the final one.
func rotateNow() error {
	dir, err := logDir()
	if err != nil {
		return err
	}
	logPath := filepath.Join(dir, baseName)
	if _, err := os.Stat(logPath); err != nil {
		fmt.Printf("nothing to rotate: %s does not exist\n", logPath)
		return nil
	}

	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("opening log: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return fmt.Errorf("locking log: %w", err)
	}
	toGzip := rotate(dir, logPath, true)
	f.Close() // releases the flock

	fmt.Printf("rotated %s -> %s.1\n", logPath, baseName)
	if toGzip != "" {
		// Synchronous here: a manual command should not report success while
		// compression is still in flight.
		if err := exec.Command("gzip", "-f", toGzip).Run(); err != nil {
			fmt.Fprintf(os.Stderr, "warning: gzip failed (%v); %s left uncompressed\n",
				err, filepath.Base(toGzip))
		} else {
			fmt.Printf("compressed %s.gz\n", filepath.Base(toGzip))
		}
	}
	return nil
}

const usage = `claude-hook-logger - append Claude Code / Cursor hook events to a rotating JSONL log

  claude-hook-logger            read one hook payload on stdin and append it (hook mode)
  claude-hook-logger --rotate   force a rotation now, regardless of size
  claude-hook-logger --help     this message

Env: CLAUDE_HOOK_LOG_DIR, CLAUDE_HOOK_MAX_BYTES, CLAUDE_HOOK_MAX_ARCHIVES
`

func main() {
	// A panic would exit 2, and PreToolUse treats exit 2 as "block this tool
	// call" — so an observability sidecar could deny the user's edits. Exit 1
	// instead: visible as a non-blocking error, never blocking.
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(os.Stderr, "claude-hook-logger panic: %v\n", r)
			os.Exit(1)
		}
	}()

	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "--rotate":
			if err := rotateNow(); err != nil {
				fmt.Fprintf(os.Stderr, "Error rotating log: %v\n", err)
				os.Exit(1)
			}
			return
		case "-h", "--help":
			fmt.Print(usage)
			return
		default:
			fmt.Fprintf(os.Stderr, "unknown argument %q\n\n%s", os.Args[1], usage)
			os.Exit(1)
		}
	}

	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error logging hook event: %v\n", err)
		os.Exit(1)
	}
}
