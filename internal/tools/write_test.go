package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/nalyk/repochat/internal/mcp"
	"github.com/nalyk/repochat/internal/repo"
)

// writeFixture is an owner repository with a few committed files, a
// repo.Sessions on a state directory outside it, and the write-mode tools.
type writeFixture struct {
	dir   string
	state string // the Sessions' state directory
	owner *repo.Repo
	s     *repo.Sessions
	tools map[string]mcp.Tool
	list  []mcp.Tool
}

func newWriteFixture(t *testing.T, opts repo.Options, files map[string]string) *writeFixture {
	t.Helper()
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	t.Setenv("HOME", t.TempDir())
	git(t, dir, "init", "-q")
	git(t, dir, "symbolic-ref", "HEAD", "refs/heads/main")
	git(t, dir, "config", "user.name", "Ada")
	git(t, dir, "config", "user.email", "a@example.com")
	if files == nil {
		files = map[string]string{"notes.txt": "hello\nworld\n", "main.go": "package main\n\nfunc main() {}\n"}
	}
	for rel, content := range files {
		write(t, dir, rel, content)
	}
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "start")
	owner, err := repo.Open(bg, dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { owner.Close() })
	state, _ := filepath.EvalSymlinks(t.TempDir())
	s, err := repo.NewSessions(owner, repo.SessionOptions{StateDir: filepath.Join(state, "sessions"), Repo: opts})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	f := &writeFixture{dir: dir, state: filepath.Join(state, "sessions"), owner: owner, s: s, tools: map[string]mcp.Tool{}, list: NewWrite(owner, s)}
	for _, tool := range f.list {
		f.tools[tool.Name] = tool
	}
	return f
}

// call runs a tool; see the package-level call.
// head is the open session's checkpoint, for finish_session's expected_checkpoint.
func (f *writeFixture) head(t *testing.T) string {
	t.Helper()
	info, err := f.s.Current(bg)
	if err != nil {
		t.Fatal(err)
	}
	return info.Head
}

func (f *writeFixture) call(t *testing.T, name string, raw map[string]any) (string, map[string]any, error) {
	t.Helper()
	return call(t, f.tools, name, raw)
}

// must runs a tool that has to succeed.
func (f *writeFixture) must(t *testing.T, name string, raw map[string]any) (string, map[string]any) {
	t.Helper()
	text, m, err := f.call(t, name, raw)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return text, m
}

var anchoredLineRe = regexp.MustCompile(`(?m)^[> ]*\s*(\d+):([a-z2-7]{3})\| (.*)$`)

// anchorFor returns the anchor ("12:abc") of the first line in text whose
// content is exactly line.
func anchorFor(t *testing.T, text, line string) string {
	t.Helper()
	for _, m := range anchoredLineRe.FindAllStringSubmatch(text, -1) {
		if m[3] == line {
			return m[1] + ":" + m[2]
		}
	}
	t.Fatalf("no anchored line %q in:\n%s", line, text)
	return ""
}

