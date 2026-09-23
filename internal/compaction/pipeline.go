package compaction

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// Pipeline ties segmentation, scoring, and the shadow log together for the
// tool layer. Stage 1 (shadow-only) is ScoreToolOutput: it records decisions
// and returns scores without ever mutating agent behavior. Stage 2
// (relocation, armed with EnableRelocation) additionally elides low-scoring
// segments from tool results and stores their originals for the expand tool.
type Pipeline struct {
	client      *DecisionClient
	shadow      *ShadowLog
	maxSegChars int
	// now is the clock for shadow-log timestamps; a var solely for tests.
	now func() time.Time

	// Relocation state (stage 2). relocMu guards the armed store, the
	// elision threshold, and the per-pipeline elide-id counter: one pipeline
	// is shared by the root agent and every subagent, whose tool calls run
	// concurrently.
	relocMu   sync.Mutex
	reloc     *Store
	threshold float64
	nextElide int
}

// PipelineOptions tunes the pipeline; zero values are production defaults.
type PipelineOptions struct {
	// MaxSegChars caps one segment's size in bytes (DefaultMaxSegChars when
	// 0 or negative).
	MaxSegChars int
	// HTTPClient overrides the decision client's transport (tests inject
	// fast/recorded transports here). Nil uses the stdlib default with a
	// 30s per-attempt timeout.
	HTTPClient *http.Client
}

// NewPipeline builds a shadow-only scoring pipeline. backend must be ready
// to call (ResolveBackend fills in the gateway's URL); apiKey overrides
// backend.APIKey when non-empty. shadow may be nil, which disables logging
// (scores still flow — useful for dry runs).
func NewPipeline(backend ResolvedBackend, apiKey string, shadow *ShadowLog, opts PipelineOptions) *Pipeline {
	c := NewDecisionClient(backend, apiKey)
	if opts.HTTPClient != nil {
		c.http = opts.HTTPClient
	}
	max := opts.MaxSegChars
	if max <= 0 {
		max = DefaultMaxSegChars
	}
	return &Pipeline{client: c, shadow: shadow, maxSegChars: max, now: time.Now}
}

// SegmentScores is the result of scoring one tool output.
type SegmentScores struct {
	// Segments is the segmentation of the tool output.
	Segments []Segment
	// Scores maps segment ID → score in [0,1]; 1.0 also covers items that
	// failed to score (fail-open). Always populated for every segment.
	Scores map[string]float64
	// Errors lists scoring and shadow-log failures (each scoring failure is
	// an *ItemScoreError keyed by segment ID). Non-fatal by contract.
	Errors []error
	// TaskHash is the digest logged alongside each decision.
	TaskHash string
}

// scoreTask derives the ongoing-task description for one tool output. Stage
// 1 has no ambient task to thread through (the tool layer is wired in the
// relocation stage), so the task is derived from the tool's name; the
// relocation stage can pass a richer task through then.
func scoreTask(toolName string) string {
	return fmt.Sprintf("Preserve what the %s tool output contributed toward the ongoing task.", toolName)
}

// ScoreToolOutput segments the tool output, scores every segment against the
// ongoing task, appends one shadow-log line per segment, and returns the
// per-segment scores.
//
// Nothing is elided or relocated here: in shadow mode every decision is
// recorded as "keep" so Replay() can quantify what a threshold would have
// elided; when relocation is armed (EnableRelocation) below-threshold scores
// are recorded as "elide" — the elision itself happens in CompactToolOutput.
//
// The error return is non-nil exactly when at least one segment failed to
// score (mirroring DecisionClient.ScoreBatch's joined item errors); the
// scores themselves are always usable thanks to fail-open.
func (p *Pipeline) ScoreToolOutput(ctx context.Context, toolName, output string) (SegmentScores, error) {
	var out SegmentScores
	if p == nil || p.client == nil {
		return out, fmt.Errorf("compaction: pipeline has no decision client")
	}

	task := scoreTask(toolName)
	out.TaskHash = HashTask(task)
	out.Segments = SegmentSegments(output, p.maxSegChars)
	if len(out.Segments) == 0 {
		return out, nil
	}

	items := make(map[string]Item, len(out.Segments))
	for _, s := range out.Segments {
		items[s.ID] = Item{Text: s.Text, Tokens: s.Tokens}
	}
	scores, err := p.client.ScoreBatch(ctx, task, items)
	out.Scores = scores
	if err != nil {
		out.Errors = append(out.Errors, err)
	}

	// Shadow log: one line per segment. In shadow mode (stage 1) the decision
	// is always keep. When relocation is armed (stage 2), the recorded
	// decision reflects what CompactToolOutput does with this score: elide
	// strictly below threshold, keep otherwise. Append failures are recorded
	// but never fail the call — logging must not be able to break scoring.
	if p.shadow != nil {
		now := p.now()
		relocStore, threshold := p.relocationArmed()
		for _, s := range out.Segments {
			score, ok := out.Scores[s.ID]
			if !ok {
				score = keepScore
			}
			decision := DecisionKeep
			if relocStore != nil && score < threshold {
				decision = DecisionElide
			}
			if aerr := p.shadow.Append(ShadowEntry{
				TS:        now,
				TaskHash:  out.TaskHash,
				SegmentID: s.ID,
				Tokens:    s.Tokens,
				Score:     score,
				Decision:  decision,
			}); aerr != nil {
				out.Errors = append(out.Errors, fmt.Errorf("shadow log append for %s: %w", s.ID, aerr))
			}
		}
	}
	return out, err
}
