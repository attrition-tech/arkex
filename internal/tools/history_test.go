package tools_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"charm.land/fantasy"
	"github.com/attrition-tech/arkex/internal/session"
	"github.com/attrition-tech/arkex/internal/tools"
)

func TestHistoryRecoversLongTextAndAttachmentsAfterRestartAndFork(t *testing.T) {
	t.Setenv("ARKEX_HOME", t.TempDir())
	s := session.New(t.TempDir())
	first := strings.Repeat("🙂 العربية\n", 600) + "AZ-719: publication is NOT authorized."
	file := fantasy.FilePart{Filename: "evidence.png", MediaType: "image/png", Data: []byte{0, 17, 255}}
	messages := []fantasy.Message{fantasy.NewUserMessage(first, file)}
	if _, err := s.SaveContext(t.Context(), messages); err != nil {
		t.Fatal(err)
	}
	s.Checkpoint(messages, "second request", fantasy.NewUserMessage("second request"), "m")
	messages = append(messages, fantasy.NewUserMessage("FUTURE_PARENT_ONLY"))
	if _, err := s.SaveContext(t.Context(), messages); err != nil {
		t.Fatal(err)
	}
	loaded, err := session.Load(s.Cwd, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	branch, err := loaded.Fork(1)
	if err != nil {
		t.Fatal(err)
	}
	if err := branch.Save(); err != nil {
		t.Fatal(err)
	}
	tool := &tools.History{Load: branch.HistoryReader()}
	search, err := tool.Run(t.Context(), json.RawMessage(`{"query":"AZ-719"}`))
	if err != nil || !strings.Contains(search.Output, "AZ-719: publication is NOT authorized.") || !strings.Contains(search.Output, "message=1") {
		t.Fatal("long message recovery failed", err, search.Output)
	}
	got, err := tool.Run(t.Context(), json.RawMessage(fmt.Sprintf(`{"message":1,"offset":%d}`, len(first)-len("AZ-719: publication is NOT authorized."))))
	if err != nil || !strings.Contains(got.Output, "AZ-719: publication is NOT authorized.") {
		t.Fatal("search position is not readable", err, got.Output)
	}
	page, err := tool.Run(t.Context(), json.RawMessage(`{"message":1,"offset":1,"limit":512}`))
	if err != nil || !utf8.ValidString(page.Output) || len(page.Output) > 512 || !strings.Contains(page.Output, "Continue with message=1 offset=") {
		t.Fatal("pagination broke UTF-8 or lost continuation", err, page.Output)
	}
	for _, input := range []string{`{"query":"FUTURE_PARENT_ONLY"}`, `{"message":2}`} {
		got, err := tool.Run(t.Context(), json.RawMessage(input))
		if err != nil || strings.Contains(got.Output, "FUTURE_PARENT_ONLY") {
			t.Fatal("branch exposed later parent history", err, got.Output)
		}
	}
	media, err := tool.Run(t.Context(), json.RawMessage(`{"message":1,"attachment":2}`))
	if err != nil || media.Media == nil || media.Media.Data != base64.StdEncoding.EncodeToString(file.Data) {
		t.Fatal("original media was not recoverable", err)
	}
	if _, err := tool.Run(t.Context(), json.RawMessage(`{"path":"/another/session.json"}`)); err == nil {
		t.Fatal("history must not accept an arbitrary session path")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := tool.Run(ctx, json.RawMessage(`{}`)); err == nil {
		t.Fatal("canceled history read succeeded")
	}
}
