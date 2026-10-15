// Package todostore implements a project-root todo file store backed by
// GitHub-style checkbox markdown files.
package todostore

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Todo is a single todo item.
type Todo struct {
	Text string
	Done bool
}

// Store is a todo store rooted at a project root directory. The active todo
// list lives in <root>/todos.md; done items are archived to
// <root>/todos_history.md.
type Store struct {
	Root  string
	Mu    sync.Mutex
	Todos []Todo
}

const todosHeader = "# Todos\n\n<!-- managed by `late`; only the orchestrator edits this file -->\n\n"

const historyHeader = "# Todos History\n"

const datePrefix = "## "

// ParseTodosMD parses GitHub-style checkbox lines (`- [ ] text`,
// `- [x] text`) from data. All other lines are skipped.
func ParseTodosMD(data []byte) []Todo {
	var todos []Todo
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimRight(line, "\r")
		trimmed := strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(trimmed, "- [ ] "); ok {
			todos = append(todos, Todo{Text: strings.TrimSpace(rest), Done: false})
			continue
		}
		if rest, ok := strings.CutPrefix(trimmed, "- [x] "); ok {
			todos = append(todos, Todo{Text: strings.TrimSpace(rest), Done: true})
			continue
		}
		if rest, ok := strings.CutPrefix(trimmed, "- [X] "); ok {
			todos = append(todos, Todo{Text: strings.TrimSpace(rest), Done: true})
		}
	}
	return todos
}

// RenderTodosMD renders todos as a checkbox markdown list.
func RenderTodosMD(todos []Todo) string {
	var b strings.Builder
	for _, t := range todos {
		if t.Done {
			b.WriteString("- [x] ")
		} else {
			b.WriteString("- [ ] ")
		}
		b.WriteString(t.Text)
		b.WriteString("\n")
	}
	return b.String()
}

// Open reads <root>/todos.md into a new store. A missing file or directory
// yields an empty store, not an error.
func Open(root string) (*Store, error) {
	s := &Store{Root: root}
	data, err := os.ReadFile(s.todosPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return s, nil
		}
		return nil, err
	}
	s.Todos = ParseTodosMD(data)
	return s, nil
}

// todosPath returns the path of the active todos file.
func (s *Store) todosPath() string { return filepath.Join(s.Root, "todos.md") }

// historyPath returns the path of the history file.
func (s *Store) historyPath() string { return filepath.Join(s.Root, "todos_history.md") }

// LoadAndArchive moves done items from todos.md into the history file under
// today's date heading (local time), deduplicated against that date's
// existing entries, then rewrites todos.md with only the remaining items.
// If there are no done items, nothing is changed.
func (s *Store) LoadAndArchive() error {
	s.Mu.Lock()
	defer s.Mu.Unlock()

	data, err := os.ReadFile(s.todosPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			s.Todos = nil
			return nil
		}
		return err
	}
	all := ParseTodosMD(data)
	s.Todos = all

	var done []Todo
	var remaining []Todo
	for _, t := range all {
		if t.Done {
			done = append(done, t)
		} else {
			remaining = append(remaining, t)
		}
	}
	if len(done) == 0 {
		return nil
	}
	if err := appendHistory(s.historyPath(), time.Now(), done); err != nil {
		return err
	}
	s.Todos = remaining
	return s.writeActiveLocked()
}

// WriteActive atomically rewrites todos.md (tmp file + fsync + rename) with
// s.Todos.
func (s *Store) WriteActive() error {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	return s.writeActiveLocked()
}

func (s *Store) writeActiveLocked() error {
	return atomicWrite(s.todosPath(), []byte(todosHeader+RenderTodosMD(s.Todos)))
}

// AppendDone appends a `[x] text` history item for the given text,
// deduplicated against today's entries, creating the history file and date
// section when needed.
func (s *Store) AppendDone(text string) error {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	return appendHistory(s.historyPath(), time.Now(), []Todo{{Text: text, Done: true}})
}

// atomicWrite writes data to path atomically: write to a tmp sibling, fsync,
// then rename over path.
func atomicWrite(path string, data []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// appendHistory appends done todos to the history file as `[x] <text>`
// entries under the date heading for now (local time), creating the file
// (`# Todos History`) and the date section when needed. Entries whose text
// exactly matches an existing entry under that date heading are skipped.
func appendHistory(path string, now time.Time, done []Todo) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		data = []byte(historyHeader)
	}

	today := now.Format("2006-01-02")
	dateHeading := datePrefix + today

	// Split the file into (heading, body lines) sections, keeping the
	// preamble (# Todos History) separate.
	var preamble []string
	type section struct {
		heading string
		lines   []string
	}
	var sections []section
	var cur *section
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimRight(line, "\r")
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, datePrefix) {
			sections = append(sections, section{heading: trimmed})
			cur = &sections[len(sections)-1]
			continue
		}
		if cur == nil {
			if trimmed == "" && len(preamble) == 0 {
				continue
			}
			preamble = append(preamble, trimmed)
			continue
		}
		if trimmed == "" && len(cur.lines) == 0 {
			continue
		}
		cur.lines = append(cur.lines, trimmed)
	}

	// Find (or create) today's section and dedupe against its entries.
	var todayIdx = -1
	for i := range sections {
		if sections[i].heading == dateHeading {
			todayIdx = i
			break
		}
	}
	if todayIdx < 0 {
		sections = append(sections, section{heading: dateHeading})
		todayIdx = len(sections) - 1
	}
	existing := make(map[string]bool, len(sections[todayIdx].lines))
	for _, l := range sections[todayIdx].lines {
		if strings.HasPrefix(l, "- [x] ") {
			existing[strings.TrimPrefix(l, "- [x] ")] = true
		} else {
			existing[l] = true
		}
	}
	for _, t := range done {
		if existing[t.Text] {
			continue
		}
		existing[t.Text] = true
		sections[todayIdx].lines = append(sections[todayIdx].lines, "- [x] "+t.Text)
	}

	// Drop empty sections and rename the unaddressable slice element.
	filled := sections[:0]
	for _, sec := range sections {
		if len(sec.lines) > 0 {
			filled = append(filled, sec)
		}
	}
	sections = filled

	// Re-render.
	var b strings.Builder
	b.WriteString(strings.TrimSpace(strings.Join(preamble, "\n")))
	b.WriteString("\n\n")
	for i, sec := range sections {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(sec.heading)
		b.WriteString("\n")
		for _, l := range sec.lines {
			b.WriteString(l)
			b.WriteString("\n")
		}
	}
	out := b.String()
	if !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	if out == strings.TrimSpace(strings.Join(preamble, "\n"))+"\n\n" {
		out = historyHeader
	}
	return os.WriteFile(path, []byte(out), 0o644)
}
