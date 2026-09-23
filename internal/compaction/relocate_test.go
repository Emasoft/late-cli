package compaction

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// relocationOutput builds a three-paragraph tool output whose segments stay
// separate (each well over defaultMinSegChars, well under
// DefaultMaxSegChars). Every paragraph carries its marker after the first 60
// characters, so a pointer's preview can never leak it.
func relocationOutput() (output, keeper1, filler, keeper2 string) {
	keeper1 = strings.Repeat("k", 100) + "KEEPER-ONE-MARK" + strings.Repeat("1", 40)
	filler = strings.Repeat("f", 100) + "FILLER-SECRET-MARK" + strings.Repeat("2", 40)
	keeper2 = strings.Repeat("j", 100) + "KEEPER-TWO-MARK" + strings.Repeat("3", 40)
	return keeper1 + "\n\n" + filler + "\n\n" + keeper2, keeper1, filler, keeper2
}

// boundaryScores scores seg-1 exactly at the threshold, seg-2 below it, and
// seg-3 above it.
func boundaryScores(threshold float64) map[string]float64 {
	return map[string]float64{
		"seg-1": threshold, // exactly at: stays (strictly-below comparison)
		"seg-2": 0.1,       // below: elided
		"seg-3": 0.9,       // above: stays
	}
}

// TestPipeline_EnableRelocationThresholdBoundary pins the relocation
// contract: the segment scoring exactly at the threshold stays, the one
// below it is elided into the store, the high scorer stays, and the compact
// text is the kept segments followed by the exact pointer format.
func TestPipeline_EnableRelocationThresholdBoundary(t *testing.T) {
	const threshold = 0.35
	d := newDecisionsServer(t, fixedScoresHandler(boundaryScores(threshold)))
	store := NewStore()

	p := NewPipeline(ResolvedBackend{Backend: Backend{Name: "test", URL: d.srv.URL, Model: "jev-latest"}, APIKey: "k"}, "k", nil, PipelineOptions{})
	p.EnableRelocation(store, threshold)

	output, keeper1, filler, keeper2 := relocationOutput()
	got, err := p.CompactToolOutput(context.Background(), "Bash", output)
	if err != nil {
		t.Fatalf("CompactToolOutput() error = %v", err)
	}

	// The stored original is the full segment span (a segment absorbs its
	// trailing blank-line separator, so spans tile the input).
	segs := SegmentSegments(output, 0)
	if len(segs) != 3 {
		t.Fatalf("SegmentSegments() = %d segments, want 3", len(segs))
	}
	fillerSpan := segs[1].Text

	if len(got.Elided) != 1 {
		t.Fatalf("Elided = %d segments, want exactly the below-threshold one", len(got.Elided))
	}
	e := got.Elided[0]
	if e.ID != "elide-1" {
		t.Errorf("elided id = %q, want elide-1 (per-store counter)", e.ID)
	}
	if e.Text != fillerSpan {
		t.Errorf("elided text = %q, want the filler segment span %q", e.Text, fillerSpan)
	}
	if !strings.Contains(e.Text, filler) {
		t.Errorf("elided text %q lost the filler paragraph", e.Text)
	}
	if e.Lines != strings.Count(fillerSpan, "\n")+1 {
		t.Errorf("elided lines = %d, want %d", e.Lines, strings.Count(fillerSpan, "\n")+1)
	}
	if len([]rune(e.Preview)) > previewChars || strings.ContainsAny(e.Preview, "\n\r\t") {
		t.Errorf("elided preview = %q, want ≤%d flat chars", e.Preview, previewChars)
	}

	// The store round-trips the original.
	if text, ok := store.Get("elide-1"); !ok || text != fillerSpan {
		t.Errorf("store.Get(elide-1) = (%q, %v), want the filler original", text, ok)
	}

	// Compact text: kept segments first, then the pointer line.
	wantKept := keeper1 + "\n\n" + keeper2
	if !strings.HasPrefix(got.CompactText, wantKept+"\n\n") {
		t.Errorf("CompactText must start with the kept segments, got:\n%s", got.CompactText)
	}
	if !strings.Contains(got.CompactText, keeper1) || !strings.Contains(got.CompactText, keeper2) {
		t.Errorf("CompactText lost a high-scoring segment:\n%s", got.CompactText)
	}
	if strings.Contains(got.CompactText, "FILLER-SECRET-MARK") {
		t.Errorf("CompactText leaked the elided segment's original:\n%s", got.CompactText)
	}
	pointer := regexp.MustCompilePOSIX(`^.*\[\[elided id=elide-1 lines=[0-9]+ tokens=[0-9]+ ".{1,60}"\]\]$`)
	if !pointer.MatchString(got.CompactText) {
		t.Errorf("CompactText missing the elided pointer in the pinned format:\n%s", got.CompactText)
	}
	lastLine := got.CompactText[strings.LastIndexByte(got.CompactText, '\n')+1:]
	if !strings.HasPrefix(lastLine, "[[elided id=elide-1 lines=") || !strings.HasSuffix(lastLine, "]]") {
		t.Errorf("pointer must be the last line, got %q", lastLine)
	}
}