func changesJSON(t *testing.T, ops ...map[string]string) []any {
	t.Helper()
	out := make([]any, len(ops))
	for i, op := range ops {
		m := map[string]any{}
		for k, v := range op {
			m[k] = v
		}
		out[i] = m
	}
	return out
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

var writeAnnotations = map[string][2]bool{ // name → {write, destructive}
	"start_session":      {true, false},
	"session_status":     {false, false},
	"propose_changes":    {false, false},
	"apply_changes":      {true, false},
	"list_checkpoints":   {false, false},
	"undo_to_checkpoint": {true, false},
	"update_session":     {true, false},
	"finish_session":     {true, true},
	"abandon_session":    {true, true},
}

func TestWriteCatalogueAddsNineToolsWithHonestAnnotations(t *testing.T) {
	f := newWriteFixture(t, repo.Options{}, nil)
	if len(f.list) != 22 {
		t.Fatalf("expected 22 tools, got %d", len(f.list))
	}
	// The 13 read tools come first and are advertised exactly as in read mode.
	want, err := os.ReadFile(readCatalogueGolden)
	if err != nil {
		t.Fatal(err)
	}
	if got := catalogueJSON(t, f.list[:13]); string(got) != string(want) {
		t.Fatalf("the read tools changed in write mode:\n%s", got)
	}
	pending := maps.Clone(writeAnnotations)
	for _, tool := range f.list[13:] {
		ann, ok := pending[tool.Name]
		if !ok {
			t.Errorf("unexpected tool %s", tool.Name)
			continue
		}
		delete(pending, tool.Name)
		if tool.Write != ann[0] || tool.Destructive != ann[1] {
			t.Errorf("%s: write=%v destructive=%v, want %v %v", tool.Name, tool.Write, tool.Destructive, ann[0], ann[1])
		}
		if !toolNameRe.MatchString(tool.Name) || tool.Title == "" || len(tool.Description) < 40 {
			t.Errorf("%s: name/title/description are not well formed", tool.Name)
		}
		raw, _ := json.Marshal(mcp.ToolJSON(tool))
		if strings.Contains(string(raw), "null") {
			t.Errorf("%s: definition contains null: %s", tool.Name, raw)
		}
		for pname, p := range tool.Schema.JSON()["properties"].(map[string]any) {
			if desc, _ := p.(map[string]any)["description"].(string); len(desc) < 10 {
				t.Errorf("%s.%s: parameters need a description", tool.Name, pname)
			}
		}
	}
	if len(pending) > 0 {
		t.Errorf("missing tools: %v", pending)
	}

	// changes: an array of objects with exactly the members of repo.Change.
	var changes mcp.Param
	for _, p := range f.tools["apply_changes"].Schema {
		if p.Name == "changes" {
			changes = p
		}
	}
	if changes.Kind != mcp.Object || changes.Raw["type"] != "array" || changes.Raw["maxItems"] != 500 {
		t.Fatalf("changes is not an array of at most 500 items: %#v", changes.Raw)
	}
	items := changes.Raw["items"].(map[string]any)
	if items["type"] != "object" || items["additionalProperties"] != false {
		t.Fatalf("changes items must be closed objects: %#v", items)
	}
	props := items["properties"].(map[string]any)
	ct := reflect.TypeOf(repo.Change{})
	if len(props) != ct.NumField() {
		t.Errorf("changes items have %d members, repo.Change has %d", len(props), ct.NumField())
	}
	for i := 0; i < ct.NumField(); i++ {
		name := ct.Field(i).Tag.Get("json")
		p, ok := props[name].(map[string]any)
		if !ok {
			t.Errorf("member %s is not described", name)
			continue
		}
		if p["type"] != "string" || len(p["description"].(string)) < 10 {
			t.Errorf("member %s: %#v", name, p)
		}
	}
	if op := props["op"].(map[string]any); !reflect.DeepEqual(op["enum"], []string{"replace", "insert", "delete", "create", "remove", "move", "format"}) {
		t.Errorf("op enum: %#v", op["enum"])
	}
	if text := props["text"].(map[string]any)["description"].(string); !strings.Contains(text, "anchor") {
		t.Errorf("text must say it carries no anchors: %s", text)
	}
	if from := props["from"].(map[string]any)["description"].(string); !strings.Contains(from, "12:abc") {
		t.Errorf("from must show what an anchor looks like: %s", from)
	}
}

func TestWriteInstructionsAreExact(t *testing.T) {
	const want = "Write mode is on. Files are edited in a session workspace on a side branch, never in the user's own checkout. Lines you read carry anchors like `12:abc|`; cite them in apply_changes instead of quoting old text. If an anchor is stale the call fails and returns the current lines: use those anchors and retry. One apply_changes call may change many files and is applied completely or not at all. Lines that hold a masked value cannot be changed; tell the user to edit them by hand. Nothing reaches the user's branch until finish_session; call it only when the user asks to merge. A replace or delete of three or more lines also needs the file's current file_tag (from read_file or from the last apply_changes result). Pass a new operation_id with every call that changes something, and expected_checkpoint (the latest checkpoint you saw) with finish_session."
	if WriteInstructions != want {
		t.Fatalf("WriteInstructions differs from the brief:\n%s", WriteInstructions)
	}
}

func TestEditRoundTripThroughTheTools(t *testing.T) {
	f := newWriteFixture(t, repo.Options{}, nil)
	_, sm := f.must(t, "start_session", map[string]any{})
	base, _ := sm["base"].(string)
	if len(base) < 40 || sm["branch"] == nil {
		t.Fatalf("start_session meta: %v", sm)
	}

	text, m := f.must(t, "read_file", map[string]any{"path": "notes.txt"})
	tag, _ := m["file_tag"].(string)
	if !regexp.MustCompile(`^[a-z2-7]{8}$`).MatchString(tag) {
		t.Fatalf("read_file in write mode must carry file_tag: %v", m)
	}
	world := anchorFor(t, text, "world")

	applied, am := f.must(t, "apply_changes", map[string]any{
		"changes": changesJSON(t, map[string]string{"op": "replace", "path": "notes.txt", "from": world, "text": "there"}),
		"message": "Say hello there",
	})
	if am["checkpoint"] == nil || !strings.Contains(applied, "there") || strings.Contains(applied, "-world") {
		t.Fatalf("apply_changes: %v\n%s", am, applied)
	}
	anchorFor(t, applied, "there") // fresh anchors around the change

	text, _ = f.must(t, "read_file", map[string]any{"path": "notes.txt"})
	if !strings.Contains(text, "| there") || strings.Contains(text, "| world") {
		t.Fatalf("read_file after apply:\n%s", text)
	}
	list, lm := f.must(t, "list_checkpoints", map[string]any{})
	if lm["total"] != float64(2) || !strings.Contains(list, "Say hello there") || !strings.Contains(list, "start") {
		t.Fatalf("list_checkpoints: %v\n%s", lm, list)
	}
	diff, _ := f.must(t, "git_diff", map[string]any{"mode": "range", "base": base})
	if !strings.Contains(diff, "-world") || !strings.Contains(diff, "+there") {
		t.Fatalf("git_diff against the start:\n%s", diff)
	}
	status, _ := f.must(t, "session_status", map[string]any{})
	if !strings.Contains(status, "notes.txt") {
		t.Fatalf("session_status:\n%s", status)
	}

	fin, fm := f.must(t, "finish_session", map[string]any{"expected_checkpoint": f.head(t)})
	if fm["commit"] == nil || !strings.Contains(fin, "main") {
		t.Fatalf("finish_session: %v\n%s", fm, fin)
	}
	data, err := os.ReadFile(filepath.Join(f.dir, "notes.txt"))
	if err != nil || string(data) != "hello\nthere\n" {
		t.Fatalf("the owner's file after finish: %q %v", data, err)
	}
	// The session is closed: the read tools serve the owner's checkout again,
	// without anchors.
	text, m = f.must(t, "read_file", map[string]any{"path": "notes.txt"})
	if anchoredLineRe.MatchString(text) || m["file_tag"] != nil {
		t.Fatalf("anchors after the session ended:\n%s", text)
	}
}

func TestStaleAnchorErrorCarriesFreshAnchorsThatWork(t *testing.T) {
	f := newWriteFixture(t, repo.Options{}, nil)
	f.must(t, "start_session", map[string]any{})
	text, _ := f.must(t, "read_file", map[string]any{"path": "notes.txt"})
	world := anchorFor(t, text, "world")
	// Lines move under the anchor the model holds.
	f.must(t, "apply_changes", map[string]any{
		"changes": changesJSON(t, map[string]string{"op": "insert", "path": "notes.txt", "at": "start", "text": "first"}),
	})
	_, _, err := f.call(t, "apply_changes", map[string]any{
		"changes": changesJSON(t, map[string]string{"op": "replace", "path": "notes.txt", "from": world, "text": "there"}),
	})
	if err == nil {
		t.Fatal("a stale anchor must refuse the change set")
	}
	msg := err.Error()
	if !strings.Contains(msg, "nothing was changed") || !strings.Contains(msg, "fresh anchors") {
		t.Fatalf("stale anchor error: %s", msg)
	}
	fresh := anchorFor(t, msg, "world")
	if fresh == world {
		t.Fatalf("the error must carry the current anchor, got the stale one %s", fresh)
	}
	f.must(t, "apply_changes", map[string]any{
		"changes": changesJSON(t, map[string]string{"op": "replace", "path": "notes.txt", "from": fresh, "text": "there"}),
	})
	text, _ = f.must(t, "read_file", map[string]any{"path": "notes.txt"})
	if !strings.Contains(text, "| first") || !strings.Contains(text, "| there") {
		t.Fatalf("after the retry:\n%s", text)
	}
}

func TestWriteToolsRefuseWithoutASession(t *testing.T) {
	f := newWriteFixture(t, repo.Options{}, nil)
	ops := changesJSON(t, map[string]string{"op": "create", "path": "x.txt", "text": "x"})
	for name, args := range map[string]map[string]any{
		"propose_changes":    {"changes": ops},
		"apply_changes":      {"changes": ops},
		"list_checkpoints":   {},
		"undo_to_checkpoint": {"checkpoint": "abcdef1"},
		"update_session":     {},
		"finish_session":     {"expected_checkpoint": "abcdef1"},
		"abandon_session":    {},
	} {
		_, _, err := f.call(t, name, args)
		if err == nil || !strings.Contains(err.Error(), "start_session") {
			t.Errorf("%s without a session: %v", name, err)
		}
	}
	// session_status answers, and says how to open one.
	text, m := f.must(t, "session_status", map[string]any{})
	if m["open"] != false || !strings.Contains(text, "start_session") {
		t.Errorf("session_status without a session: %v\n%s", m, text)
	}
	// The read tools serve the owner's checkout, without anchors.
	text, m = f.must(t, "read_file", map[string]any{"path": "notes.txt"})
	if anchoredLineRe.MatchString(text) || m["file_tag"] != nil {
		t.Errorf("anchors without a session:\n%s", text)
	}
	if _, err := os.Stat(filepath.Join(f.dir, "x.txt")); err == nil {
		t.Error("a write without a session changed the owner's checkout")
	}
}

func TestReadToolsFollowTheSession(t *testing.T) {
	f := newWriteFixture(t, repo.Options{}, nil)
	write(t, f.dir, "mine.txt", "the owner's own\n")
	visible := func(path string) bool {
		_, _, err := f.call(t, "read_file", map[string]any{"path": path})
		return err == nil
	}
	if !visible("mine.txt") {
		t.Fatal("before start, the owner's untracked file must be visible")
	}
	f.must(t, "start_session", map[string]any{})
	if visible("mine.txt") {
		t.Fatal("during the session, the owner's untracked file must not be visible")
	}
	f.must(t, "apply_changes", map[string]any{
		"changes": changesJSON(t, map[string]string{"op": "create", "path": "new.txt", "text": "made by the model"}),
	})
	if !visible("new.txt") {
		t.Fatal("the model's new file must be visible during the session")
	}
	found, _ := f.must(t, "search_code", map[string]any{"query": "made by"})
	if !regexp.MustCompile(`(?m)^> 1:[a-z2-7]{3}\| made by the model$`).MatchString(found) {
		t.Fatalf("search hits in write mode carry anchors:\n%s", found)
	}
	if _, err := os.Stat(filepath.Join(f.dir, "new.txt")); err == nil {
		t.Fatal("the owner's checkout must not change before finish")
	}
	text, _ := f.must(t, "abandon_session", map[string]any{})
	if !strings.Contains(text, "kept") {
		t.Errorf("abandon_session must say the branch is kept:\n%s", text)
	}
	if !visible("mine.txt") || visible("new.txt") {
		t.Fatal("after abandon, the owner's checkout must be served again")
	}
}

func TestApplyNeedsExactlyOneOfChangesAndPlan(t *testing.T) {
	f := newWriteFixture(t, repo.Options{}, nil)
	f.must(t, "start_session", map[string]any{})
	ops := changesJSON(t, map[string]string{"op": "create", "path": "x.txt", "text": "x"})
	plan, pm := f.must(t, "propose_changes", map[string]any{"changes": ops})
	id, _ := pm["plan"].(string)
	if !regexp.MustCompile(`^[0-9a-f]{16}$`).MatchString(id) || !strings.Contains(plan, "+x") {
		t.Fatalf("propose_changes: %v\n%s", pm, plan)
	}
	for _, args := range []map[string]any{{}, {"changes": ops, "plan": id}, {"message": "m"}} {
		_, _, err := f.call(t, "apply_changes", args)
		if err == nil || !strings.Contains(err.Error(), "exactly one") {
			t.Errorf("apply_changes %v: %v", args, err)
		}
	}
	if _, _, err := f.call(t, "apply_changes", map[string]any{"plan": "not-a-plan"}); err == nil {
		t.Error("a plan id that is not 16 hex characters must be refused")
	}
	info, err := f.s.Current(bg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(info.Workspace, "x.txt")); err == nil {
		t.Fatal("a refused apply wrote a file")
	}
	f.must(t, "apply_changes", map[string]any{"plan": id})
	text, _ := f.must(t, "read_file", map[string]any{"path": "x.txt"})
	if !strings.Contains(text, "| x") {
		t.Fatalf("the plan was not applied:\n%s", text)
	}
}

func TestProposeDiffIsMaskedAndBudgeted(t *testing.T) {
	secret := "ghp_" + strings.Repeat("A1b2C3d4", 5)
	var big strings.Builder
	big.WriteString("token = \"" + secret + "\"\n")
	for i := 0; i < 3000; i++ {
		fmt.Fprintf(&big, "line %d of a file that is long enough to exceed the budget\n", i)
	}
	opts := repo.Options{MaxOutputBytes: repo.MinOutput}
	f := newWriteFixture(t, opts, map[string]string{"big.txt": big.String(), "notes.txt": "hello\n"})
	f.must(t, "start_session", map[string]any{})
	_, m := f.must(t, "read_file", map[string]any{"path": "big.txt", "max_lines": 5})
	tag, _ := m["file_tag"].(string)
	res, err := f.tools["propose_changes"].Handler(bg, mustArgs(t, f.tools["propose_changes"], map[string]any{
		"changes": changesJSON(t, map[string]string{"op": "remove", "path": "big.txt", "file_tag": tag}),
	}))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Text) > repo.MinOutput {
		t.Fatalf("propose_changes result is %d bytes, budget %d", len(res.Text), repo.MinOutput)
	}
	if strings.Contains(res.Text, secret) || !strings.Contains(res.Text, "[REDACTED:github-token]") {
		t.Fatalf("the diff must be masked:\n%s", res.Text[:min(len(res.Text), 500)])
	}
	if !strings.Contains(res.Text, "diff cut at the size budget") || !strings.Contains(res.Text, `"diff_truncated":true`) {
		t.Fatalf("the cut must be stated:\n%s", res.Text[len(res.Text)-400:])
	}
	if !strings.Contains(res.Text, `"plan":"`) {
		t.Fatal("a cut diff still gives the plan id")
	}
}

