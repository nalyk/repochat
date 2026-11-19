package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nalyk/repochat/internal/mcp"
	"github.com/nalyk/repochat/internal/repo"
)

// With --audit-args the caller's arguments are recorded. A caller can send
// megabytes of them; what is recorded and sent to the launcher must stay
// small, or one call makes the live view and `repochat log` go blind.
func TestRecordedArgumentsAreBounded(t *testing.T) {
	e := setup(t, "")
	r, err := repo.Open(context.Background(), e.repo, repo.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	huge := strings.Repeat("<", 1200<<10)
	many := make([]any, 5000)
	for i := range many {
		many[i] = huge[:4096]
	}
	args := map[string]any{"query": huge, "paths": many, "limit": float64(5), "nested": map[string]any{"deep": huge}}
	for i := 0; i < 500; i++ {
		args[strings.Repeat("k", 300)+string(rune('a'+i%26))+strings.Repeat("x", i)] = "v"
	}
	a := activityFrom(mcp.Event{Kind: "call", Tool: "search_code", Args: args, Time: time.Now()}, &session{AuditArgs: true}, r)
	line, err := json.Marshal(ctlMessage{Op: "event", Activity: &a})
	if err != nil {
		t.Fatal(err)
	}
	if len(line) > 256<<10 {
		t.Fatalf("one recorded call is %d bytes", len(line))
	}
	if a.Args["limit"] != float64(5) {
		t.Errorf("small values must be recorded as they are: %v", a.Args["limit"])
	}
	if q, _ := a.Args["query"].(string); !strings.HasPrefix(q, "<<<<") || !strings.Contains(q, "more bytes") {
		t.Errorf("a long value must be recorded as its beginning plus a note, got %.80q", q)
	}
}

func TestOversizedLinesDoNotEndAStream(t *testing.T) {
	input := "first\n" + strings.Repeat("x", 3<<20) + "\nsecond\nlast without newline"
	var got []string
	skipped, err := eachLine(strings.NewReader(input), 1<<20, func(line []byte) { got = append(got, string(line)) })
	if err != nil || skipped != 1 || strings.Join(got, "|") != "first|second|last without newline" {
		t.Fatalf("got %q, skipped %d, err %v", got, skipped, err)
	}
}

// `repochat log` must show what it can read and say what it could not.
func TestLogSurvivesAnOversizedEntry(t *testing.T) {
	setup(t, "")
	a, err := openAudit()
	if err != nil {
		t.Fatal(err)
	}
	entry := func(tool string) string {
		raw, _ := json.Marshal(activity{Time: time.Now(), Kind: "call", Tool: tool, OK: true})
		return string(raw) + "\n"
	}
	content := entry("git_status") + `{"kind":"call","tool":"search_code","args":{"query":"` + strings.Repeat("x", 5<<20) + `"}}` + "\n" + entry("read_file")
	if err := os.WriteFile(a.path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	out, errText, code := runCLI("log")
	if code != 0 || !strings.Contains(out, "git_status") || !strings.Contains(out, "read_file") {
		t.Fatalf("entries around an oversized one must still be shown: exit %d\n%s", code, out)
	}
	if !strings.Contains(errText, "1 entry") {
		t.Errorf("the skipped entry must be mentioned: %q", errText)
	}
}

// Two repochat processes that reach the size limit at the same moment must
// not rotate twice: the second rotation would replace the history that was
// just set aside with the nearly empty new file.
func TestConcurrentRotationKeepsTheHistory(t *testing.T) {
	setup(t, "")
	for round := 0; round < 40; round++ {
		a, err := openAudit()
		if err != nil {
			t.Fatal(err)
		}
		os.Remove(a.path + ".1")
		if err := os.WriteFile(a.path, []byte(strings.Repeat("x", auditMaxBytes+1)+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		for w := 0; w < 4; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				b, _ := openAudit()
				b.write(activity{Kind: "call", Tool: "read_file", OK: true, Time: time.Now()})
			}()
		}
		wg.Wait()
		fi, err := os.Stat(filepath.Join(filepath.Dir(a.path), auditFile+".1"))
		if err != nil || fi.Size() < auditMaxBytes {
			t.Fatalf("round %d: the rotated history was lost (%v, size %d)", round, err, sizeOf(fi))
		}
	}
}

func sizeOf(fi os.FileInfo) int64 {
	if fi == nil {
		return -1
	}
	return fi.Size()
}