// TestPipeline_CompactOutputAtAndAboveThresholdKeepsOriginalBytes: nothing
// below the threshold means the result passes through byte-for-byte (never
// reconstructed, which would drop leading blank bytes).
func TestPipeline_CompactOutputNothingElidedKeepsOriginalBytes(t *testing.T) {
	// Every segment scores 0.9, so nothing is elided.
	high := newDecisionsServer(t, func(_ int, req capturedRequest) (int, string) {
		scores := make(map[string]float64, len(req.Req.Questions))
		for ref := range req.Req.Questions {
			scores[ref] = 0.9
		}
		return http.StatusOK, answersBody(scores)
	})
	store := NewStore()
	p := NewPipeline(ResolvedBackend{Backend: Backend{Name: "test", URL: high.srv.URL, Model: "jev-latest"}, APIKey: "k"}, "k", nil, PipelineOptions{})
	p.EnableRelocation(store, 0.35)

	output := strings.Repeat("keep me\n\n", 40)
	got, err := p.CompactToolOutput(context.Background(), "Bash", output)
	if err != nil {
		t.Fatalf("CompactToolOutput() error = %v", err)
	}
	if got.CompactText != output {
		t.Error("CompactText must equal the original byte-for-byte when nothing is elided")
	}
	if len(got.Elided) != 0 {
		t.Errorf("Elided = %v, want empty", got.Elided)
	}
	if _, ok := store.Get("elide-1"); ok {
		t.Error("store must stay empty when nothing is elided")
	}
}

// TestPipeline_CompactOutputAllElided: every segment below the threshold
// leaves only pointer lines behind.
func TestPipeline_CompactOutputAllElided(t *testing.T) {
	d := newDecisionsServer(t, fixedScoresHandler(map[string]float64{"seg-1": 0.1, "seg-2": 0.2}))
	store := NewStore()
	p := NewPipeline(ResolvedBackend{Backend: Backend{Name: "test", URL: d.srv.URL, Model: "jev-latest"}, APIKey: "k"}, "k", nil, PipelineOptions{})
	p.EnableRelocation(store, 0.35)

	output := strings.Repeat("x", 120) + "\n\n" + strings.Repeat("y", 120)
	got, err := p.CompactToolOutput(context.Background(), "Bash", output)
	if err != nil {
		t.Fatalf("CompactToolOutput() error = %v", err)
	}
	if len(got.Elided) != 2 {
		t.Fatalf("Elided = %d segments, want 2", len(got.Elided))
	}
	if strings.Contains(got.CompactText, strings.Repeat("x", 120)) {
		t.Error("CompactText kept an elided segment's text")
	}
	if !strings.Contains(got.CompactText, "id=elide-1") || !strings.Contains(got.CompactText, "id=elide-2") {
		t.Errorf("CompactText missing both pointers:\n%s", got.CompactText)
	}
	if _, ok := store.Get("elide-2"); !ok {
		t.Error("store must hold the second elided original")
	}
}