func TestApplyFreshIsBudgeted(t *testing.T) {
	f := newWriteFixture(t, repo.Options{MaxOutputBytes: repo.MinOutput}, nil)
	f.must(t, "start_session", map[string]any{})
	var big strings.Builder
	for i := 0; i < 2000; i++ {
		fmt.Fprintf(&big, "generated line %d with some text on it\n", i)
	}
	res, err := f.tools["apply_changes"].Handler(bg, mustArgs(t, f.tools["apply_changes"], map[string]any{
		"changes": changesJSON(t, map[string]string{"op": "create", "path": "gen.txt", "text": big.String()}),
	}))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Text) > repo.MinOutput || !strings.Contains(res.Text, "read the files again") {
		t.Fatalf("apply_changes result %d bytes:\n%s", len(res.Text), res.Text[max(0, len(res.Text)-400):])
	}
}

func TestChangeErrorIsCutToTheBudget(t *testing.T) {
	f := newWriteFixture(t, repo.Options{MaxOutputBytes: repo.MinOutput}, nil)
	c := (&catalog{owner: f.owner}).at(f.owner)
	huge := &repo.ChangeError{Path: "a.txt", Reason: "stale", Fresh: strings.Repeat("12:abc| some line\n", 5000)}
	_, err := c.writeFail(huge, false)
	if len(err.Error()) > repo.MinOutput || !strings.Contains(err.Error(), "read the files again") {
		t.Fatalf("cut error: %d bytes, ends %q", len(err.Error()), err.Error()[max(0, len(err.Error())-200):])
	}
	// A finish error that says the checkout may be partly updated is never cut.
	long := errors.New("finish failed, and the repository's checkout may be partly updated: " + strings.Repeat("recover with these steps. ", 2000))
	_, err = c.writeFail(long, true)
	if err.Error() != long.Error() {
		t.Fatal("the recovery text must reach the user whole")
	}
}

