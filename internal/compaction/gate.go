package compaction

// GateConfig carries the elide-decision safety semantics of the reference
// pipeline (jev-compaction pipeline.py GateConfig): the score under which a
// segment may be elided, the token floor under which gating is skipped
// entirely, the elide-fraction tripwire, and per-kind score floors.
//
// Apply it to a pipeline with ApplyGateConfig; a pipeline that never had a
// gate config applied runs on DefaultGateConfig.
type GateConfig struct {
	// KeepThreshold is the score strictly below which a segment is elided.
	// Applying a config with a value in (0, 1] makes it the relocation
	// threshold exactly as EnableRelocation would (whichever was set last
	// stays in force); 0 or an out-of-range value leaves the current
	// threshold untouched. Reference: keep_threshold=0.35.
	KeepThreshold float64
	// MinGateTokens is the estimated-token floor under which
	// CompactToolOutput skips scoring entirely and returns the input
	// unchanged: below it the scoring round trip costs more than any
	// possible elision saves. 0 disables the token gate. Reference:
	// min_gate_tokens=400.
	MinGateTokens int
	// MaxElideFraction is the tripwire: when the scorer wants to elide more
	// than this share of the output's tokens, the scorer is distrusted and
	// NOTHING is elided (the tripwire is recorded in the result and the
	// shadow log). Values outside (0, 1] fall back to the default; 1
	// effectively disables the tripwire. Reference: max_elide_fraction=0.7.
	MaxElideFraction float64
	// ProtectedKinds maps segment kinds to their own elision floor: a
	// segment of that kind is only elided below the floor, no matter its
	// score relative to KeepThreshold. nil means the reference defaults
	// (stacktrace and diff at 0.05); pass an empty non-nil map for no
	// protected kinds. Reference: protected_kinds={stacktrace,diff} at
	// protected_floor=0.05.
	ProtectedKinds map[SegmentKind]float64
}

// Reference-parity gate defaults (jev-compaction pipeline.py).
const (
	// DefaultMinGateTokens is the token floor under which gating is skipped:
	// the round trip costs more than elision could save.
	DefaultMinGateTokens = 400
	// DefaultMaxElideFraction is the tripwire fraction: past it the scorer
	// is distrusted and nothing is elided.
	DefaultMaxElideFraction = 0.7
	// DefaultProtectedFloor is the score floor for protected kinds:
	// stacktrace and diff segments are elided only below it.
	DefaultProtectedFloor = 0.05
)

// TripwireMaxElideFraction is the CompactResult.Tripwire value recorded when
// the max-elide-fraction tripwire fired (everything kept).
const TripwireMaxElideFraction = "max_elide_fraction"

// TripwireAction is the shadow-log action recorded on a tripwire entry.
const TripwireAction = "tripwire"

// DefaultGateConfig returns the reference-parity gate configuration.
func DefaultGateConfig() GateConfig {
	return GateConfig{
		KeepThreshold:    DefaultRelocationThreshold,
		MinGateTokens:    DefaultMinGateTokens,
		MaxElideFraction: DefaultMaxElideFraction,
		ProtectedKinds: map[SegmentKind]float64{
			KindStacktrace: DefaultProtectedFloor,
			KindDiff:       DefaultProtectedFloor,
		},
	}
}

// clamped normalizes cfg the way the config resolvers do: out-of-range
// values fall back to safe behavior instead of eliding everything or
// nothing by accident. KeepThreshold outside (0, 1] becomes 0 — "unset", the
// armed relocation threshold stays in force. MaxElideFraction outside (0, 1]
// falls back to DefaultMaxElideFraction. A negative MinGateTokens becomes 0
// (the token gate is a floor, never a mandate). A nil ProtectedKinds map
// means the reference defaults; per-kind floors are clamped into [0, 1].
func (g GateConfig) clamped() GateConfig {
	out := g
	if out.KeepThreshold <= 0 || out.KeepThreshold > 1 {
		out.KeepThreshold = 0
	}
	if out.MaxElideFraction <= 0 || out.MaxElideFraction > 1 {
		out.MaxElideFraction = DefaultMaxElideFraction
	}
	if out.MinGateTokens < 0 {
		out.MinGateTokens = 0
	}
	if out.ProtectedKinds == nil {
		out.ProtectedKinds = DefaultGateConfig().ProtectedKinds
	} else {
		floors := make(map[SegmentKind]float64, len(out.ProtectedKinds))
		for kind, floor := range out.ProtectedKinds {
			if floor < 0 {
				floor = 0
			}
			if floor > 1 {
				floor = 1
			}
			floors[kind] = floor
		}
		out.ProtectedKinds = floors
	}
	return out
}