// TestPipeline_CompactOutputFailOpenKeepsOriginal: any scoring error means
// the original result comes back unchanged (never break a tool call over
// compaction). A 400 is non-retryable so this fails fast; the 503 case
// shrinks the retry curve.
func TestPipeline_CompactOutputFailOpenKeepsOriginal(t *testing.T) {
	output, _, _, _ := relocationOutput()
	for _, tc := range []struct {
		name    string
		status  int
		shrink  bool
		wantErr bool
	}{
		{name: "permanent 400", status: http.StatusBadRequest},
		{name: "transient 503", status: http.StatusServiceUnavailable, shrink: true, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newDecisionsServer(t, func(_ int, _ capturedRequest) (int, string) {
				return tc.status, `{"error": {"message": "down"}}`
			})
			store := NewStore()
			p := NewPipeline(ResolvedBackend{Backend: Backend{Name: "test", URL: d.srv.URL, Model: "jev-latest"}, APIKey: "k"}, "k", nil, PipelineOptions{})
			if tc.shrink {
				p.client.baseBackoff, p.client.maxBackoff = time.Millisecond, time.Millisecond
			}
			p.EnableRelocation(store, 0.35)

			got, err := p.CompactToolOutput(context.Background(), "Bash", output)
			if tc.wantErr && err == nil {
				t.Fatal("CompactToolOutput() error = nil, want the outage recorded")
			}
			if got.CompactText != output {
				t.Errorf("fail-open must return the original text unchanged, got:\n%s", got.CompactText)
			}
			if len(got.Elided) != 0 {
				t.Errorf("Elided = %v, want empty on fail-open", got.Elided)
			}
			if _, ok := store.Get("elide-1"); ok {
				t.Error("store must stay empty on fail-open")
			}
		})
	}
}

// TestPipeline_CompactWithoutRelocationIsShadow: an un-armed pipeline scores
// and logs but never mutates the result or the store — the shadow contract.
func TestPipeline_CompactWithoutRelocationIsShadow(t *testing.T) {
	d := newDecisionsServer(t, fixedScoresHandler(map[string]float64{"seg-1": 0.01, "seg-2": 0.02}))
	store := NewStore()
	p := NewPipeline(ResolvedBackend{Backend: Backend{Name: "test", URL: d.srv.URL, Model: "jev-latest"}, APIKey: "k"}, "k", nil, PipelineOptions{})

	output := strings.Repeat("a", 120) + "\n\n" + strings.Repeat("b", 120)
	got, err := p.CompactToolOutput(context.Background(), "Bash", output)
	if err != nil {
		t.Fatalf("CompactToolOutput() error = %v", err)
	}
	if got.CompactText != output || len(got.Elided) != 0 {
		t.Errorf("shadow-only pipeline must not mutate the result, got %+v", got)
	}
	// Arming with a nil store disarms relocation again.
	p.EnableRelocation(nil, 0.35)
	got, err = p.CompactToolOutput(context.Background(), "Bash", output)
	if err != nil || got.CompactText != output {
		t.Errorf("disarmed pipeline must not mutate the result, got %+v, err %v", got, err)
	}
	if _, ok := store.Get("elide-1"); ok {
		t.Error("store must stay empty without relocation")
	}
}

