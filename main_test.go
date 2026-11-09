package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

// --- helpers ---------------------------------------------------------------

// touch writes a file whose CONTENT identifies it, padded to size. Zero-filled
// fixtures made the rotation tests filename-only: a shift that moved the wrong
// bytes into a correctly-named slot passed. Assert with wantContent.
func touch(t *testing.T, dir, name string, size int) string {
	t.Helper()
	p := filepath.Join(dir, name)
	tag := []byte("<<" + name + ">>")
	body := make([]byte, size)
	for i := range body {
		body[i] = '.'
	}
	if size >= len(tag) {
		copy(body, tag)
	}
	if err := os.WriteFile(p, body, 0o644); err != nil {
		t.Fatalf("touch %s: %v", name, err)
	}
	return p
}

// wantContent asserts that the file now at `slot` holds the bytes originally
// written to `origin`.
func wantContent(t *testing.T, dir, slot, origin string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, slot))
	if err != nil {
		t.Errorf("%s missing: %v", slot, err)
		return
	}
	if want := "<<" + origin + ">>"; !strings.HasPrefix(string(data), want) {
		t.Errorf("%s holds the wrong bytes: want content of %s, got %.40q", slot, origin, data)
	}
}

func ls(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

// buildBinary compiles the hook once for the end-to-end tests.
func buildBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "claude-hook-logger")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build failed: %v\n%s", err, out)
	}
	return bin
}

func runHook(t *testing.T, bin, dir, stdin string) (int, string) {
	t.Helper()
	cmd := exec.Command(bin)
	cmd.Stdin = strings.NewReader(stdin)
	cmd.Env = append(os.Environ(), "CLAUDE_HOOK_LOG_DIR="+dir)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("run: %v", err)
	}
	return code, stderr.String()
}

// --- end-to-end ------------------------------------------------------------

