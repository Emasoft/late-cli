package executor

import (
	"context"
	"errors"
	"io"
	"math/rand/v2"
	"net"
	"net/url"
	"time"

	"late/internal/client"
	"late/internal/common"
)

const (
	// DefaultMaxStreamRetries is the default retry budget for a failing LLM
	// stream call. Overridable via -max-stream-retries / LATE_MAX_STREAM_RETRIES.
	DefaultMaxStreamRetries = 100
	// DefaultMaxBadBodyRetries is the dedicated retry budget for HTTP 400
	// responses. A 400 "read body failed" from strict OpenAI-compatible
	// gateways (e.g. z.ai/GLM) is frequently a transient upstream failure,
	// and a handful of quick retries resolves it; genuinely malformed
	// requests still fail after this small, bounded budget. It is
	// deliberately much smaller than the infrastructure budget
	// (DefaultMaxStreamRetries). Not exposed as a CLI flag.
	DefaultMaxBadBodyRetries = 3
	// streamRetryBaseDelay is the backoff for the first retry.
	streamRetryBaseDelay = 500 * time.Millisecond
	// streamRetryMaxDelay caps a single backoff interval.
	streamRetryMaxDelay = 30 * time.Second
)

// streamRetryClass buckets a failed stream attempt into a retry tier:
// retryClassNone fails fast, retryClassInfra draws from the infrastructure
// budget (transport/5xx/429/408), and retryClassBadBody draws from the
// separate, much smaller bad-body budget (HTTP 400 body-parse rejections,
// frequently transient on strict OpenAI-compatible gateways such as
// z.ai/GLM).
type streamRetryClass int

const (
	// retryClassNone means the error must fail fast: cancellation, permanent
	// client errors, and anything unknown.
	retryClassNone streamRetryClass = iota
	// retryClassInfra covers infrastructure-style failures: network-level
	// errors (timeouts, refused/reset connections, mid-body disconnects) and
	// transient server responses (408/429/5xx).
	retryClassInfra
	// retryClassBadBody covers HTTP 400 body-parse rejections, which strict
	// OpenAI-compatible gateways often emit transiently.
	retryClassBadBody
)

// maxStreamRetryAttempt clamps the attempt exponent so the
// 1<<(attempt-1) shift can never overflow before the cap is applied.
const maxStreamRetryAttempt = 40

// streamRetryDelay returns the exponentially growing, jittered wait before
// retry attempt `attempt` (1-based). The doubling delay is capped at
// streamRetryMaxDelay; full jitter spreads the wait uniformly over [0, cap]
// to avoid synchronized retry storms across agents.
func streamRetryDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > maxStreamRetryAttempt {
		attempt = maxStreamRetryAttempt
	}
	backoff := streamRetryBaseDelay * (1 << (attempt - 1))
	if backoff > streamRetryMaxDelay || backoff <= 0 { // <=0 guards shift overflow
		backoff = streamRetryMaxDelay
	}
	return rand.N(backoff)
}

// classifyStreamError buckets a failed LLM stream attempt into a retry tier.
// retryClassInfra covers infrastructure-style failures that draw from the
// main retry budget: network-level errors (timeouts, refused/reset
// connections, mid-body disconnects) and transient server responses
// (408/429/5xx). retryClassBadBody isolates HTTP 400 body-parse rejections,
// frequently transient on strict OpenAI-compatible gateways, into their own
// tier. Everything else — context cancellation, permanent client errors
// (401/403/404), and unknown errors — maps to retryClassNone and fails fast,
// exactly like the pre-retry behavior.
func classifyStreamError(err error) streamRetryClass {
	if err == nil {
		return retryClassNone
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return retryClassNone
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		return retryClassInfra
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return retryClassInfra
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return retryClassInfra
	}
	var se *client.StatusError
	if errors.As(err, &se) {
		if se.StatusCode == 400 {
			return retryClassBadBody
		}
		if se.StatusCode == 408 || se.StatusCode == 429 || se.StatusCode >= 500 {
			return retryClassInfra
		}
		return retryClassNone
	}
	return retryClassNone
}

// isRetryableStreamError reports whether a failed LLM stream attempt should
// be automatically retried from the infrastructure budget. See
// classifyStreamError for the tiering.
func isRetryableStreamError(err error) bool {
	return classifyStreamError(err) == retryClassInfra
}

// maxStreamRetriesFromContext resolves the retry budget from ctx, falling
// back to DefaultMaxStreamRetries. Negative values mean "disabled".
func maxStreamRetriesFromContext(ctx context.Context) int {
	if v, ok := ctx.Value(common.MaxStreamRetriesKey).(int); ok {
		if v < 0 {
			return 0
		}
		return v
	}
	return DefaultMaxStreamRetries
}

// maxBadBodyRetriesFromContext resolves the dedicated HTTP 400 retry budget
// from ctx, falling back to DefaultMaxBadBodyRetries. Negative values mean
// "disabled".
func maxBadBodyRetriesFromContext(ctx context.Context) int {
	if v, ok := ctx.Value(common.MaxBadBodyRetriesKey).(int); ok {
		if v < 0 {
			return 0
		}
		return v
	}
	return DefaultMaxBadBodyRetries
}