// TestPipeline_RelocationShadowLogDecisions: the shadow log records "elide"
// for below-threshold segments only when relocation is armed; shadow mode
// keeps recording "keep" for everything.
func TestPipeline_RelocationShadowLogDecisions(t *testing.T) {
	newArmed := func(armed bool) (*Pipeline, *ShadowLog) {
		d := newDecisionsServer(t, fixedScoresHandler(boundaryScores(0.35)))
		shadow, err := NewShadowLogAt(filepath.Join(t.TempDir(), "shadow.jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		p := NewPipeline(ResolvedBackend{Backend: Backend{Name: "test", URL: d.srv.URL, Model: "jev-latest"}, APIKey: "k"}, "k", shadow, PipelineOptions{})
		if armed {
			p.EnableRelocation(NewStore(), 0.35)
		}
		return p, shadow
	}

	output, _, _, _ := relocationOutput()

	t.Run("armed records elide", func(t *testing.T) {
		p, shadow := newArmed(true)
		if _, err := p.CompactToolOutput(context.Background(), "Bash", output); err != nil {
			t.Fatalf("CompactToolOutput() error = %v", err)
		}
		report, err := shadow.Replay(0.35)
		if err != nil {
			t.Fatalf("Replay() error = %v", err)
		}
		if report.Entries != 3 || report.ElidedEntries != 1 {
			t.Fatalf("Replay() = %+v, want 3 entries with 1 elided", report)
		}
	})

	t.Run("shadow records keep", func(t *testing.T) {
		p, shadow := newArmed(false)
		if _, err := p.CompactToolOutput(context.Background(), "Bash", output); err != nil {
			t.Fatalf("CompactToolOutput() error = %v", err)
		}
		report, err := shadow.Replay(0.35)
		if err != nil {
			t.Fatalf("Replay() error = %v", err)
		}
		if report.Entries != 3 || report.ElidedEntries != 1 {
			t.Fatalf("Replay() = %+v, want 3 entries with 1 below threshold", report)
		}
	})
}

// TestPipeline_ElideIDsIncrementAcrossCalls: the per-store counter never
// reuses an id, so the store can hold originals from many tool results.
func TestPipeline_ElideIDsIncrementAcrossCalls(t *testing.T) {
	d := newDecisionsServer(t, echoHandler) // seg-N scores 0.01*N: all below 0.35
	store := NewStore()
	p := NewPipeline(ResolvedBackend{Backend: Backend{Name: "test", URL: d.srv.URL, Model: "jev-latest"}, APIKey: "k"}, "k", nil, PipelineOptions{})
	p.EnableRelocation(store, 0.35)

	ctx := context.Background()
	first, err := p.CompactToolOutput(ctx, "Bash", strings.Repeat("a", 120)+"\n\n"+strings.Repeat("b", 120))
	if err != nil {
		t.Fatalf("first CompactToolOutput() error = %v", err)
	}
	second, err := p.CompactToolOutput(ctx, "Bash", strings.Repeat("c", 120)+"\n\n"+strings.Repeat("d", 120))
	if err != nil {
		t.Fatalf("second CompactToolOutput() error = %v", err)
	}

	if first.Elided[0].ID != "elide-1" || first.Elided[1].ID != "elide-2" {
		t.Errorf("first call ids = %s, %s; want elide-1, elide-2", first.Elided[0].ID, first.Elided[1].ID)
	}
	if second.Elided[0].ID != "elide-3" || second.Elided[1].ID != "elide-4" {
		t.Errorf("second call ids = %s, %s; want elide-3, elide-4 (counter continues)", second.Elided[0].ID, second.Elided[1].ID)
	}
	for i, e := range append(append([]ElidedSegment{}, first.Elided...), second.Elided...) {
		if _, ok := store.Get(e.ID); !ok {
			t.Errorf("store missing %s (segment %d)", e.ID, i)
		}
	}
}

// TestPipeline_CompactToolResultTrailer pins the executor-facing string
// form: compact text plus the expand-tool trailer, original on fail-open.
func TestPipeline_CompactToolResultTrailer(t *testing.T) {
	output, keeper1, _, keeper2 := relocationOutput()
	d := newDecisionsServer(t, fixedScoresHandler(boundaryScores(0.35)))
	store := NewStore()
	p := NewPipeline(ResolvedBackend{Backend: Backend{Name: "test", URL: d.srv.URL, Model: "jev-latest"}, APIKey: "k"}, "k", nil, PipelineOptions{})
	p.EnableRelocation(store, 0.35)

	got := p.CompactToolResult(context.Background(), "Bash", output)
	for _, want := range []string{
		keeper1, keeper2,
		"1 segments elided — use the expand tool with the elided ids to retrieve originals (elide-1).",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("CompactToolResult() missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "FILLER-SECRET-MARK") {
		t.Errorf("CompactToolResult() leaked the elided original:\n%s", got)
	}

	// Nothing elided → byte-identical, no trailer.
	high := newDecisionsServer(t, fixedScoresHandler(map[string]float64{"seg-1": 0.9, "seg-2": 0.9, "seg-3": 0.9}))
	p2 := NewPipeline(ResolvedBackend{Backend: Backend{Name: "test", URL: high.srv.URL, Model: "jev-latest"}, APIKey: "k"}, "k", nil, PipelineOptions{})
	p2.EnableRelocation(store, 0.35)
	if got := p2.CompactToolResult(context.Background(), "Bash", output); got != output {
		t.Errorf("CompactToolResult() with nothing elided must return the original, got:\n%s", got)
	}
}

// TestPipeline_EnableRelocationClampsThreshold: out-of-range thresholds fall
// back to DefaultRelocationThreshold instead of eliding everything or
// nothing by accident.
func TestPipeline_EnableRelocationClampsThreshold(t *testing.T) {
	d := newDecisionsServer(t, fixedScoresHandler(map[string]float64{"seg-1": 0.3}))
	output := strings.Repeat("a", 120)

	for _, bad := range []float64{0, -1, 1.5} {
		store := NewStore()
		p := NewPipeline(ResolvedBackend{Backend: Backend{Name: "test", URL: d.srv.URL, Model: "jev-latest"}, APIKey: "k"}, "k", nil, PipelineOptions{})
		p.EnableRelocation(store, bad)
		got, err := p.CompactToolOutput(context.Background(), "Bash", output)
		if err != nil {
			t.Fatalf("threshold %v: CompactToolOutput() error = %v", bad, err)
		}
		// Every out-of-range threshold clamps to DefaultRelocationThreshold,
		// under which the 0.3-scoring segment is elided (a raw threshold of
		// 0 would keep everything; 1.5 would be nonsense).
		if len(got.Elided) != 1 {
			t.Errorf("threshold %v: Elided = %d, want the default-threshold elision", bad, len(got.Elided))
		}
	}
}

// TestStore_GetRoundTripAndMisses covers the store's read side.
func TestStore_GetRoundTripAndMisses(t *testing.T) {
	var nilStore *Store
	if _, ok := nilStore.Get("elide-1"); ok {
		t.Error("nil store must report a miss")
	}

	s := NewStore()
	if _, ok := s.Get("elide-1"); ok {
		t.Error("empty store must report a miss")
	}
	s.Put("elide-1", "original text")
	if text, ok := s.Get("elide-1"); !ok || text != "original text" {
		t.Errorf("Get(elide-1) = (%q, %v), want the stored original", text, ok)
	}
	s.Put("elide-1", "replaced")
	if text, _ := s.Get("elide-1"); text != "replaced" {
		t.Errorf("Get(elide-1) = %q, want the replaced original", text)
	}
}

// TestStore_ConcurrentAccess exercises the mutex under -race: one pipeline
// is shared by all agents, whose tool calls run concurrently.
func TestStore_ConcurrentAccess(t *testing.T) {
	s := NewStore()
	var wg sync.WaitGroup
	for i := 1; i <= 64; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			id := fmt.Sprintf("elide-%d", n)
			s.Put(id, strings.Repeat("x", n))
			if text, ok := s.Get(id); !ok || len(text) != n {
				t.Errorf("Get(%s) = (%d chars, %v), want %d chars", id, len(text), ok, n)
			}
		}(i)
	}
	wg.Wait()
	if _, ok := s.Get("elide-65"); ok {
		t.Error("Get(missing id) must report a miss")
	}
}