func TestListCheckpointsKeepsTheStart(t *testing.T) {
	f := newWriteFixture(t, repo.Options{}, nil)
	f.must(t, "start_session", map[string]any{})
	for i := 0; i < 3; i++ {
		f.must(t, "apply_changes", map[string]any{
			"changes": changesJSON(t, map[string]string{"op": "insert", "path": "notes.txt", "at": "end", "text": fmt.Sprintf("more %d", i)}),
			"message": fmt.Sprintf("Step %d", i),
		})
	}
	text, m := f.must(t, "list_checkpoints", map[string]any{"limit": 2})
	if m["total"] != float64(4) || m["shown"] != float64(2) || m["complete"] != false {
		t.Fatalf("meta: %v", m)
	}
	if !strings.Contains(text, "Step 2") || strings.Contains(text, "Step 1") || !strings.Contains(text, "start") || !strings.Contains(text, "2 older") {
		t.Fatalf("list_checkpoints limit=2:\n%s", text)
	}
	// Undo back to the start through the listed id.
	id := regexp.MustCompile(`(?m)^([0-9a-f]{12})\s+start\b`).FindStringSubmatch(text)
	if id == nil {
		t.Fatalf("no start id in:\n%s", text)
	}
	undo, _ := f.must(t, "undo_to_checkpoint", map[string]any{"checkpoint": id[1]})
	if !strings.Contains(undo, "read the file again") {
		t.Fatalf("undo result:\n%s", undo)
	}
	got, _ := f.must(t, "read_file", map[string]any{"path": "notes.txt"})
	if strings.Contains(got, "more 0") {
		t.Fatalf("undo did not restore:\n%s", got)
	}
}