func TestAppendsValidJSONL(t *testing.T) {
	bin, dir := buildBinary(t), t.TempDir()
	payload := `{"hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"ls -la"}}`
	if code, msg := runHook(t, bin, dir, payload); code != 0 {
		t.Fatalf("exit %d: %s", code, msg)
	}

	data, err := os.ReadFile(filepath.Join(dir, baseName))
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("want 1 line, got %d", len(lines))
	}

	var got struct {
		Timestamp string          `json:"timestamp"`
		HookData  json.RawMessage `json:"hook_data"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &got); err != nil {
		t.Fatalf("line is not valid JSON: %v", err)
	}
	if got.Timestamp == "" {
		t.Error("missing timestamp")
	}
	// The payload must be spliced through byte-for-byte, not re-serialised.
	if string(got.HookData) != payload {
		t.Errorf("hook_data altered:\n got %s\nwant %s", got.HookData, payload)
	}
}

func TestInvalidJSONRejected(t *testing.T) {
	bin, dir := buildBinary(t), t.TempDir()
	code, msg := runHook(t, bin, dir, `{"broken": `)
	if code != 1 {
		t.Errorf("want exit 1, got %d", code)
	}
	if !strings.Contains(msg, "JSON") {
		t.Errorf("want a JSON error on stderr, got %q", msg)
	}
	// A rejected payload must not create a log line.
	if data, err := os.ReadFile(filepath.Join(dir, baseName)); err == nil && len(data) > 0 {
		t.Errorf("wrote %d bytes for invalid input", len(data))
	}
}

func TestConcurrentWritesDoNotInterleave(t *testing.T) {
	bin, dir := buildBinary(t), t.TempDir()
	const writers = 12
	// A payload comfortably larger than PIPE_BUF, where naive appends tear.
	payload := fmt.Sprintf(`{"tool_name":"Bash","blob":%q}`, strings.Repeat("x", 20_000))

	// runHook must not be called from these goroutines: it uses t.Fatalf, which
	// is documented API misuse off the test goroutine. Collect instead.
	var wg sync.WaitGroup
	codes := make(chan int, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cmd := exec.Command(bin)
			cmd.Stdin = strings.NewReader(payload)
			cmd.Env = append(os.Environ(), "CLAUDE_HOOK_LOG_DIR="+dir)
			code := 0
			if err := cmd.Run(); err != nil {
				if ee, ok := err.(*exec.ExitError); ok {
					code = ee.ExitCode()
				} else {
					code = -1
				}
			}
			codes <- code
		}()
	}
	wg.Wait()
	close(codes)
	for code := range codes {
		if code != 0 {
			t.Errorf("concurrent writer exited %d", code)
		}
	}

	data, err := os.ReadFile(filepath.Join(dir, baseName))
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) != writers {
		t.Fatalf("want %d lines, got %d", writers, len(lines))
	}
	for i, ln := range lines {
		if !json.Valid([]byte(ln)) {
			t.Fatalf("line %d torn by concurrent write", i)
		}
	}
}

// --- rotation --------------------------------------------------------------

func withLimits(t *testing.T, bytes int64, archives int) {
	t.Helper()
	ob, oa := maxBytes, maxArchives
	maxBytes, maxArchives = bytes, archives
	t.Cleanup(func() { maxBytes, maxArchives = ob, oa })
}

func TestNoRotationUnderThreshold(t *testing.T) {
	withLimits(t, 1000, 10)
	dir := t.TempDir()
	log := touch(t, dir, baseName, 500)
	if got := rotateIfNeeded(dir, log); got != "" {
		t.Errorf("rotated below threshold, returned %q", got)
	}
	if names := ls(t, dir); len(names) != 1 || names[0] != baseName {
		t.Errorf("dir changed: %v", names)
	}
}

func TestRotationMovesLogToDotOne(t *testing.T) {
	withLimits(t, 1000, 10)
	dir := t.TempDir()
	log := touch(t, dir, baseName, 2000)
	rotateIfNeeded(dir, log)
	if _, err := os.Stat(filepath.Join(dir, baseName+".1")); err != nil {
		t.Errorf("log was not moved to .1: %v", err)
	}
	if _, err := os.Stat(log); !os.IsNotExist(err) {
		t.Error("live log still present after rotation")
	}
}

// The regression that motivated the (number, suffix) keying: gzipped archives
// must shift too, or they orphan and the next `gzip -f` clobbers .2.gz.
func TestGzippedArchivesShiftToo(t *testing.T) {
	withLimits(t, 1000, 10)
	dir := t.TempDir()
	log := touch(t, dir, baseName, 2000)
	touch(t, dir, baseName+".1", 64)
	touch(t, dir, baseName+".2.gz", 64)
	touch(t, dir, baseName+".3.gz", 64)

	toGzip := rotateIfNeeded(dir, log)

	want := []string{baseName + ".1", baseName + ".2", baseName + ".3.gz", baseName + ".4.gz"}
	if got := ls(t, dir); !equal(got, want) {
		t.Errorf("chain wrong:\n got %v\nwant %v", got, want)
	}
	// Names alone can't prove the right bytes landed in each slot.
	wantContent(t, dir, baseName+".1", baseName)
	wantContent(t, dir, baseName+".2", baseName+".1")
	wantContent(t, dir, baseName+".3.gz", baseName+".2.gz")
	wantContent(t, dir, baseName+".4.gz", baseName+".3.gz")
	if toGzip != filepath.Join(dir, baseName+".2") {
		t.Errorf("want .2 queued for gzip, got %q", toGzip)
	}
}

// A plain and a gzipped archive at the same number is the only state the old
// bug could leave behind; each chain must shift independently without collision.
func TestPlainAndGzAtSameNumberDoNotCollide(t *testing.T) {
	withLimits(t, 1000, 10)
	dir := t.TempDir()
	log := touch(t, dir, baseName, 2000)
	touch(t, dir, baseName+".2", 64)
	touch(t, dir, baseName+".2.gz", 64)

	toGzip := rotateIfNeeded(dir, log)

	want := []string{baseName + ".1", baseName + ".3", baseName + ".3.gz"}
	if got := ls(t, dir); !equal(got, want) {
		t.Errorf("collision:\n got %v\nwant %v", got, want)
	}
	// The two chains must shift independently, neither clobbering the other.
	wantContent(t, dir, baseName+".1", baseName)
	wantContent(t, dir, baseName+".3", baseName+".2")
	wantContent(t, dir, baseName+".3.gz", baseName+".2.gz")
	// .2 is now free but nothing sits there, so nothing should be queued.
	if toGzip != "" {
		t.Errorf("unexpected gzip target %q", toGzip)
	}
}

func TestArchiveCapEvictsOldest(t *testing.T) {
	withLimits(t, 1000, 3)
	dir := t.TempDir()
	log := touch(t, dir, baseName, 2000)
	touch(t, dir, baseName+".1", 64)
	touch(t, dir, baseName+".2.gz", 64)
	touch(t, dir, baseName+".3.gz", 64) // 3+1 > 3, must be evicted

	rotateIfNeeded(dir, log)

	for _, name := range ls(t, dir) {
		if strings.HasSuffix(name, ".4.gz") {
			t.Errorf("archive exceeded cap: %s", name)
		}
	}
	// Was near-vacuous on names alone: assert the SURVIVING bytes are .2.gz's,
	// i.e. .3.gz was the file evicted, not some other one.
	wantContent(t, dir, baseName+".3.gz", baseName+".2.gz")
	wantContent(t, dir, baseName+".1", baseName)
	wantContent(t, dir, baseName+".2", baseName+".1")
}

func TestUnrelatedFilesUntouched(t *testing.T) {
	withLimits(t, 1000, 10)
	dir := t.TempDir()
	log := touch(t, dir, baseName, 2000)
	touch(t, dir, "notes.txt", 64)
	touch(t, dir, baseName+".bak", 64)

	rotateIfNeeded(dir, log)

	for _, name := range []string{"notes.txt", baseName + ".bak"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s was disturbed: %v", name, err)
		}
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// --- result truncation -----------------------------------------------------

func TestSmallPayloadsSplicedUntouched(t *testing.T) {
	payload := []byte(`{"tool_name":"Bash","tool_response":{"stdout":"short"}}`)
	if got := shrink(payload); string(got) != string(payload) {
		t.Errorf("small payload altered:\n got %s\nwant %s", got, payload)
	}
}

func TestOversizedResultTruncated(t *testing.T) {
	blob := strings.Repeat("A", 300_000) // a base64 screenshot, roughly
	raw := []byte(fmt.Sprintf(`{"tool_name":"screenshot","tool_response":{"image":%q}}`, blob))

	out := shrink(raw)
	if len(out) >= len(raw) {
		t.Fatalf("no saving: %d -> %d bytes", len(raw), len(out))
	}
	if !json.Valid(out) {
		t.Fatal("truncated payload is not valid JSON")
	}
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	img := doc["tool_response"].(map[string]any)["image"].(string)
	if !strings.Contains(img, "truncated") {
		t.Error("truncation marker missing")
	}
	if len(img) > maxStringBytes+64 {
		t.Errorf("string still %d bytes", len(img))
	}
}

// tool_input carries the edits you would want to recover; it must survive intact
// even when the surrounding payload is huge.
func TestToolInputNeverTruncated(t *testing.T) {
	edit := strings.Repeat("B", 100_000)
	raw := []byte(fmt.Sprintf(`{"tool_input":{"new_string":%q},"tool_response":{"out":%q}}`,
		edit, strings.Repeat("C", 100_000)))

	var doc map[string]any
	if err := json.Unmarshal(shrink(raw), &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got := doc["tool_input"].(map[string]any)["new_string"].(string)
	if got != edit {
		t.Errorf("tool_input truncated: kept %d of %d bytes", len(got), len(edit))
	}
	if out := doc["tool_response"].(map[string]any)["out"].(string); !strings.Contains(out, "truncated") {
		t.Error("tool_response should have been truncated")
	}
}

func TestOversizedButNoLongStringsIsUntouched(t *testing.T) {
	// Big overall, but every individual string is small: re-serialising would
	// only churn key order for no saving.
	var sb strings.Builder
	sb.WriteString(`{"items":[`)
	for i := 0; i < 4000; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		fmt.Fprintf(&sb, `{"k":"v%d"}`, i)
	}
	sb.WriteString(`]}`)
	raw := []byte(sb.String())
	if len(raw) <= truncateOver {
		t.Fatalf("test payload too small: %d bytes", len(raw))
	}
	if got := shrink(raw); string(got) != string(raw) {
		t.Error("payload with no long strings was rewritten")
	}
}

func TestMalformedOversizedPayloadPassedThrough(t *testing.T) {
	raw := []byte(`{"blob":"` + strings.Repeat("x", 40_000)) // >truncateOver, unparseable
	if got := shrink(raw); string(got) != string(raw) {
		t.Error("unparseable payload should pass through unchanged")
	}
}

// --- regressions found in review -------------------------------------------

// The highest-value missing test: json.Valid accepts pretty-printed input, and
// splicing it verbatim wrote a MULTI-LINE record into a JSONL file, corrupting
// every downstream reader — including for lines written by other sessions.
func TestPrettyPrintedInputStaysOneLine(t *testing.T) {
	bin, dir := buildBinary(t), t.TempDir()
	pretty := "{\n  \"hook_event_name\": \"PreToolUse\",\n  \"tool_name\": \"Bash\",\n" +
		"  \"tool_input\": {\n    \"command\": \"ls -la\"\n  }\n}\n"
	if code, msg := runHook(t, bin, dir, pretty); code != 0 {
		t.Fatalf("exit %d: %s", code, msg)
	}
	if code, msg := runHook(t, bin, dir, `{"second":true}`); code != 0 {
		t.Fatalf("exit %d: %s", code, msg)
	}

	data, err := os.ReadFile(filepath.Join(dir, baseName))
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 physical lines, got %d:\n%s", len(lines), data)
	}
	for i, ln := range lines {
		if !json.Valid([]byte(ln)) {
			t.Errorf("line %d is not valid JSON: %.120q", i, ln)
		}
	}
}

// Key order must survive the fast path — that is the reason for compacting
// rather than decoding and re-encoding.
func TestKeyOrderPreservedOnFastPath(t *testing.T) {
	bin, dir := buildBinary(t), t.TempDir()
	payload := `{"zebra":1,"apple":2,"middle":3}`
	if code, msg := runHook(t, bin, dir, payload); code != 0 {
		t.Fatalf("exit %d: %s", code, msg)
	}
	data, _ := os.ReadFile(filepath.Join(dir, baseName))
	if !strings.Contains(string(data), payload) {
		t.Errorf("key order or bytes changed; got %s", data)
	}
}

// json.Unmarshal into `any` decodes numbers as float64, which silently mangles
// nanosecond timestamps and 19-digit IDs on the truncation path.
func TestLargeIntegersSurviveTruncation(t *testing.T) {
	const id = "1753425830829083123"
	raw := []byte(fmt.Sprintf(`{"id":%s,"tool_response":{"blob":%q}}`,
		id, strings.Repeat("z", 100_000)))
	out := shrink(raw)
	if !strings.Contains(string(out), `"id":`+id) {
		t.Errorf("integer lost precision; want %s in output, got %.200s", id, out)
	}
}

// The default encoder escapes < > &, which would make shell commands in
// oversized payloads un-greppable — and .1 is kept plain precisely for grepping.
func TestNoHTMLEscapingOnTruncatedPath(t *testing.T) {
	raw := []byte(fmt.Sprintf(`{"cmd":"grep -R \"a<b && c\" .","tool_response":{"blob":%q}}`,
		strings.Repeat("z", 100_000)))
	out := string(shrink(raw))
	if strings.Contains(out, `\u003c`) || strings.Contains(out, `\u0026`) {
		t.Errorf("HTML-escaped output is un-greppable: %.160s", out)
	}
	if !strings.Contains(out, "a<b && c") {
		t.Errorf("command not greppable in output: %.160s", out)
	}
}

// Truncating a string barely over the limit costs more than it saves once the
// marker is appended, and triggers a pointless re-encode that loses key order.
func TestTruncationNeverGrowsPayload(t *testing.T) {
	var sb strings.Builder
	sb.WriteString(`{"a":[`)
	for i := 0; i < 10; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		fmt.Fprintf(&sb, "%q", strings.Repeat("q", maxStringBytes+1))
	}
	sb.WriteString(`]}`)
	raw := []byte(sb.String())
	if out := shrink(raw); len(out) > len(raw) {
		t.Errorf("payload grew: %d -> %d bytes", len(raw), len(out))
	}
}

