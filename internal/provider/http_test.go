package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"charm.land/fantasy"
	"github.com/attrition-tech/arkex/internal/chatgpt"
	"github.com/attrition-tech/arkex/internal/config"
)

const completionStream = "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello there\"},\"finish_reason\":null}]}\n\n" +
	"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"

func TestInferenceStartupAndIdleTimeouts(t *testing.T) {
	store := &chatgpt.Store{Path: filepath.Join(t.TempDir(), "auth.json")}
	if err := store.Put("test", chatgpt.Tokens{AccessToken: "test", RefreshToken: "test", ClientID: "oaiapp_test", Subject: "test", Issuer: "https://auth.example", Scopes: "resource.invoke chatgpt.tokens.use.direct", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	old := AuthStore
	AuthStore = func() (*chatgpt.Store, error) { return store, nil }
	defer func() { AuthStore = old }()
	for _, kind := range []config.Kind{config.KindLLMServer, config.KindSubscription} {
		for _, stage := range []string{"headers", "idle", "active", "cancel headers", "cancel idle"} {
			t.Run(string(kind)+"/"+stage, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					_, _ = io.Copy(io.Discard, r.Body)
					if strings.HasPrefix(stage, "cancel") {
						defer cancel()
					}
					if stage == "headers" {
						<-r.Context().Done()
						return
					}
					if stage == "cancel headers" {
						cancel()
						<-r.Context().Done()
						return
					}
					w.Header().Set("Content-Type", "text/event-stream")
					w.WriteHeader(http.StatusOK)
					w.(http.Flusher).Flush()
					if stage == "idle" || stage == "cancel idle" {
						if stage == "cancel idle" {
							cancel()
						}
						<-r.Context().Done()
						return
					}
					// Heartbeats keep a response alive beyond both configured limits,
					// even when there are no answer or reasoning deltas yet.
					for range 8 {
						_, _ = fmt.Fprint(w, ": heartbeat\n\n")
						w.(http.Flusher).Flush()
						time.Sleep(50 * time.Millisecond)
					}
					stream := completionStream
					if kind == config.KindSubscription {
						stream = fakeStream
					}
					_, _ = fmt.Fprint(w, stream)
				}))
				defer srv.Close()
				m, err := Open(ctx, config.ModelRef{ConnID: "test", Conn: config.Connection{
					Kind: kind, BaseURL: srv.URL, HeaderTimeout: "100ms", IdleTimeout: "250ms",
				}, Model: config.Model{ID: "test"}}, nil)
				if err != nil {
					t.Fatal(err)
				}
				text, finished, err := readInference(ctx, m)
				if stage == "active" {
					if err != nil || !finished || text != "hello there" {
						t.Fatalf("healthy stream stopped: text=%q finished=%v err=%v", text, finished, err)
					}
					return
				}
				if err == nil || finished {
					t.Fatalf("stalled/cancelled response completed: text=%q finished=%v err=%v", text, finished, err)
				}
				if strings.HasPrefix(stage, "cancel") {
					if !errors.Is(err, context.Canceled) {
						t.Fatalf("cancellation became a timeout: %v", err)
					}
				} else if !strings.Contains(err.Error(), stage) || !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("missing timeout cause: %v", err)
				}
			})
		}
	}
}

// Read the public model stream, including failures opening it and reading it.
func readInference(ctx context.Context, m *Model) (string, bool, error) {
	seq, err := m.LM.Stream(ctx, m.Call(fantasy.Prompt{fantasy.NewUserMessage("test")}, nil))
	if err != nil {
		return "", false, err
	}
	var text strings.Builder
	finished := false
	for p := range seq {
		switch p.Type {
		case fantasy.StreamPartTypeTextDelta:
			text.WriteString(p.Delta)
		case fantasy.StreamPartTypeFinish:
			finished = true
		case fantasy.StreamPartTypeError:
			err = p.Error
		}
	}
	return text.String(), finished, err
}

type streamTransport func(*http.Request) (*http.Response, error)

func (f streamTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestInferenceOutlivesOldTotalTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		old := http.DefaultTransport
		defer func() { http.DefaultTransport = old }()
		http.DefaultTransport = streamTransport(func(r *http.Request) (*http.Response, error) {
			reader, writer := io.Pipe()
			stop := context.AfterFunc(r.Context(), func() { _ = reader.CloseWithError(context.Cause(r.Context())) })
			go func() {
				defer stop()
				defer func() { _ = writer.Close() }()
				for range 12 {
					if _, err := io.WriteString(writer, ": heartbeat\n\n"); err != nil {
						return
					}
					time.Sleep(time.Minute)
				}
				_, _ = io.WriteString(writer, completionStream)
			}()
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: reader, Request: r}, nil
		})
		m, err := Open(t.Context(), config.ModelRef{Conn: config.Connection{BaseURL: "http://test.invalid"}, Model: config.Model{ID: "test"}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		started := time.Now()
		text, finished, err := readInference(t.Context(), m)
		if err != nil || !finished || text != "hello there" || time.Since(started) != 12*time.Minute {
			t.Fatalf("healthy long response: text=%q finished=%v elapsed=%s err=%v", text, finished, time.Since(started), err)
		}
	})
}