// resolvedGate is the gate as it applies at decision time: the stored (or
// default) config plus the keep threshold actually in force — the relocation
// threshold set by ApplyGateConfig or EnableRelocation, whichever last.
type resolvedGate struct {
	cfg  GateConfig
	keep float64
}

// floor returns the score under which a segment of kind may be elided: the
// protected floor when the kind has one, else the keep threshold.
func (g resolvedGate) floor(kind SegmentKind) float64 {
	if f, ok := g.cfg.ProtectedKinds[kind]; ok {
		return f
	}
	return g.keep
}

// resolveGate snapshots the effective gate configuration. Safe for
// concurrent use (one pipeline is shared by every agent's tool calls).
func (p *Pipeline) resolveGate() resolvedGate {
	if p == nil {
		return resolvedGate{cfg: DefaultGateConfig(), keep: DefaultRelocationThreshold}
	}
	p.relocMu.Lock()
	defer p.relocMu.Unlock()
	return p.resolveGateLocked()
}

// resolveGateLocked is resolveGate without the lock; callers hold relocMu.
func (p *Pipeline) resolveGateLocked() resolvedGate {
	cfg := DefaultGateConfig()
	if p.gateSet {
		cfg = p.gate
	}
	return resolvedGate{cfg: cfg, keep: p.threshold}
}

// minGateTokens returns the token floor under which CompactToolOutput skips
// scoring (0 disables the gate). A pipeline without an applied gate config
// runs on the reference default.
func (p *Pipeline) minGateTokens() int {
	if p == nil {
		return 0
	}
	p.relocMu.Lock()
	defer p.relocMu.Unlock()
	if p.gateSet {
		return p.gate.MinGateTokens
	}
	return DefaultGateConfig().MinGateTokens
}

// ApplyGateConfig stores cfg as the pipeline's gate configuration, replacing
// any previous one (a partial struct replaces the whole gate; base partial
// overrides on DefaultGateConfig). A positive KeepThreshold in (0, 1] also
// becomes the relocation threshold, exactly as EnableRelocation would set
// it — whichever of the two was applied last stays in force, so the
// -compaction-threshold flag keeps precedence by being applied after the
// gate. Out-of-range values are clamped (see GateConfig.clamped). Safe for
// concurrent use; a nil pipeline is a no-op.
func (p *Pipeline) ApplyGateConfig(cfg GateConfig) {
	if p == nil {
		return
	}
	g := cfg.clamped()
	p.relocMu.Lock()
	defer p.relocMu.Unlock()
	p.gate = g
	p.gateSet = true
	if g.KeepThreshold > 0 {
		p.threshold = g.KeepThreshold
	}
}

// logTripwire records one tripwire entry in the shadow log: the scorer
// wanted to elide past MaxElideFraction, so nothing was elided. Append
// failures are ignored — logging must not be able to break compaction, and
// the tripwire has already done its job by the time this runs.
func (p *Pipeline) logTripwire(taskHash string, totalTokens int) {
	if p == nil || p.shadow == nil {
		return
	}
	_ = p.shadow.Append(ShadowEntry{
		TS:       p.now(),
		TaskHash: taskHash,
		Tokens:   totalTokens,
		Type:     EntryTypeTripwire,
		Action:   TripwireAction,
	})
}