func TestMultiByteUTF8TruncatedCleanly(t *testing.T) {
	// 3-byte runes guarantee the byte-boundary cut lands mid-rune.
	blob := strings.Repeat("日", 50_000)
	raw := []byte(fmt.Sprintf(`{"tool_response":{"text":%q}}`, blob))
	out := shrink(raw)
	if !json.Valid(out) {
		t.Fatal("truncated payload is not valid JSON")
	}
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got := doc["tool_response"].(map[string]any)["text"].(string)
	if !utf8.ValidString(got) {
		t.Error("truncation produced invalid UTF-8")
	}
	if strings.ContainsRune(got, utf8.RuneError) {
		t.Error("truncation left a replacement character at the cut")
	}
}

// A .0 archive (only ever hand-created) would shift into .1 and then be
// overwritten by the live log, destroying its contents.
func TestStrayZeroArchiveIsIgnored(t *testing.T) {
	withLimits(t, 1000, 10)
	dir := t.TempDir()
	log := touch(t, dir, baseName, 2000)
	touch(t, dir, baseName+".0", 64)

	rotateIfNeeded(dir, log)

	wantContent(t, dir, baseName+".0", baseName+".0") // untouched
	wantContent(t, dir, baseName+".1", baseName)      // live log, not clobbered
}

