package agent

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/attrition-tech/arkex/internal/config"
	"github.com/attrition-tech/arkex/internal/provider"
	"github.com/attrition-tech/arkex/internal/tools"
)

func TestRetryableNetworkErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"raw TCP read timeout", &net.OpError{Op: "read", Net: "tcp", Err: os.ErrDeadlineExceeded}, true},
		{"wrapped reset", &url.Error{Op: "Post", URL: "https://example.test", Err: &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}}, true},
		{"request deadline", context.DeadlineExceeded, true},
		{"truncated stream", io.ErrUnexpectedEOF, true},
		{"closed connection", io.EOF, true},
		{"offline", syscall.ENETUNREACH, true},
		{"connection refused", syscall.ECONNREFUSED, true},
		{"temporary DNS", &net.DNSError{IsTemporary: true}, true},
		{"unknown hostname", &net.DNSError{IsNotFound: true}, false},
		{"TLS certificate", &url.Error{Op: "Post", Err: x509.UnknownAuthorityError{}}, false},
		{"user cancel", context.Canceled, false},
		{"bad request with EOF cause", &fantasy.ProviderError{StatusCode: 400, Cause: io.EOF}, false},
		{"auth", &fantasy.ProviderError{AuthError: true, Cause: io.EOF}, false},
		{"wrapped transport", &fantasy.ProviderError{Cause: syscall.ECONNRESET}, true},
		{"other error", errors.New("invalid model configuration"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := retryableRequestError(tc.err); got != tc.want {
				t.Fatalf("retryable=%v want %v: %v", got, tc.want, tc.err)
			}
		})
	}
}

// Exercise the real HTTP client and provider's SSE reader, not a fake model.
func TestNetworkTimeoutRecovery(t *testing.T) {
	for _, stage := range []string{"headers", "empty stream", "partial stream", "cancel run"} {
		t.Run(stage, func(t *testing.T) {
			t.Parallel()
			var requests atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				if requests.Add(1) == 1 {
					if stage != "headers" {
						w.Header().Set("Content-Type", "text/event-stream")
						text := ""
						if stage == "partial stream" {
							text = "UNFINISHED"
						}
						_, _ = fmt.Fprintf(w, "data: %s\n\n", delta(`{"content":"`+text+`"}`, ""))
						w.(http.Flusher).Flush()
					}
					<-r.Context().Done() // hold the socket until the real client times out
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				sse(w, delta(`{"content":"RECOVERED"}`, ""), delta(`{}`, "stop"))
			}))
			defer srv.Close()
			lm, err := provider.Open(t.Context(), config.ModelRef{Conn: config.Connection{API: config.APIOpenAICompat, BaseURL: srv.URL}, Model: config.Model{ID: "test"}}, &http.Client{Timeout: 150 * time.Millisecond})
			if err != nil {
				t.Fatal(err)
			}
			a := &Agent{Model: lm, Policy: AllowAll{}}
			prompts, waits := 0, 0
			a.Retry = func(_ context.Context, err error, partial bool) bool {
				prompts++
				var ne net.Error
				if !partial || !errors.As(err, &ne) || !ne.Timeout() {
					t.Errorf("wrong interruption: partial=%v err=%v", partial, err)
				}
				return true
			}
			ctx := t.Context()
			if stage == "cancel run" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 50*time.Millisecond)
				defer cancel()
			}
			err = a.Run(ctx, "hi", func(e Event) {
				if _, ok := e.(RetryWait); ok {
					waits++
				}
			})
			if stage == "cancel run" {
				if !errors.Is(err, context.DeadlineExceeded) || requests.Load() != 1 || waits != 0 || prompts != 0 {
					t.Fatalf("canceled run restarted: %v %d %d %d", err, requests.Load(), waits, prompts)
				}
				return
			}
			if err != nil || requests.Load() != 2 {
				t.Fatalf("err=%v requests=%d", err, requests.Load())
			}
			if (stage == "partial stream" && (prompts != 1 || waits != 0)) || (stage != "partial stream" && (prompts != 0 || waits != 1)) {
				t.Fatalf("stage=%s prompts=%d waits=%d", stage, prompts, waits)
			}
			if len(a.messages) != 2 || a.messages[1].Content[0].(fantasy.TextPart).Text != "RECOVERED" {
				t.Fatal("interrupted response entered conversation")
			}
		})
	}
}

