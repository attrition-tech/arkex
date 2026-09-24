package agent

import (
	"context"
	"errors"
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
	Attempt int       `json:"attempt"`
	Until   time.Time `json:"until"`
}

func (RetryWait) isEvent() {}

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
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	var dns *net.DNSError
	if errors.As(err, &dns) && dns.IsTemporary {
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

func retryDelay(err error, attempt int) time.Duration {
	d := time.Second*time.Duration(1<<attempt) + time.Duration(rand.IntN(250))*time.Millisecond
	var pe *fantasy.ProviderError
	if errors.As(err, &pe) {
		for k, v := range pe.ResponseHeaders {
			if strings.EqualFold(k, "Retry-After") {
				if seconds, e := strconv.ParseFloat(v, 64); e == nil && seconds > 0 && seconds < 86400 {
					return max(d, time.Duration(seconds*float64(time.Second)))
				}
				if at, e := http.ParseTime(v); e == nil {
					return max(d, time.Until(at))
				}
			}
		}
	}
	return d
}

func (a *Agent) streamWithRetry(ctx context.Context, emit func(Event)) (turnResult, error) {
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
		if err == nil || ctx.Err() != nil {
			return turn, err
		}
		invalid := errors.Is(err, ErrIncompleteResponse) || errors.Is(err, ErrMalformedToolResponse)
		if !invalid && !retryableRequestError(err) {
			return turn, err
		}
		// Invalid responses never execute tools. Retry them twice even when
		// text streamed; other partial failures still require consent.
		if (invalid && attempt >= 2) || (!invalid && (partial || attempt >= 3)) {
			if a.Retry == nil {
				return turn, err
			}
			if !a.Retry(ctx, err, partial) {
				return turn, context.Canceled
			}
			attempt = 0
		} else {
			d := retryDelay(err, attempt)
			attempt++
			emit(RetryWait{Attempt: attempt, Until: time.Now().Add(d)})
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
		emit(RequestRestart{Partial: partial})
	}
}
