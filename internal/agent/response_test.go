package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"charm.land/fantasy"
	"github.com/attrition-tech/arkex/internal/provider"
	"github.com/attrition-tech/arkex/internal/tools"
)

type responseModel struct {
	fantasy.LanguageModel
	parts []fantasy.StreamPart
}

func (m responseModel) Stream(context.Context, fantasy.Call) (fantasy.StreamResponse, error) {
	return func(yield func(fantasy.StreamPart) bool) {
		for _, part := range m.parts {
			if !yield(part) {
				return
			}
		}
	}, nil
}

func TestMalformedToolText(t *testing.T) {
	tag := `<｜DSML｜parameter name="command" string="true">pwd`
	for _, tc := range []struct {
		text string
		want bool
	}{
		{"Running it now:\n" + tag, true},
		{`< | DSML | parameter name="timeout_ms" string="false">600000`, true},
		{`<|DSML|invoke name="bash">`, true},
		{"The model returned DSML", false},
		{"An example is " + tag, false},
		{"`" + tag + "`", false},
		{"> " + tag, false},
		{"    " + tag, false},
		{"\t" + tag, false},
		{"```xml\n" + tag + "\n```", false},
		{"~~~~xml\n" + tag + "\n~~~\n" + tag + "\n~~~~", false},
		{"```xml\nexample\n```\n" + tag, true},
	} {
		if got := malformedToolText(tc.text); got != tc.want {
			t.Errorf("%q: got %v want %v", tc.text, got, tc.want)
		}
	}
}

func TestResponseValidation(t *testing.T) {
	text := fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, Delta: "Hello"}
	finish := fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop}
	malformed := fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, Delta: `<|DSML|parameter name="command" string="true">pwd`}
	for _, tc := range []struct {
		name  string
		parts []fantasy.StreamPart
		want  error
	}{
		{"empty missing finish", nil, ErrIncompleteResponse},
		{"text missing finish", []fantasy.StreamPart{text}, ErrIncompleteResponse},
		{"adapter unexpected EOF", []fantasy.StreamPart{text, {Type: fantasy.StreamPartTypeError, Error: fmt.Errorf("stream: %w", io.ErrUnexpectedEOF)}}, ErrIncompleteResponse},
		{"valid completion", []fantasy.StreamPart{text, finish}, nil},
		{"malformed completion", []fantasy.StreamPart{malformed, finish}, ErrMalformedToolResponse},
		{"reasoning is not a call", []fantasy.StreamPart{{Type: fantasy.StreamPartTypeReasoningDelta, Delta: malformed.Delta}, finish}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := &Agent{Model: &provider.Model{LM: responseModel{parts: tc.parts}}}
			_, err := a.stream(t.Context(), func(Event) {})
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v want %v", err, tc.want)
			}
			if tc.want == nil {
				return
			}
			waits := 0
			if _, err = a.streamWithRetry(t.Context(), func(e Event) {
				if _, ok := e.(RetryWait); ok {
					waits++
				}
			}); !errors.Is(err, tc.want) || waits != 2 {
				t.Fatal(err)
			}
			waits = 0
			prompts := 0
			a.Retry = func(_ context.Context, err error, partial bool) bool {
				prompts++
				if waits != prompts*2 {
					t.Fatalf("prompt %d after %d retries", prompts, waits)
				}
				if !errors.Is(err, tc.want) {
					t.Fatal(err)
				}
				if prompts == 1 {
					return true
				}
				return false
			}
			_, err = a.streamWithRetry(t.Context(), func(e Event) {
				if _, ok := e.(RetryWait); ok {
					waits++
				}
			})
			if !errors.Is(err, context.Canceled) || prompts != 2 {
				t.Fatalf("err=%v prompts=%d", err, prompts)
			}
		})
	}
}

func TestMalformedRetryPreservesWork(t *testing.T) {
	dir := t.TempDir()
	fs := &fakeServer{script: []func(http.ResponseWriter){
		func(w http.ResponseWriter) {
			sse(w, delta(`{"tool_calls":[{"index":0,"id":"saved","type":"function","function":{"name":"write","arguments":"{\"path\":\"saved.txt\",\"content\":\"saved\"}"}}]}`, ""), delta(`{}`, "tool_calls"))
		},
		func(w http.ResponseWriter) {
			// Split the raw tag across chunks to exercise assembled text.
			sse(w, delta(`{"content":"<|DS"}`, ""), delta(`{"content":"ML|parameter name=\"command\" string=\"true\">touch should-not-exist"}`, ""), delta(`{}`, "stop"))
		},
		func(w http.ResponseWriter) {
			sse(w, delta(`{"content":"Recovered"}`, ""), delta(`{}`, "stop"))
		},
	}}
	srv := httptest.NewServer(http.HandlerFunc(fs.handler))
	defer srv.Close()
	a := &Agent{Model: timingModel(t, srv.URL), Tools: tools.Default(dir), Policy: AllowAll{}}
	prompts, results := 0, 0
	a.Retry = func(_ context.Context, err error, partial bool) bool {
		prompts++
		if !errors.Is(err, ErrMalformedToolResponse) || !partial {
			t.Fatalf("err=%v partial=%v", err, partial)
		}
		return prompts == 1
	}
	err := a.Run(t.Context(), "write a file", func(e Event) {
		if _, ok := e.(ToolResult); ok {
			results++
		}
	})
	if err != nil || prompts != 0 || results != 1 || len(fs.requests) != 3 {
		t.Fatalf("err=%v prompts=%d tools=%d requests=%d", err, prompts, results, len(fs.requests))
	}
	if !reflect.DeepEqual(fs.requests[1]["messages"], fs.requests[2]["messages"]) {
		t.Fatal("failed response contaminated replay")
	}
	if b, err := os.ReadFile(filepath.Join(dir, "saved.txt")); err != nil || string(b) != "saved" {
		t.Fatalf("completed write lost: %q %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "should-not-exist")); !os.IsNotExist(err) {
		t.Fatal("DSML prose executed")
	}
}
