package agent

import (
	"context"
	"net/http/httptrace"
	"sync"
	"time"
)

// RequestTiming measures one model request, including failed attempts.
// Zero means the corresponding boundary was not observed. Durations include
// network/server time; they are not measurements of model compute alone.
type RequestTiming struct {
	Prepare    time.Duration `json:"prepare_ns"`
	Dispatch   time.Duration `json:"dispatch_ns"`
	Connection time.Duration `json:"connection_ns"`
	FirstToken time.Duration `json:"first_token_ns"`
	Total      time.Duration `json:"total_ns"`
}

func (RequestTiming) isEvent() {}

// ReasoningTime completes an observed reasoning segment, not server-side
// thinking that happened before the stream began.
type ReasoningTime struct {
	ID       string        `json:"id"`
	Duration time.Duration `json:"duration_ns"`
}

func (ReasoningTime) isEvent() {}

// HTTP trace hooks may run on transport goroutines. Only the first successful
// write is recorded; retries remain included in total/first-content latency.
func traceRequest(ctx context.Context, start time.Time) (context.Context, func() (time.Duration, time.Duration)) {
	var mu sync.Mutex
	var dispatch, connection time.Duration
	var connecting time.Time
	trace := &httptrace.ClientTrace{
		GetConn: func(string) {
			mu.Lock()
			connecting = time.Now()
			mu.Unlock()
		},
		GotConn: func(httptrace.GotConnInfo) {
			mu.Lock()
			if !connecting.IsZero() {
				connection += time.Since(connecting)
			}
			mu.Unlock()
		},
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			mu.Lock()
			if info.Err == nil && dispatch == 0 {
				dispatch = time.Since(start)
			}
			mu.Unlock()
		},
	}
	return httptrace.WithClientTrace(ctx, trace), func() (time.Duration, time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		return dispatch, connection
	}
}
