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
	// streamRetryBaseDelay is the backoff for the first retry.
	streamRetryBaseDelay = 500 * time.Millisecond
	// streamRetryMaxDelay caps a single backoff interval.
	streamRetryMaxDelay = 30 * time.Second
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

// isRetryableStreamError reports whether a failed LLM stream attempt should
// be automatically retried. Only infrastructure-style failures retry:
// network-level errors (timeouts, refused/reset connections, mid-body
// disconnects) and transient server responses (408/429/5xx). Anything else
// — including context cancellation and unknown errors — fails fast, exactly
// like the pre-retry behavior.
func isRetryableStreamError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var se *client.StatusError
	if errors.As(err, &se) {
		return se.StatusCode == 408 || se.StatusCode == 429 || se.StatusCode >= 500
	}
	return false
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
