package chatgpt

import (
	"bytes"
	"encoding/json"
	"io"

	"github.com/openai/openai-go/v3/packages/ssestream"
)

// planStream keeps response.incomplete distinct from successful inference.
// Fantasy v0.43 can map incomplete events to Stop or ToolCalls, losing that
// distinction. Translate only that event to a failure before it reaches Fantasy.
// The SDK decoder handles SSE framing, including multi-line data fields.
type planStream struct {
	ssestream.Decoder
	pending []byte
}

func (s *planStream) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if len(s.pending) == 0 {
		if !s.Next() {
			if err := s.Err(); err != nil {
				return 0, err
			}
			return 0, io.EOF
		}
		event := s.Event()
		var envelope struct {
			Type     string `json:"type"`
			Response struct {
				Details struct {
					Reason string `json:"reason"`
				} `json:"incomplete_details"`
			} `json:"response"`
		}
		if json.Unmarshal(event.Data, &envelope) == nil && envelope.Type == "response.incomplete" {
			message := "ChatGPT response was incomplete"
			if reason := envelope.Response.Details.Reason; reason != "" {
				message += ": " + reason
			}
			event.Type = "response.failed"
			event.Data, _ = json.Marshal(map[string]any{
				"type": "response.failed",
				"response": map[string]any{"error": map[string]string{
					"code": "incomplete_response", "message": message,
				}},
			})
		}
		s.pending = []byte("event: " + event.Type + "\ndata: ")
		s.pending = append(s.pending, bytes.ReplaceAll(event.Data, []byte("\n"), []byte("\ndata: "))...)
		s.pending = append(s.pending, '\n', '\n')
	}
	n := copy(p, s.pending)
	s.pending = s.pending[n:]
	return n, nil
}
