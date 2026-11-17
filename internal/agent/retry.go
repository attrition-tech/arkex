package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"charm.land/fantasy"
)

type RetryWait struct {
	Attempt int       `json:"attempt"` // zero for a manually requested retry
	Limit   int       `json:"limit"`
	Until   time.Time `json:"until"`
	Reason  string    `json:"reason"`
	Partial bool      `json:"partial"`
}

func (RetryWait) isEvent() {}

// RequestRecovered is emitted only after a restarted response completes.
type RequestRecovered struct {
	Retries int           `json:"retries"`
	Elapsed time.Duration `json:"elapsed_ns"`
}

func (RequestRecovered) isEvent() {}

// RequestRestart discards only the interrupted attempt's UI output.
type RequestRestart struct {
	Partial bool `json:"partial"`
}

func (RequestRestart) isEvent() {}

var ErrIncompleteResponse = errors.New("response ended without a completion event")
var ErrMalformedToolResponse = errors.New("malformed tool response: DSML parameters arrived as text, not a tool call")

// Only recognize raw protocol tags at the start of an unquoted line. Mentions,
// inline code, block quotes, indented code and fenced examples are not calls.
var dsmlTag = regexp.MustCompile(`^<\s*[|｜]\s*DSML\s*[|｜]\s*(?:invoke\s+name=["']|parameter\s+name=["']|function_calls\s*>)`)

func malformedToolText(text string) bool {
	var fence byte
	var fenceLen int
	for _, line := range strings.Split(text, "\n") {
		trim := strings.TrimLeft(line, " ")
		if len(trim) > 0 && (trim[0] == '`' || trim[0] == '~') {
			n := len(trim) - len(strings.TrimLeft(trim, string(trim[0])))
			if n >= 3 {
				if fence == 0 {
					fence, fenceLen = trim[0], n
				} else if trim[0] == fence && n >= fenceLen && strings.TrimSpace(trim[n:]) == "" {
					fence = 0
				}
				continue
			}
		}
		if fence == 0 && !strings.HasPrefix(line, "    ") && !strings.HasPrefix(line, "\t") && dsmlTag.MatchString(trim) {
			return true
		}
	}
	return false
}

// retryableRequestError includes transport failures that can arrive directly
// from an SSE body reader, not only errors wrapped by a provider. The caller
// checks the run context separately: a request timeout may recover, but a
// canceled/expired run must never restart.
func retryableRequestError(err error) bool {
	if errors.Is(err, context.Canceled) || IsContextOverflow(err) {
		return false
	}
	var pe *fantasy.ProviderError
	if errors.As(err, &pe) {
		if pe.AuthError {
			return false
		}
		if pe.IsRetryable() {
			return true
		}
		if pe.StatusCode >= 400 {
			return false
		}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	var dns *net.DNSError
	if errors.As(err, &dns) && dns.IsTemporary {
		return true
	}
	// Go exposes this TCP TLS alert only through net.OpError, not an
	// exported sentinel. Retry the failed connection, never relax TLS checks
	// or treat other TLS errors (especially certificate failures) as transient.
	var op *net.OpError
	if errors.As(err, &op) && op.Op == "local error" && op.Err != nil && op.Err.Error() == "tls: bad record MAC" {
		return true
	}
	for _, transient := range []error{io.EOF, io.ErrUnexpectedEOF, syscall.ECONNRESET,
		syscall.ECONNABORTED, syscall.ECONNREFUSED, syscall.EPIPE,
		syscall.ENETDOWN, syscall.ENETUNREACH, syscall.EHOSTUNREACH, syscall.ETIMEDOUT} {
		if errors.Is(err, transient) {
			return true
		}
	}
	return false
}

func retryAfter(err error) time.Duration {
	var pe *fantasy.ProviderError
	if errors.As(err, &pe) {
		for k, v := range pe.ResponseHeaders {
			if strings.EqualFold(k, "Retry-After") {
				if seconds, e := strconv.ParseFloat(v, 64); e == nil && seconds > 0 {
					// Saturate instead of overflowing or ignoring a very long delay.
					seconds = min(seconds, float64((1<<63-1)/int64(time.Second)))
					return time.Duration(seconds * float64(time.Second))
				}
				if at, e := http.ParseTime(v); e == nil {
					return max(0, time.Until(at))
				}
			}
		}
	}
	return 0
}

func retryDelay(err error, attempt int) time.Duration {
	d := time.Second*time.Duration(1<<attempt) + time.Duration(rand.IntN(250))*time.Millisecond
	return max(d, retryAfter(err))
}

func (a *Agent) streamWithRetry(ctx context.Context, emit func(Event)) (turnResult, error) {
	const maxAutomaticWait = 30 * time.Second
	var waited time.Duration
	var interrupted time.Time
	retries := 0
	for attempt := 0; ; {
		partial := false
		turn, err := a.stream(ctx, func(e Event) {
			switch v := e.(type) {
			case TextDelta:
				partial = partial || v.Text != ""
			case ReasoningDelta:
				partial = partial || v.Text != ""
			case ToolCallStart, ToolCallInputDelta:
				partial = true
			}
			emit(e)
		})
		if ctx.Err() != nil {
			return turn, ctx.Err()
		}
		if err == nil {
			if retries > 0 {
				emit(RequestRecovered{Retries: retries, Elapsed: time.Since(interrupted)})
			}
			return turn, err
		}
		invalid := errors.Is(err, ErrIncompleteResponse) || errors.Is(err, ErrMalformedToolResponse)
		if !invalid && !retryableRequestError(err) {
			return turn, err
		}
		if interrupted.IsZero() {
			interrupted = time.Now()
		}
		limit := 3
		if invalid {
			limit = 2
		}
		// Tools execute only after a complete successful stream, so partial
		// text is safe to replace. All failure classes share this allowance.
		d := retryDelay(err, attempt)
		advice := retryAfter(err)
		advisedUntil := time.Now().Add(advice)
		reason := err.Error()
		if attempt >= limit || d > maxAutomaticWait-waited {
			stopped := fmt.Errorf("automatic recovery stopped after %d retries: %w", attempt, err)
			if attempt < limit {
				stopped = fmt.Errorf("next retry needs a %s delay, exceeding the remaining automatic wait budget: %w", d.Round(time.Second), err)
			}
			if a.Retry == nil {
				return turn, stopped
			}
			if !a.Retry(ctx, stopped, partial) {
				return turn, context.Canceled
			}
			attempt, waited = 0, 0
			// Consent grants another bounded cycle, not permission to ignore
			// Retry-After. Time spent in the dialog counts toward that deadline.
			d = max(0, time.Until(advisedUntil))
		} else {
			attempt++
			waited += d
		}
		if ctx.Err() != nil {
			return turn, ctx.Err()
		}
		if d > 0 {
			emit(RetryWait{Attempt: attempt, Limit: limit, Until: time.Now().Add(d), Reason: reason, Partial: partial})
			timer := time.NewTimer(d)
			select {
			case <-ctx.Done():
				timer.Stop()
				return turn, ctx.Err()
			case <-timer.C:
			}
		}
		if ctx.Err() != nil {
			return turn, ctx.Err()
		}
		retries++
		emit(RequestRestart{Partial: partial})
	}
}
