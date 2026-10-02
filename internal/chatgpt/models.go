package chatgpt

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Model is one entry of the account-specific model catalog.
type Model struct {
	ID              string   // slug used in requests
	Name            string   // display name
	ContextWindow   int      // tokens; 0 when the catalog does not say
	Reasoning       string   // default reasoning level, "" when unknown
	ReasoningLevels []string // supported reasoning levels, in catalog order
}

// ListModels fetches the catalog the signed-in account can use and returns
// the models the backend marks as visible, in catalog order.
func ListModels(ctx context.Context, client *http.Client, ep Endpoints, tokens Tokens) ([]Model, error) {
	client = oauthClient(client)
	if err := tokens.ValidateAccess(); err != nil {
		return nil, err
	}
	url := ep.base() + "/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tokens.AccessToken)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		requestID := strings.TrimSpace(resp.Header.Get("x-request-id"))
		if requestID != "" {
			return nil, fmt.Errorf("model catalog: %s (request %s)", resp.Status, requestID)
		}
		return nil, fmt.Errorf("model catalog: %s", resp.Status)
	}
	var raw struct {
		Models []struct {
			Slug             string `json:"slug"`
			DisplayName      string `json:"display_name"`
			Visibility       string `json:"visibility"`
			ContextWindow    int    `json:"context_window"`
			DefaultReasoning string `json:"default_reasoning_level"`
			Levels           []struct {
				Effort string `json:"effort"`
			} `json:"supported_reasoning_levels"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("model catalog: %w", err)
	}
	var out []Model
	for _, m := range raw.Models {
		if m.Slug == "" || m.Visibility != "list" {
			continue
		}
		mm := Model{ID: m.Slug, Name: m.DisplayName, ContextWindow: m.ContextWindow, Reasoning: m.DefaultReasoning}
		if mm.Reasoning == "none" {
			mm.Reasoning = "off"
		}
		for _, l := range m.Levels {
			if l.Effort == "none" {
				l.Effort = "off"
			}
			if l.Effort != "" {
				mm.ReasoningLevels = append(mm.ReasoningLevels, l.Effort)
			}
		}
		out = append(out, mm)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("model catalog: no models available for this account")
	}
	return out, nil
}
