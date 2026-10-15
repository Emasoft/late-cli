package todostore_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"late/internal/todostore"
)

func mustRead(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func todosPath(root string) string   { return filepath.Join(root, "todos.md") }
func historyPath(root string) string { return filepath.Join(root, "todos_history.md") }
func todayHeading() string           { return "## " + time.Now().Format("2006-01-02") }
func mustOpen(t *testing.T, root string) *todostore.Store {
	t.Helper()
	s, err := todostore.Open(root)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s
}

func TestParseRenderRoundTrip(t *testing.T) {
	in := "# Todos\n\n- [ ] buy milk\n- [ ] feed  cat  \n- [x] done task\n- [X] upper done\nnot a checkbox\nrandom text line\n"
	got := todostore.ParseTodosMD([]byte(in))
	want := []todostore.Todo{
		{Text: "buy milk"},
		{Text: "feed  cat"},
		{Text: "done task", Done: true},
		{Text: "upper done", Done: true},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d todos, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("todo[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}

	out := todostore.RenderTodosMD(got)
	wantRender := "- [ ] buy milk\n- [ ] feed  cat\n- [x] done task\n- [x] upper done\n"
	if out != wantRender {
		t.Errorf("render = %q, want %q", out, wantRender)
	}

	// Full round-trip is stable.
	again := todostore.ParseTodosMD([]byte(out))
	if len(again) != len(want) {
		t.Fatalf("round-trip produced %d todos, want %d", len(again), len(want))
	}
	for i := range want {
		if again[i] != want[i] {
			t.Errorf("round-trip todo[%d] = %+v, want %+v", i, again[i], want[i])
		}
	}
}

func TestOpenMissingDir(t *testing.T) {
	root := filepath.Join(t.TempDir(), "does", "not", "exist")
	s := mustOpen(t, root)
	if len(s.Todos) != 0 {
		t.Errorf("expected empty store, got %+v", s.Todos)
	}
}

func TestOpenHandWrittenTodosMD(t *testing.T) {
	root := t.TempDir()
	t.Cleanup(func() {})
	mustWrite(t, todosPath(root), "# Todos\n\n- [ ] open one\n- [x] done one\n- [X] done two\n")
	s := mustOpen(t, root)
	if len(s.Todos) != 3 {
		t.Fatalf("got %d todos, want 3: %+v", len(s.Todos), s.Todos)
	}
	if s.Todos[0].Text != "open one" || s.Todos[0].Done {
		t.Errorf("todo[0] = %+v", s.Todos[0])
	}
	if !s.Todos[1].Done || s.Todos[1].Text != "done one" {
		t.Errorf("todo[1] = %+v", s.Todos[1])
	}
	if !s.Todos[2].Done || s.Todos[2].Text != "done two" {
		t.Errorf("todo[2] = %+v", s.Todos[2])
	}
}

func TestLoadAndArchive(t *testing.T) {
	root := t.TempDir()
	t.Cleanup(func() {})
	mustWrite(t, todosPath(root), "- [ ] keep me\n- [x] finished a\n- [X] finished b\n")

	s := mustOpen(t, root)
	if err := s.LoadAndArchive(); err != nil {
		t.Fatalf("LoadAndArchive: %v", err)
	}

	got := todostore.ParseTodosMD([]byte(mustRead(t, todosPath(root))))
	if len(got) != 1 || got[0] != (todostore.Todo{Text: "keep me"}) {
		t.Errorf("todos.md after archive = %+v, want [keep me]", got)
	}
	// Store state should reflect remaining open items.
	if len(s.Todos) != 1 || s.Todos[0].Text != "keep me" {
		t.Errorf("s.Todos = %+v", s.Todos)
	}

	hist := mustRead(t, historyPath(root))
	if !strings.Contains(hist, "# Todos History\n") {
		t.Errorf("history missing header:\n%s", hist)
	}
	section := todayHeading()
	if !strings.Contains(hist, section) {
		t.Errorf("history missing date heading %q:\n%s", section, hist)
	}
	for _, want := range []string{"- [x] finished a", "- [x] finished b"} {
		if !strings.Contains(hist, want+"\n") {
			t.Errorf("history missing %q:\n%s", want, hist)
		}
	}

	// Second call is a no-op: no duplicate entries.
	if err := s.LoadAndArchive(); err != nil {
		t.Fatalf("second LoadAndArchive: %v", err)
	}
	hist2 := mustRead(t, historyPath(root))
	if hist2 != hist {
		t.Errorf("history changed on second archive:\n--- first ---\n%s\n--- second ---\n%s", hist, hist2)
	}
}

func TestLoadAndArchiveDedupesAgainstExistingEntries(t *testing.T) {
	root := t.TempDir()
	t.Cleanup(func() {})
	today := todayHeading()
	mustWrite(t, historyPath(root), "# Todos History\n\n"+today+"\n\n- [x] already archived\n")
	mustWrite(t, todosPath(root), "- [x] already archived\n- [x] new one\n")

	s := mustOpen(t, root)
	if err := s.LoadAndArchive(); err != nil {
		t.Fatalf("LoadAndArchive: %v", err)
	}

	hist := mustRead(t, historyPath(root))
	if strings.Count(hist, "- [x] already archived") != 1 {
		t.Errorf("dedupe failed, history:\n%s", hist)
	}
	if !strings.Contains(hist, "- [x] new one\n") {
		t.Errorf("new entry missing:\n%s", hist)
	}
}

func TestAppendDoneCreatesHistoryFile(t *testing.T) {
	root := t.TempDir()
	t.Cleanup(func() {})
	s := mustOpen(t, root)

	if err := s.AppendDone("task one"); err != nil {
		t.Fatalf("AppendDone: %v", err)
	}

	hist := mustRead(t, historyPath(root))
	want := "# Todos History\n\n" + todayHeading() + "\n- [x] task one\n"
	if !strings.Contains(hist, want) {
		t.Errorf("history =\n%s\nwant section:\n%s", hist, want)
	}

	// Appending the same text again dedupes: still exactly one entry.
	if err := s.AppendDone("task one"); err != nil {
		t.Fatalf("second AppendDone: %v", err)
	}
	hist2 := mustRead(t, historyPath(root))
	if got := strings.Count(hist2, "- [x] task one"); got != 1 {
		t.Errorf("expected exactly one 'task one' entry, got %d:\n%s", got, hist2)
	}

	// Appending new text adds it.
	if err := s.AppendDone("task two"); err != nil {
		t.Fatalf("third AppendDone: %v", err)
	}
	hist3 := mustRead(t, historyPath(root))
	if !strings.Contains(hist3, "- [x] task two\n") {
		t.Errorf("task two missing:\n%s", hist3)
	}
}

func TestWriteActiveAtomicity(t *testing.T) {
	root := t.TempDir()
	t.Cleanup(func() {})
	s := mustOpen(t, root)
	s.Todos = []todostore.Todo{{Text: "an item"}}

	if err := s.WriteActive(); err != nil {
		t.Fatalf("WriteActive: %v", err)
	}

	if _, err := os.Stat(todosPath(root) + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("leftover .tmp file: %v", err)
	}
	data := mustRead(t, todosPath(root))
	wantHeader := "# Todos\n\n<!-- managed by `late`; only the orchestrator edits this file -->\n\n"
	if !strings.HasPrefix(data, wantHeader) {
		t.Errorf("todos.md missing managed header, content:\n%s", data)
	}
	if !strings.Contains(data, "- [ ] an item\n") {
		t.Errorf("item missing:\n%s", data)
	}
}

func TestHandEditsHonoredBetweenSessions(t *testing.T) {
	root := t.TempDir()
	t.Cleanup(func() {})

	// Session 1: write an active file.
	s1 := mustOpen(t, root)
	s1.Todos = []todostore.Todo{{Text: "original"}}
	if err := s1.WriteActive(); err != nil {
		t.Fatalf("WriteActive: %v", err)
	}

	// Human edits todos.md between sessions.
	mustWrite(t, todosPath(root), "# Todos\n\n- [ ] original\n- [ ] hand added\n")

	// Session 2: re-open, hand edit picked up.
	s2 := mustOpen(t, root)
	if len(s2.Todos) != 2 || s2.Todos[1].Text != "hand added" {
		t.Fatalf("s2.Todos = %+v", s2.Todos)
	}
	if err := s2.LoadAndArchive(); err != nil {
		t.Fatalf("LoadAndArchive: %v", err)
	}
	got := todostore.ParseTodosMD([]byte(mustRead(t, todosPath(root))))
	if len(got) != 2 {
		t.Fatalf("todos after archive = %+v, want 2 items", got)
	}
	if got[0].Text != "original" || got[1].Text != "hand added" {
		t.Errorf("todos after archive = %+v", got)
	}
}
