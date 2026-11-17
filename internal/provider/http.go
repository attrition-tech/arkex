package provider

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
)

// inferenceTransport bounds opening a request and waiting for body bytes,
// without imposing a total deadline on a healthy, long-running stream.
type inferenceTransport struct {
	base         http.RoundTripper
	header, idle time.Duration
}

func (t *inferenceTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx, cancel := context.WithCancelCause(req.Context())
	timer := time.AfterFunc(t.header, func() {
		cancel(fmt.Errorf("LLM response headers timed out after %s: %w", t.header, context.DeadlineExceeded))
	})
	resp, err := t.base.RoundTrip(req.Clone(ctx))
	timer.Stop()
	if ctx.Err() != nil {
		if resp != nil {
			_ = resp.Body.Close()
		}
		err = context.Cause(ctx)
	}
	if err != nil {
		cancel(nil)
		return nil, err
	}
	resp.Body = &idleBody{ReadCloser: resp.Body, ctx: ctx, cancel: cancel, timeout: t.idle}
	return resp, nil
}

type idleBody struct {
	io.ReadCloser
	ctx     context.Context
	cancel  context.CancelCauseFunc
	timeout time.Duration
}

func (b *idleBody) Read(p []byte) (int, error) {
	// Start a fresh idle allowance for each network read. Bytes such as SSE
	// heartbeats count as activity even when they produce no visible text.
	timer := time.AfterFunc(b.timeout, func() {
		b.cancel(fmt.Errorf("LLM stream idle for %s: %w", b.timeout, context.DeadlineExceeded))
	})
	n, err := b.ReadCloser.Read(p)
	timer.Stop()
	if b.ctx.Err() != nil {
		err = context.Cause(b.ctx)
	}
	return n, err
}

func (b *idleBody) Close() error {
	b.cancel(nil)
	return b.ReadCloser.Close()
}
