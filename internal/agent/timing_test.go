package agent

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/dantearo/arkex/internal/config"
	"github.com/dantearo/arkex/internal/provider"
)

func timingModel(t testing.TB, url string) *provider.Model {
	t.Helper()
	m, err := provider.Open(t.Context(), config.ModelRef{Conn: config.Connection{API: config.APIOpenAICompat, BaseURL: url}, Model: config.Model{ID: "test"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestRequestTimingBoundaries(t *testing.T) {
	for _, mode := range []string{"reasoning", "text", "tool", "empty", "error"} {
		t.Run(mode, func(t *testing.T) {
			reasoningSeen := make(chan struct{}, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				if mode == "error" {
					http.Error(w, "bad request", 400)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprintf(w, "data: %s\n\n", delta(`{"role":"assistant"}`, ""))
				w.(http.Flusher).Flush()
				time.Sleep(40 * time.Millisecond)
				switch mode {
				case "reasoning":
					_, _ = fmt.Fprintf(w, "data: %s\n\n", delta(`{"reasoning_content":"consider"}`, ""))
					w.(http.Flusher).Flush()
					// Start the measured pause after the client observes reasoning,
					// not when the server writes it: transport delay is unmeasured.
					select {
					case <-reasoningSeen:
					case <-r.Context().Done():
						return
					case <-time.After(5 * time.Second):
						return
					}
					time.Sleep(20 * time.Millisecond)
					sse(w, delta(`{"content":"answer"}`, ""), delta(`{}`, "stop"))
				case "text":
					sse(w, delta(`{"content":"answer"}`, ""), delta(`{}`, "stop"))
				case "tool":
					sse(w, delta(`{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"read","arguments":"{}"}}]}`, ""), delta(`{}`, "tool_calls"))
				default:
					sse(w, delta(`{}`, "stop"))
				}
			}))
			defer srv.Close()
			a := &Agent{Model: timingModel(t, srv.URL)}
			var got RequestTiming
			var reasoning ReasoningTime
			count := 0
			_, err := a.stream(t.Context(), func(e Event) {
				switch e := e.(type) {
				case RequestTiming:
					got = e
					count++
				case ReasoningDelta:
					select {
					case reasoningSeen <- struct{}{}:
					default:
					}
				case ReasoningTime:
					reasoning = e
				}
			})
			if (err != nil) != (mode == "error") {
				t.Fatalf("unexpected error: %v", err)
			}
			if count != 1 || got.Prepare < 0 || got.Dispatch < got.Prepare || got.Total < got.Dispatch || got.Connection < 0 || got.Connection > got.Dispatch {
				t.Fatalf("bad boundaries: %+v count=%d", got, count)
			}
			if mode == "error" || mode == "empty" {
				if got.FirstToken != 0 {
					t.Fatalf("invented content timing: %+v", got)
				}
			} else if got.FirstToken < 40*time.Millisecond || got.FirstToken > got.Total {
				t.Fatalf("headers counted as content: %+v", got)
			}
			if mode == "reasoning" && (reasoning.Duration < 20*time.Millisecond || reasoning.Duration > got.Total-got.FirstToken+5*time.Millisecond) {
				t.Fatalf("reasoning includes initial wait: %+v %+v", reasoning, got)
			}
		})
	}
}

// Loopback isolates client costs; it is not a comparison of model vendors or
// terminal rendering. Both paths use the same provider, payload and warm pool.
func BenchmarkRequestOverhead(b *testing.B) {
	for _, size := range []int{32, 256 * 1024} {
		for _, harness := range []bool{false, true} {
			b.Run(fmt.Sprintf("bytes=%d/harness=%t", size, harness), func(b *testing.B) {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					_, _ = io.Copy(io.Discard, r.Body)
					w.Header().Set("Content-Type", "text/event-stream")
					sse(w, delta(`{"content":"ok"}`, ""), delta(`{}`, "stop"), usage())
				}))
				defer srv.Close()
				m := timingModel(b, srv.URL)
				msg := fantasy.NewUserMessage(strings.Repeat("x", size))
				a := &Agent{Model: m, Policy: AllowAll{}}
				samples := make([]time.Duration, b.N)
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					start := time.Now()
					if harness {
						a.SetMessages(nil)
						if err := a.RunMessage(b.Context(), msg, func(Event) {}); err != nil {
							b.Fatal(err)
						}
					} else {
						seq, err := m.LM.Stream(b.Context(), m.Call(fantasy.Prompt{msg}, nil))
						if err != nil {
							b.Fatal(err)
						}
						for part := range seq {
							if part.Type == fantasy.StreamPartTypeError {
								b.Fatal(part.Error)
							}
						}
					}
					samples[i] = time.Since(start)
				}
				b.StopTimer()
				sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
				b.ReportMetric(float64(samples[len(samples)/2].Nanoseconds())/1e6, "p50-ms")
				b.ReportMetric(float64(samples[(len(samples)-1)*95/100].Nanoseconds())/1e6, "p95-ms")
			})
		}
	}
}
