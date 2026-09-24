package session

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// Listings retain metadata and a count, not decoded conversations. Reuse a
// scratch message so peak memory scales with the largest message, not history.
// No new on-disk metadata or migration is needed for existing sessions.
func readSummary(r io.Reader) (s Summary, state string, err error) {
	d := json.NewDecoder(r)
	tok, err := d.Token()
	if err != nil || tok != json.Delim('{') {
		return s, state, fmt.Errorf("invalid session object: %v", err)
	}
	var scratch json.RawMessage
	for d.More() {
		tok, err = d.Token()
		if err != nil {
			return s, state, err
		}
		key, ok := tok.(string)
		if !ok {
			return s, state, fmt.Errorf("invalid session key")
		}
		switch strings.ToLower(key) {
		case "id":
			err = d.Decode(&s.ID)
		case "title":
			err = d.Decode(&s.Title)
		case "model":
			err = d.Decode(&s.Model)
		case "state":
			err = d.Decode(&state)
		case "updated":
			err = d.Decode(&s.Updated)
		case "messages":
			s.Messages = 0
			tok, err = d.Token()
			if err == nil && tok != nil {
				if tok != json.Delim('[') {
					return s, state, fmt.Errorf("invalid messages array")
				}
				for d.More() {
					if err = d.Decode(&scratch); err != nil {
						return s, state, err
					}
					s.Messages++
				}
				_, err = d.Token() // array end
			}
		default:
			err = d.Decode(&scratch)
		}
		if err != nil {
			return s, state, err
		}
	}
	if _, err = d.Token(); err != nil {
		return s, state, err
	}
	if err = d.Decode(&scratch); err != io.EOF {
		return s, state, fmt.Errorf("unexpected data after session: %v", err)
	}
	return s, state, nil
}