func TestLeadingZeroArchiveIsIgnored(t *testing.T) {
	withLimits(t, 1000, 10)
	dir := t.TempDir()
	log := touch(t, dir, baseName, 2000)
	touch(t, dir, baseName+".1", 64)
	touch(t, dir, baseName+".01", 64) // also parses as 1; would collide

	rotateIfNeeded(dir, log)

	wantContent(t, dir, baseName+".01", baseName+".01") // skipped, intact
	wantContent(t, dir, baseName+".2", baseName+".1")   // real .1 shifted
	wantContent(t, dir, baseName+".1", baseName)
}

// End-to-end rotation through the built binary, reachable now that the limits
// are env-overridable. Also pins that the next event starts a fresh log.
func TestRotationEndToEnd(t *testing.T) {
	bin, dir := buildBinary(t), t.TempDir()
	env := []string{"CLAUDE_HOOK_LOG_DIR=" + dir, "CLAUDE_HOOK_MAX_BYTES=2000"}
	payload := fmt.Sprintf(`{"tool_name":"Bash","blob":%q}`, strings.Repeat("y", 800))

	for i := 0; i < 6; i++ {
		cmd := exec.Command(bin)
		cmd.Stdin = strings.NewReader(payload)
		cmd.Env = append(os.Environ(), env...)
		if err := cmd.Run(); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}

	if _, err := os.Stat(filepath.Join(dir, baseName+".1")); err != nil {
		t.Fatalf("expected a rotated .1: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, baseName))
	if err != nil {
		t.Fatalf("live log missing after rotation: %v", err)
	}
	for i, ln := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if !json.Valid([]byte(ln)) {
			t.Errorf("post-rotation line %d invalid: %.80q", i, ln)
		}
	}
}

func TestEmptyStdinIsRejectedNotBlocking(t *testing.T) {
	bin, dir := buildBinary(t), t.TempDir()
	code, _ := runHook(t, bin, dir, "")
	// Exit 1 is a non-blocking error; exit 2 would BLOCK the user's tool call.
	if code != 1 {
		t.Errorf("want exit 1 for empty stdin, got %d", code)
	}
}

func TestMissingLogDirIsCreated(t *testing.T) {
	bin := buildBinary(t)
	dir := filepath.Join(t.TempDir(), "deeply", "nested", "logs")
	if code, msg := runHook(t, bin, dir, `{"a":1}`); code != 0 {
		t.Fatalf("exit %d: %s", code, msg)
	}
	if _, err := os.Stat(filepath.Join(dir, baseName)); err != nil {
		t.Errorf("log not created in missing tree: %v", err)
	}
}

// --- manual rotation -------------------------------------------------------

func TestManualRotateForcesRegardlessOfSize(t *testing.T) {
	bin, dir := buildBinary(t), t.TempDir()
	// Well under any threshold: only --rotate should move this.
	if code, msg := runHook(t, bin, dir, `{"first":true}`); code != 0 {
		t.Fatalf("seed exit %d: %s", code, msg)
	}

	cmd := exec.Command(bin, "--rotate")
	cmd.Env = append(os.Environ(), "CLAUDE_HOOK_LOG_DIR="+dir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("--rotate failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "rotated") {
		t.Errorf("no rotation reported: %s", out)
	}

	data, err := os.ReadFile(filepath.Join(dir, baseName+".1"))
	if err != nil {
		t.Fatalf(".1 missing after --rotate: %v", err)
	}
	if !strings.Contains(string(data), `"first":true`) {
		t.Error(".1 does not hold the pre-rotation content")
	}
	// The live log must be gone, and the next event must start a fresh one.
	if _, err := os.Stat(filepath.Join(dir, baseName)); !os.IsNotExist(err) {
		t.Error("live log still present immediately after rotation")
	}
	if code, msg := runHook(t, bin, dir, `{"second":true}`); code != 0 {
		t.Fatalf("post-rotate exit %d: %s", code, msg)
	}
	fresh, err := os.ReadFile(filepath.Join(dir, baseName))
	if err != nil {
		t.Fatalf("fresh log not created: %v", err)
	}
	if strings.Contains(string(fresh), `"first":true`) {
		t.Error("fresh log contains pre-rotation content")
	}
	if !json.Valid([]byte(strings.TrimRight(string(fresh), "\n"))) {
		t.Error("fresh log line is not valid JSON")
	}
}

func TestManualRotateOnMissingLogIsHarmless(t *testing.T) {
	bin, dir := buildBinary(t), t.TempDir()
	cmd := exec.Command(bin, "--rotate")
	cmd.Env = append(os.Environ(), "CLAUDE_HOOK_LOG_DIR="+dir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Errorf("want exit 0 when there is nothing to rotate, got %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "nothing to rotate") {
		t.Errorf("unhelpful message: %s", out)
	}
}

func TestUnknownArgExitsNonZeroWithUsage(t *testing.T) {
	bin := buildBinary(t)
	cmd := exec.Command(bin, "--bogus")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Error("want non-zero exit for an unknown argument")
	}
	if !strings.Contains(string(out), "claude-hook-logger --rotate") {
		t.Errorf("usage not shown: %s", out)
	}
}

// --rotate must not consume stdin: it is run by hand, not by the harness.
func TestManualRotateIgnoresStdin(t *testing.T) {
	bin, dir := buildBinary(t), t.TempDir()
	runHook(t, bin, dir, `{"seed":true}`)
	cmd := exec.Command(bin, "--rotate")
	cmd.Stdin = strings.NewReader(`{"should":"be ignored"}`)
	cmd.Env = append(os.Environ(), "CLAUDE_HOOK_LOG_DIR="+dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("--rotate with stdin failed: %v\n%s", err, out)
	}
	data, _ := os.ReadFile(filepath.Join(dir, baseName+".1"))
	if strings.Contains(string(data), "be ignored") {
		t.Error("--rotate wrote its stdin into the log")
	}
}