func TestFinishShowsWhatRunsLater(t *testing.T) {
	f := newWriteFixture(t, repo.Options{}, nil)
	f.must(t, "start_session", map[string]any{})
	f.must(t, "apply_changes", map[string]any{
		"changes": changesJSON(t,
			map[string]string{"op": "create", "path": "build.sh", "text": "echo hi"},
			map[string]string{"op": "create", "path": "a.txt", "text": "a"}),
	})
	st, sm := f.must(t, "session_status", map[string]any{})
	if !strings.Contains(st, "build.sh") || sm["runs_later"] == nil {
		t.Fatalf("session_status must show what runs later: %v\n%s", sm, st)
	}
	text, m := f.must(t, "finish_session", map[string]any{"squash": true, "message": "Add the build script", "expected_checkpoint": f.head(t)})
	later := strings.Index(text, "build.sh")
	files := strings.Index(text, "a.txt")
	if later < 0 || files < 0 || later > files || !strings.Contains(text, "unsigned") || m["squashed"] != true {
		t.Fatalf("finish_session must put what runs later and the notes first: %v\n%s", m, text)
	}
	if got := gitOut(t, f.dir, "log", "-1", "--format=%s"); got != "Add the build script" {
		t.Fatalf("squashed commit subject: %q", got)
	}
}

func mustArgs(t *testing.T, tool mcp.Tool, raw map[string]any) mcp.Args {
	t.Helper()
	a, err := tool.Schema.Validate(raw)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// ---------------------------------------------------------------------------
// Review round 1
// ---------------------------------------------------------------------------

func TestReadModeInstructionsAreUnchanged(t *testing.T) {
	want, err := os.ReadFile("testdata/instructions.txt")
	if err != nil {
		t.Fatal(err)
	}
	if Instructions != string(want) {
		t.Fatalf("Instructions changed:\n%s", Instructions)
	}
}

func TestWriteModeInstructionsDoNotContradictThemselves(t *testing.T) {
	if !strings.Contains(WriteModeInstructions, WriteInstructions) {
		t.Error("WriteModeInstructions must contain WriteInstructions verbatim")
	}
	for _, bad := range []string{"Read-only access", "Nothing can be modified"} {
		if strings.Contains(WriteModeInstructions, bad) {
			t.Errorf("WriteModeInstructions says %q", bad)
		}
	}
	shared := 0
	for _, l := range strings.Split(Instructions, "\n") {
		if strings.HasPrefix(l, "- ") && !strings.Contains(l, "Nothing can be modified") {
			shared++
			if !strings.Contains(WriteModeInstructions, l+"\n") {
				t.Errorf("WriteModeInstructions lacks the detail line %q", l)
			}
		}
	}
	if shared < 4 || !strings.Contains(WriteModeInstructions, "Start with repo_overview.") {
		t.Errorf("shared parts missing (%d detail lines)", shared)
	}
}

// probeCatalog is the write-mode catalogue of f with Current replaced.
func probeCatalog(f *writeFixture, current func(context.Context) (*repo.SessionInfo, error)) *catalog {
	c := &catalog{src: served{f.s}, owner: f.owner, s: f.s, current: current}
	return c.at(f.owner)
}

func TestFinishNeverCallsAnOpenSessionClosed(t *testing.T) {
	f := newWriteFixture(t, repo.Options{}, nil)
	f.must(t, "start_session", map[string]any{})
	f.must(t, "apply_changes", map[string]any{
		"changes": changesJSON(t, map[string]string{"op": "create", "path": "a.txt", "text": "a"}),
	})
	// The probe after the merge fails with something other than "no session".
	calls := 0
	c := probeCatalog(f, func(ctx context.Context) (*repo.SessionInfo, error) {
		calls++
		if calls == 1 {
			return f.s.Current(ctx)
		}
		return nil, errors.New("git: signal: killed")
	})
	res, err := c.finishSession(bg, mcp.Args{"expected_checkpoint": f.head(t)})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Text, "session is closed") || strings.Contains(res.Text, `"session_open":false`) ||
		!strings.Contains(res.Text, "could not be determined") || !strings.Contains(res.Text, "session_status") {
		t.Fatalf("finish with an unknown session state:\n%s", res.Text)
	}

	// The probe itself is not bound to the call's deadline.
	g := newWriteFixture(t, repo.Options{}, nil)
	g.must(t, "start_session", map[string]any{})
	expired, cancel := context.WithCancel(bg)
	cancel()
	if open, known := probeCatalog(g, nil).sessionOpen(expired); !open || !known {
		t.Fatalf("an expired call context must not make an open session look closed: open=%v known=%v", open, known)
	}
}

func TestFinishTextWhenTheBranchCannotBeRead(t *testing.T) {
	land := strings.Repeat("ab", 20)
	fin := &repo.Finished{Commit: land, Files: []string{"a.txt"}, Notes: []string{
		"WARNING: git reported the merge of " + land + " as done, but where main is now could not be read (boom); the session, its branch and its workspace are kept. Check the checkout with git status and git log.",
	}}
	text := finishText("main", fin, true, true)
	if strings.Contains(text, "main is at") || !strings.Contains(text, "the merged commit is "+land[:12]) {
		t.Fatalf("finish text:\n%s", text)
	}
	// Otherwise the branch position is known and stated.
	fin.Notes = []string{"WARNING: git reported the merge as done, but main is at " + land + ", not at the merged commit x; kept."}
	if text := finishText("main", fin, true, true); !strings.Contains(text, "main is at "+land[:12]) {
		t.Fatalf("finish text:\n%s", text)
	}
}