func TestNetworkDisconnectDoesNotRepeatCompletedCommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX command")
	}
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		n := requests.Add(1)
		if n == 2 {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			_ = conn.Close() // socket disappears after completed tool execution
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if n == 1 {
			sse(w, delta(`{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"bash","arguments":"{\"command\":\"printf X >> executions; echo TOOL_DONE\"}"}}]}`, ""), delta(`{}`, "tool_calls"))
		} else {
			if !strings.Contains(string(body), "TOOL_DONE") {
				t.Error("retry lost completed tool result")
			}
			sse(w, delta(`{"content":"RECOVERED"}`, ""), delta(`{}`, "stop"))
		}
	}))
	defer srv.Close()
	dir := t.TempDir()
	a := &Agent{Model: timingModel(t, srv.URL), Tools: tools.Default(dir), Policy: AllowAll{}}
	waits := 0
	err := a.Run(t.Context(), "go", func(e Event) {
		if _, ok := e.(RetryWait); ok {
			waits++
		}
	})
	b, readErr := os.ReadFile(filepath.Join(dir, "executions"))
	if err != nil || readErr != nil || string(b) != "X" || requests.Load() != 3 || waits != 1 {
		t.Fatalf("err=%v read=%v executions=%q requests=%d waits=%d", err, readErr, b, requests.Load(), waits)
	}
}

func TestRetryPreservesCompletedTools(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		n := calls.Add(1)
		if n == 2 || n == 3 {
			http.Error(w, "unavailable", http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if n == 1 {
			sse(w, delta(`{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"read","arguments":"{\"path\":\"hello\"}"}}]}`, ""), delta(`{}`, "tool_calls"))
		} else {
			sse(w, delta(`{"content":"Finished"}`, ""), delta(`{}`, "stop"))
		}
	}))
	defer srv.Close()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hello"), []byte("hello"), 0600); err != nil {
		t.Fatal(err)
	}
	a := &Agent{Model: timingModel(t, srv.URL), Tools: tools.Default(dir), Policy: AllowAll{}}
	results, waits := 0, 0
	err := a.Run(t.Context(), "read", func(e Event) {
		switch e.(type) {
		case ToolResult:
			results++
		case RetryWait:
			waits++
		}
	})
	if err != nil || results != 1 || waits != 2 || calls.Load() != 4 || len(a.Messages()) != 4 {
		t.Fatalf("err=%v results=%d waits=%d calls=%d messages=%d", err, results, waits, calls.Load(), len(a.Messages()))
	}
}

func TestRetryExhaustionCanRestartAndCancel(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	prompts := 0
	a := &Agent{Model: timingModel(t, srv.URL), Policy: AllowAll{}}
	a.Retry = func(_ context.Context, _ error, partial bool) bool {
		prompts++
		if partial || calls.Load() != int32(prompts*4) {
			t.Errorf("unexpected boundary: partial=%v calls=%d", partial, calls.Load())
		}
		return prompts == 1
	}
	err := a.Run(t.Context(), "hi", func(Event) {})
	if !errors.Is(err, context.Canceled) || prompts != 2 || calls.Load() != 8 {
		t.Fatalf("%v prompts=%d calls=%d", err, prompts, calls.Load())
	}
}

func TestIncompleteResponseRetriesWithoutConsent(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if calls.Add(1) == 1 {
			_, _ = fmt.Fprintf(w, "data: %s\n\n", delta(`{"content":"Unfinished"}`, ""))
			return
		}
		sse(w, delta(`{"content":"Replacement"}`, ""), delta(`{}`, "stop"))
	}))
	defer srv.Close()
	a := &Agent{Model: timingModel(t, srv.URL), Policy: AllowAll{}}
	confirmed := false
	a.Retry = func(_ context.Context, _ error, partial bool) bool { confirmed = partial; return true }
	restarts := 0
	err := a.Run(t.Context(), "hi", func(e Event) {
		if r, ok := e.(RequestRestart); ok && r.Partial {
			restarts++
		}
	})
	if err != nil || confirmed || restarts != 1 || calls.Load() != 2 {
		t.Fatalf("err=%v consent=%v restarts=%d calls=%d", err, confirmed, restarts, calls.Load())
	}
	if len(a.messages) != 2 || a.messages[1].Content[0].(fantasy.TextPart).Text != "Replacement" {
		t.Fatal("partial response replayed")
	}
}

func TestRetryCancelWaitAndPermanentFailure(t *testing.T) {
	for _, status := range []int{401, 400, 502} {
		var calls atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); http.Error(w, "failure", status) }))
		a := &Agent{Model: timingModel(t, srv.URL), Policy: AllowAll{}}
		ctx, cancel := context.WithCancel(t.Context())
		waits := 0
		err := a.Run(ctx, "hi", func(e Event) {
			if _, ok := e.(RetryWait); ok {
				waits++
				cancel()
			}
		})
		cancel()
		srv.Close()
		if err == nil || calls.Load() != 1 || (status == 502 && (waits != 1 || !errors.Is(err, context.Canceled))) || (status != 502 && waits != 0) {
			t.Fatalf("status=%d err=%v waits=%d calls=%d", status, err, waits, calls.Load())
		}
	}
	if d := retryDelay(&fantasy.ProviderError{ResponseHeaders: map[string]string{"Retry-After": "12"}}, 0); d != 12*time.Second {
		t.Fatal(d)
	}
}
