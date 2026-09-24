package session

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestReadSummaryCompatibility(t *testing.T) {
	for _, tc := range []struct {
		text  string
		n     int
		state string
	}{
		{`{"id":"a","messages":[{"text":"a ] } \\"},null,42],"state":"trash"}`, 3, "trash"},
		{`{"messages":null,"ID":"a","unknown":{"nested":[1,2]}}`, 0, ""},
		{`{"id":"a","messages":[1,2],"messages":[3]}`, 1, ""},
		{`{"id":"a"}`, 0, ""},
	} {
		s, state, err := readSummary(strings.NewReader(tc.text))
		if err != nil || s.ID != "a" || s.Messages != tc.n || state != tc.state {
			t.Fatalf("%s: %+v %q %v", tc.text, s, state, err)
		}
	}
	for _, text := range []string{`{`, `{"id":"a","messages":[{`, `{"id":"a","messages":{}}`, `{"id":"a"} {}`, `{"id":"a","updated":"invalid"}`, `{"id":"a","title":123}`} {
		if _, _, err := readSummary(strings.NewReader(text)); err == nil {
			t.Fatalf("accepted corrupt file: %s", text)
		}
	}
}

func BenchmarkSessionSummary(b *testing.B) {
	text := `{"id":"a","messages":[` + strings.Repeat(`{"text":"`+strings.Repeat("x", 4096)+`"},`, 1999) + `null]}`
	for _, stream := range []bool{false, true} {
		name := "previous"
		if stream {
			name = "stream"
		}
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if stream {
					s, _, err := readSummary(strings.NewReader(text))
					if err != nil || s.Messages != 2000 {
						b.Fatal(err)
					}
				} else {
					var s struct {
						ID       string
						Messages []json.RawMessage
					}
					if err := json.Unmarshal([]byte(text), &s); err != nil || len(s.Messages) != 2000 {
						b.Fatal(err)
					}
				}
			}
		})
	}
}