func TestAbandonWithoutABranchNameOmitsTheMergeCommand(t *testing.T) {
	f := newWriteFixture(t, repo.Options{}, nil)
	f.must(t, "start_session", map[string]any{})
	c := probeCatalog(f, func(context.Context) (*repo.SessionInfo, error) { return nil, errors.New("boom") })
	res, err := c.abandon(bg, mcp.Args{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Text, "git merge") || strings.Contains(res.Text, "branch  ") {
		t.Fatalf("abandon without a branch name:\n%s", res.Text)
	}
}

func TestNewSessionFlagIsTranslatedForTheModel(t *testing.T) {
	f := newWriteFixture(t, repo.Options{}, nil)
	f.must(t, "start_session", map[string]any{})
	info, err := f.s.Current(bg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(info.Workspace); err != nil {
		t.Fatal(err)
	}
	// repochat restarted: the recorded session is resumed and found broken.
	f.s.Close()
	s2, err := repo.NewSessions(f.owner, repo.SessionOptions{StateDir: filepath.Dir(filepath.Dir(filepath.Dir(info.Workspace)))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s2.Close() })
	f.tools = map[string]mcp.Tool{}
	for _, tool := range NewWrite(f.owner, s2) {
		f.tools[tool.Name] = tool
	}
	const hint = "From here: call start_session with fresh=true."
	for _, name := range []string{"list_checkpoints", "read_file"} {
		args := map[string]any{}
		if name == "read_file" {
			args["path"] = "notes.txt"
		}
		_, _, err := f.call(t, name, args)
		if err == nil || !strings.Contains(err.Error(), "--new-session") || !strings.HasSuffix(err.Error(), hint) {
			t.Errorf("%s with the workspace gone: %v", name, err)
		}
	}
	f.must(t, "start_session", map[string]any{"fresh": true})
	f.must(t, "read_file", map[string]any{"path": "notes.txt"})
}

// C5 through the tools: a range read before its middle line changed is
// refused without the file's current tag and with the old one, and goes
// through with the tag the apply_changes result gave.
func TestLongRangeNeedsTheFileTagThroughTheTools(t *testing.T) {
	f := newWriteFixture(t, repo.Options{}, map[string]string{"notes.txt": "first\nmiddle\nlast\n"})
	f.must(t, "start_session", map[string]any{})
	text, m := f.must(t, "read_file", map[string]any{"path": "notes.txt"})
	oldTag, _ := m["file_tag"].(string)
	a1, a2, a3 := anchorFor(t, text, "first"), anchorFor(t, text, "middle"), anchorFor(t, text, "last")
	applied, _ := f.must(t, "apply_changes", map[string]any{
		"changes": changesJSON(t, map[string]string{"op": "replace", "path": "notes.txt", "from": a2, "text": "new middle"}),
	})
	tagLine := regexp.MustCompile(`(?m)^FILE notes\.txt\n\[file_tag ([a-z2-7]{8})\]$`).FindStringSubmatch(applied)
	if tagLine == nil || tagLine[1] == oldTag {
		t.Fatalf("apply_changes gave no new file_tag for notes.txt (old %s):\n%s", oldTag, applied)
	}
	stale := map[string]string{"op": "replace", "path": "notes.txt", "from": a1, "to": a3, "text": "replacement"}
	_, _, err := f.call(t, "apply_changes", map[string]any{"changes": changesJSON(t, stale)})
	if err == nil || !strings.Contains(err.Error(), "three or more lines") || !strings.Contains(err.Error(), "file_tag") || !strings.Contains(err.Error(), "nothing was changed") {
		t.Fatalf("a long range without file_tag: %v", err)
	}
	stale["file_tag"] = oldTag
	_, _, err = f.call(t, "apply_changes", map[string]any{"changes": changesJSON(t, stale)})
	if err == nil || !strings.Contains(err.Error(), "does not match the file as it is now") {
		t.Fatalf("a long range with the stale file_tag: %v", err)
	}
	if got := gitOut(t, f.dir, "worktree", "list"); !strings.Contains(got, "repochat/") {
		t.Fatalf("no session worktree: %s", got)
	}
	text, m = f.must(t, "read_file", map[string]any{"path": "notes.txt"})
	if !strings.Contains(text, "| new middle") || m["file_tag"] != tagLine[1] {
		t.Fatalf("after the refusals the file is not as the first apply left it (tag %v, want %s):\n%s", m["file_tag"], tagLine[1], text)
	}
	stale["file_tag"] = tagLine[1]
	f.must(t, "apply_changes", map[string]any{"changes": changesJSON(t, stale)})
	text, _ = f.must(t, "read_file", map[string]any{"path": "notes.txt"})
	if !strings.Contains(text, "| replacement") || strings.Contains(text, "middle") {
		t.Fatalf("after the range with the fresh tag:\n%s", text)
	}

	// The schema and the instructions say so.
	var changes mcp.Param
	for _, p := range f.tools["apply_changes"].Schema {
		if p.Name == "changes" {
			changes = p
		}
	}
	props := changes.Raw["items"].(map[string]any)["properties"].(map[string]any)
	for _, name := range []string{"op", "to", "file_tag"} {
		if desc := props[name].(map[string]any)["description"].(string); !strings.Contains(desc, "three or more lines") {
			t.Errorf("%s does not state the file_tag rule: %s", name, desc)
		}
	}
	if !strings.Contains(WriteInstructions, "A replace or delete of three or more lines also needs the file's current file_tag (from read_file or from the last apply_changes result).") {
		t.Error("WriteInstructions does not contain the file_tag rule")
	}
}

// validated runs a tool's schema and handler, for arguments the schema may
// itself refuse: it returns the first refusal and the text of a success.
func (f *writeFixture) validated(t *testing.T, name string, raw map[string]any) (string, error) {
	t.Helper()
	tool := f.tools[name]
	args, err := tool.Schema.Validate(raw)
	if err != nil {
		return "", err
	}
	res, err := tool.Handler(bg, args)
	return res.Text, err
}

func TestApplyReplayThroughTheTool(t *testing.T) {
	f := newWriteFixture(t, repo.Options{}, nil)
	f.must(t, "start_session", map[string]any{})
	create := map[string]any{
		"changes":      changesJSON(t, map[string]string{"op": "create", "path": "x.txt", "text": "x\n"}),
		"operation_id": "op-create-x-1",
	}
	first, fm := f.must(t, "apply_changes", create)
	if strings.HasPrefix(first, "Operation") || fm["replayed"] != nil {
		t.Fatalf("the first call is not a replay:\n%s\n%v", first, fm)
	}
	text, m := f.must(t, "apply_changes", create)
	if !strings.HasPrefix(text, "Operation op-create-x-1 was already done: ") || !strings.Contains(text, "Nothing was done again.") || !strings.Contains(text, "Read the files again for fresh anchors.") {
		t.Fatalf("replay text:\n%s", text)
	}
	if m["replayed"] != true || m["checkpoint"] != fm["checkpoint"] {
		t.Fatalf("replay meta %v, first %v", m, fm)
	}
	list, lm := f.must(t, "list_checkpoints", map[string]any{})
	if lm["total"] != float64(2) {
		t.Fatalf("one change plus the start expected:\n%s", list)
	}
	// Other arguments under the same id are refused.
	_, err := f.validated(t, "apply_changes", map[string]any{
		"changes":      changesJSON(t, map[string]string{"op": "create", "path": "y.txt", "text": "y\n"}),
		"operation_id": "op-create-x-1",
	})
	if err == nil || !strings.Contains(err.Error(), "already used for a different call") {
		t.Fatalf("same id, other arguments: %v", err)
	}
}

func TestOperationIDAndExpectedCheckpointAreValidated(t *testing.T) {
	f := newWriteFixture(t, repo.Options{}, nil)
	_, sm := f.must(t, "start_session", map[string]any{})
	head, _ := sm["head"].(string)
	create := changesJSON(t, map[string]string{"op": "create", "path": "x.txt", "text": "x\n"})
	for _, c := range []struct{ what, key, val string }{
		{"short id", "operation_id", "short"},
		{"path id", "operation_id", "../../etc"},
		{"long id", "operation_id", strings.Repeat("a", 65)},
		{"non-hex checkpoint", "expected_checkpoint", "xyz1234"},
		{"short checkpoint", "expected_checkpoint", "abc12"},
		{"stale checkpoint", "expected_checkpoint", strings.Repeat("0", 7)},
	} {
		args := map[string]any{"changes": create, c.key: c.val}
		if _, err := f.validated(t, "apply_changes", args); err == nil {
			t.Errorf("%s: apply_changes was not refused", c.what)
		}
		if _, lm := f.must(t, "list_checkpoints", map[string]any{}); lm["total"] != float64(1) {
			t.Errorf("%s: the session gained a checkpoint: %v", c.what, lm)
		}
	}
	for _, name := range []string{"undo_to_checkpoint", "update_session"} {
		args := map[string]any{"expected_checkpoint": strings.Repeat("0", 7)}
		if name == "undo_to_checkpoint" {
			args["checkpoint"] = head
		}
		_, err := f.validated(t, name, args)
		if err == nil || !strings.Contains(err.Error(), "the session moved") || strings.Contains(err.Error(), "change set") {
			t.Errorf("%s with a stale checkpoint: %v", name, err)
		}
	}
	_, err := f.validated(t, "finish_session", map[string]any{"expected_checkpoint": strings.Repeat("0", 7)})
	if err == nil || !strings.Contains(err.Error(), "the session moved") || strings.Contains(err.Error(), "change set") {
		t.Errorf("finish_session with a stale checkpoint: %v", err)
	}
	// A current prefix is accepted.
	f.must(t, "apply_changes", map[string]any{"changes": create, "expected_checkpoint": head[:7]})
}

func TestFinishWithoutExpectedCheckpointExplainsWhy(t *testing.T) {
	f := newWriteFixture(t, repo.Options{}, nil)
	f.must(t, "start_session", map[string]any{})
	_, _, err := f.call(t, "finish_session", map[string]any{})
	if err == nil {
		t.Fatal("finish_session without expected_checkpoint was not refused")
	}
	for _, want := range []string{"expected_checkpoint", "session_status", "Refresh tools"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal lacks %q: %v", want, err)
		}
	}
	for _, p := range f.tools["finish_session"].Schema {
		if p.Name == "expected_checkpoint" && (p.Required || !strings.HasPrefix(p.Desc, "Required.")) {
			t.Errorf("finish_session's expected_checkpoint: Required=%v desc %q", p.Required, p.Desc)
		}
	}
}

func TestFinishHandoffNamesTheRevision(t *testing.T) {
	f := newWriteFixture(t, repo.Options{}, nil)
	_, sm := f.must(t, "start_session", map[string]any{})
	_, am := f.must(t, "apply_changes", map[string]any{"changes": changesJSON(t, map[string]string{"op": "create", "path": "x.txt", "text": "x\n"})})
	cp, _ := am["checkpoint"].(string)
	text, m := f.must(t, "finish_session", map[string]any{"expected_checkpoint": cp, "operation_id": "fin-handoff-1"})
	for _, want := range []string{"Merged revision: the session at ", "the session started at " + sm["base"].(string)[:12], "Full diff: `git diff ", "No checks were run: repochat cannot run commands yet."} {
		if !strings.Contains(text, want) {
			t.Errorf("finish text lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "repochat session diff") {
		t.Errorf("the finish text offers `repochat session diff`, which needs an open session:\n%s", text)
	}
	if m["head"] == nil || m["base"] != sm["base"] {
		t.Errorf("finish meta: %v", m)
	}
	again, am2 := f.must(t, "finish_session", map[string]any{"expected_checkpoint": cp, "operation_id": "fin-handoff-1"})
	if !strings.HasPrefix(again, "Operation fin-handoff-1 was already done: ") || am2["replayed"] != true {
		t.Errorf("finish replay:\n%s\n%v", again, am2)
	}
	if strings.Contains(again, "repochat session diff") {
		t.Errorf("the replayed finish offers `repochat session diff`:\n%s", again)
	}
}

var fullDiffRe = regexp.MustCompile("Full diff: `git diff ([0-9a-f]{40,64}) ([0-9a-f]{40,64})`")

// The command the finish hands over shows the merged change when run in the
// user's checkout after the finish, for a fast-forward and for a squash, and
// in the replayed answer too.
func TestFinishHandoffDiffCommandShowsTheChange(t *testing.T) {
	for _, squash := range []bool{false, true} {
		t.Run(fmt.Sprintf("squash=%v", squash), func(t *testing.T) {
			f := newWriteFixture(t, repo.Options{}, nil)
			f.must(t, "start_session", map[string]any{})
			_, am := f.must(t, "apply_changes", map[string]any{"changes": changesJSON(t, map[string]string{"op": "create", "path": "handoff.txt", "text": "handed over\n"})})
			cp, _ := am["checkpoint"].(string)
			args := map[string]any{"expected_checkpoint": cp, "operation_id": "fin-diffcmd-1", "squash": squash}
			if squash {
				args["message"] = "squashed"
			}
			text, _ := f.must(t, "finish_session", args)
			again, _ := f.must(t, "finish_session", args)
			for _, out := range []string{text, again} {
				m := fullDiffRe.FindStringSubmatch(out)
				if m == nil {
					t.Fatalf("no full diff command in:\n%s", out)
				}
				diff := gitOut(t, f.dir, "diff", m[1], m[2])
				if !strings.Contains(diff, "handoff.txt") || !strings.Contains(diff, "+handed over") {
					t.Errorf("`git diff %s %s` shows:\n%s", m[1], m[2], diff)
				}
			}
		})
	}
}

func TestStartAndAbandonReplayThroughTheTools(t *testing.T) {
	f := newWriteFixture(t, repo.Options{}, nil)
	_, m1 := f.must(t, "start_session", map[string]any{"operation_id": "start-op-001"})
	text, m2 := f.must(t, "start_session", map[string]any{"operation_id": "start-op-001"})
	if !strings.HasPrefix(text, "Operation start-op-001 was already done: ") || m2["replayed"] != true || m2["session"] != m1["session"] {
		t.Fatalf("start replay:\n%s\n%v", text, m2)
	}
	f.must(t, "abandon_session", map[string]any{"operation_id": "abandon-op-01"})
	text, m3 := f.must(t, "abandon_session", map[string]any{"operation_id": "abandon-op-01"})
	if !strings.HasPrefix(text, "Operation abandon-op-01 was already done: ") || m3["replayed"] != true {
		t.Fatalf("abandon replay:\n%s\n%v", text, m3)
	}
}

// A replayed start says whether that call started the session or resumed it.
func TestStartReplaySaysStartedOrResumed(t *testing.T) {
	f := newWriteFixture(t, repo.Options{}, nil)
	f.must(t, "start_session", map[string]any{"operation_id": "start-new-001"})
	text, _ := f.must(t, "start_session", map[string]any{"operation_id": "start-new-001"})
	if !strings.Contains(text, "was started by that call") {
		t.Errorf("replay of a start that opened the session:\n%s", text)
	}
	f.must(t, "start_session", map[string]any{"operation_id": "start-res-001"})
	text, _ = f.must(t, "start_session", map[string]any{"operation_id": "start-res-001"})
	if !strings.Contains(text, "was resumed by that call") || strings.Contains(text, "started by that call") {
		t.Errorf("replay of a start that resumed the session:\n%s", text)
	}
}

// A start whose receipt cannot be written says so in its result, not only on
// the terminal.
func TestStartReceiptFailureIsInTheResult(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permissions do not bind root")
	}
	f := newWriteFixture(t, repo.Options{}, nil)
	f.must(t, "start_session", map[string]any{})
	dir := filepath.Join(f.state, f.owner.ID)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	text, _ := f.must(t, "start_session", map[string]any{"operation_id": "start-nowrite"})
	if !strings.Contains(text, "receipt") || !strings.Contains(text, "not recognised") {
		t.Errorf("the start result does not tell of the unwritten receipt:\n%s", text)
	}
}

func TestArgsDigestIgnoresOperationIDAndKeyOrder(t *testing.T) {
	a := mcp.Args{"message": "m", "squash": true, "operation_id": "aaaaaaaa", "changes": json.RawMessage(`{"a":1,"b":[1,2]}`)}
	b := mcp.Args{"changes": json.RawMessage(`{ "b": [1,2], "a": 1 }`), "squash": true, "operation_id": "zzzzzzzz", "message": "m"}
	d := argsDigest("finish_session", a)
	if !repo.DigestRe.MatchString(d) {
		t.Fatalf("digest %q is not 16 lower-case hex characters", d)
	}
	if got := argsDigest("finish_session", b); got != d {
		t.Errorf("key order or operation_id changed the digest: %s %s", d, got)
	}
	if argsDigest("apply_changes", a) == d {
		t.Error("another tool gave the same digest")
	}
	c := mcp.Args{"message": "m2", "squash": true, "changes": json.RawMessage(`{"a":1,"b":[1,2]}`)}
	if argsDigest("finish_session", c) == d {
		t.Error("another argument gave the same digest")
	}
	if argsDigest("finish_session", mcp.Args{"message": "m", "squash": true, "changes": json.RawMessage(`{"a":1,"b":[1,2]}`), "expected_checkpoint": "abcdef1"}) == d {
		t.Error("an added argument gave the same digest")
	}
}
