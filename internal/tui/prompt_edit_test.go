package tui

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/fantasy"
	"github.com/charmbracelet/x/ansi"
	"github.com/dantearo/arkex/internal/agent"
	"github.com/dantearo/arkex/internal/session"
)

func sendEditTest(h *harness, text string) {
	h.setInput(text)
	_, cmd := h.Update(key("enter"))
	h.drive(cmd)
}

func TestEditResendActualRequestAfterCompactionAndResume(t *testing.T) {
	lm := &fakeLM{script: []string{streamText("prior-answer", 20), streamText("discard-answer", 20),
		streamText("later-answer", 20), jsonReply("summary includes discard-target and later-prompt"), streamText("revised-answer", 20)}}
	h := fakeAgentModel(t, lm, 100000)
	sendEditTest(h, "prior-prompt")
	image := []byte{1, 3, 5, 7}
	h.attachments = []attachment{{name: "original.png", mediaType: "image/png", data: image}}
	sendEditTest(h, "discard-target")
	sendEditTest(h, "later-prompt")
	originalID := h.conv.ID
	h.drive(h.compact())
	s, err := session.Load(h.o.Cwd, originalID)
	if err != nil {
		t.Fatal(err)
	}
	h.loadConversation(s)
	if len(promptEditItems(h.model)) != 3 {
		t.Fatal("compacted prompts lost from edit menu")
	}
	h.beginPromptEdit(2)
	if h.input.Value() != "discard-target" || len(h.attachments) != 1 || string(h.attachments[0].data) != string(image) {
		t.Fatal("original prompt/image not restored after resume")
	}
	path := filepath.Join(h.o.Cwd, "already-written.txt")
	if err := os.WriteFile(path, []byte("keep file changes"), 0600); err != nil {
		t.Fatal(err)
	}
	h.setInput("replacement-prompt")
	_, cmd := h.Update(key("enter"))
	h.drive(cmd)
	if h.conv.ID != originalID || len(lm.requests) != 4 || h.pal == nil {
		t.Fatal("sent before confirmation")
	}
	h.pal.sel = 1
	h.drive(h.pal.activate(h.model))
	if h.conv.ID == originalID || h.conv.ParentID != originalID {
		t.Fatal("did not fork")
	}
	msgs := lm.requests[4]["messages"]
	wire, _ := json.Marshal(msgs)
	for _, want := range []string{"prior-prompt", "prior-answer", "replacement-prompt", "data:image/png;base64,AQMFBw=="} {
		if !strings.Contains(string(wire), want) {
			t.Fatalf("missing %q in outgoing request: %s", want, wire)
		}
	}
	for _, unwanted := range []string{"discard-target", "discard-answer", "later-prompt", "later-answer", "summary includes"} {
		if strings.Contains(string(wire), unwanted) {
			t.Fatalf("old branch leaked %q: %s", unwanted, wire)
		}
	}
	original, err := session.Load(h.o.Cwd, originalID)
	if err != nil || len(original.Prompts) != 3 {
		t.Fatalf("original lost: %v", err)
	}
	revised, err := session.Load(h.o.Cwd, h.conv.ID)
	if err != nil || len(revised.Prompts) != 2 || revised.Prompts[1].Text != "replacement-prompt" {
		t.Fatalf("branch not persisted: %v", err)
	}
	if data, _ := os.ReadFile(path); string(data) != "keep file changes" {
		t.Fatal("editing undid external changes")
	}
}

func TestEditCancelAndKeyboardMouseEntry(t *testing.T) {
	h := fakeAgentModel(t, &fakeLM{script: []string{streamText("answer", 20)}}, 100000)
	sendEditTest(h, "original prompt")
	h.setInput("")
	typeKeys(h.model, "tab", "enter")
	if h.editingPrompt == nil {
		t.Fatal("tab/enter did not enter editor")
	}
	typeKeys(h.model, "esc")
	h.setInput("unsent draft")
	h.attachments = []attachment{{name: "draft.png", mediaType: "image/png", data: []byte{8}}}
	h.beginPromptEdit(1)
	h.setInput("changed but not sent")
	for _, width := range []int{24, 80, 120} {
		h.width = width
		h.resizeInput()
		view := ansi.Strip(h.inputView())
		if !strings.Contains(view, "Editing prompt") {
			t.Fatal("missing editor label")
		}
		for _, line := range strings.Split(view, "\n") {
			if ansi.StringWidth(line) > width {
				t.Fatalf("editor overflows at %d: %q", width, line)
			}
		}
	}
	typeKeys(h.model, "esc")
	if h.input.Value() != "unsent draft" || len(h.attachments) != 1 || h.attachments[0].name != "draft.png" || h.editingPrompt != nil {
		t.Fatal("cancel did not restore composer")
	}
	h.width = 100
	h.resizeInput()
	h.flashText = ""
	h.refresh()
	for _, s := range h.spans {
		if s.b.kind != blockUser {
			continue
		}
		y := s.top - h.vp.YOffset()
		h.Update(tea.MouseClickMsg{X: 4, Y: y, Button: tea.MouseLeft})
		h.Update(tea.MouseReleaseMsg{X: 4, Y: y, Button: tea.MouseLeft})
		if h.pal == nil || h.pal.view[0].title != "Edit and resend…" || h.pal.view[1].title != "Remove from here…" {
			t.Fatal("mouse action missing")
		}
		return
	}
	t.Fatal("no prompt span")
}

func TestEditWaitsForCancellationAndRejectsSaveConflict(t *testing.T) {
	h := fakeAgentModel(t, &fakeLM{script: []string{streamText("answer", 20)}}, 100000)
	sendEditTest(h, "original")
	h.running = true
	cancelled := false
	h.cancel = func() { cancelled = true }
	h.requestPromptEdit(1)
	h.pal.sel = 1
	h.pal.activate(h.model)
	if !cancelled || h.editingPrompt != nil || h.editAfterStop != 1 {
		t.Fatal("editing raced cancellation")
	}
	h.Update(runDoneMsg{err: context.Canceled})
	if h.editingPrompt == nil || h.editAfterStop != 0 {
		t.Fatal("edit not resumed after stop")
	}
	originalID := h.conv.ID
	other, err := session.Load(h.o.Cwd, originalID)
	if err != nil {
		t.Fatal(err)
	}
	other.Title = "changed elsewhere"
	if err := other.Save(); err != nil {
		t.Fatal(err)
	}
	h.setInput("replacement")
	h.resendPrompt()
	if h.conv.ID != originalID || h.editingPrompt == nil || h.input.Value() != "replacement" || h.running {
		t.Fatal("failed save abandoned original or draft")
	}
}

func TestCancelThenSendRetainsBothPrompts(t *testing.T) {
	lm := &fakeLM{script: []string{streamText("unfinished output", 20), streamText("next answer", 20)}}
	h := fakeAgentModel(t, lm, 100000)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := h.sess.Agent.Run(ctx, "cancelled instruction", func(e agent.Event) {
		if _, ok := e.(agent.TextDelta); ok {
			cancel()
		}
	})
	if err == nil {
		t.Fatal("expected cancellation")
	}
	sendEditTest(h, "new instruction")
	if len(lm.requests) != 2 {
		t.Fatalf("requests = %d", len(lm.requests))
	}
	wire, _ := json.Marshal(lm.requests[1]["messages"])
	if !strings.Contains(string(wire), "cancelled instruction") || !strings.Contains(string(wire), "new instruction") {
		t.Fatalf("normal cancellation changed history semantics: %s", wire)
	}
	if strings.Contains(string(wire), "unfinished output") {
		t.Fatal("partial response leaked into next request")
	}
}

func TestLegacyRepeatedPromptsMapToDistinctCheckpoints(t *testing.T) {
	h := fakeAgentModel(t, &fakeLM{}, 100000)
	s := session.New(h.o.Cwd)
	s.Messages = []fantasy.Message{fantasy.NewUserMessage("repeat"), {Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{fantasy.TextPart{Text: "answer"}}}, fantasy.NewUserMessage("repeat")}
	s.Model = "fake/m"
	h.loadConversation(s)
	var indices []int
	for _, b := range h.blocks {
		if b.kind == blockUser {
			indices = append(indices, b.prompt)
		}
	}
	if len(indices) != 2 || indices[0] != 1 || indices[1] != 2 {
		t.Fatalf("checkpoint mapping: %v", indices)
	}
	summary := agent.CompactedMessages("earlier context")
	legacy := session.New(h.o.Cwd)
	legacy.Messages = append(summary, fantasy.NewUserMessage("surviving prompt"))
	h.loadConversation(legacy)
	if len(legacy.Prompts) != 1 {
		t.Fatal("synthetic summary became editable")
	}
}

func TestRemovePromptAfterCompactionAndResume(t *testing.T) {
	lm := &fakeLM{script: []string{streamText("keep answer", 20), streamText("discard answer", 20),
		streamText("later answer", 20), jsonReply("summary containing discard prompt"), streamText("new answer", 20)}}
	h := fakeAgentModel(t, lm, 100000)
	sendEditTest(h, "keep prompt")
	sendEditTest(h, "discard prompt")
	sendEditTest(h, "later prompt")
	originalID := h.conv.ID
	h.drive(h.compact())
	s, err := session.Load(h.o.Cwd, originalID)
	if err != nil {
		t.Fatal(err)
	}
	h.loadConversation(s)
	h.setInput("unsent draft")
	h.attachments = []attachment{{name: "draft.png", mediaType: "image/png", data: []byte{8}}}
	h.confirmPromptRemove(2)
	h.pal.activate(h.model) // Cancel is the default.
	if h.conv.ID != originalID || h.input.Value() != "unsent draft" {
		t.Fatal("cancel changed conversation or draft")
	}
	h.confirmPromptRemove(2)
	h.pal.sel = 1
	h.drive(h.pal.activate(h.model))
	if h.conv.ID == originalID || h.running || len(lm.requests) != 4 {
		t.Fatal("removal must fork without a model call")
	}
	if h.input.Value() != "unsent draft" || len(h.attachments) != 1 {
		t.Fatal("removal lost draft")
	}
	branch, err := session.Load(h.o.Cwd, h.conv.ID)
	if err != nil || len(branch.Prompts) != 1 || len(branch.Messages) != 2 {
		t.Fatalf("branch not saved: %v", err)
	}
	original, err := session.Load(h.o.Cwd, originalID)
	if err != nil || len(original.Prompts) != 3 {
		t.Fatalf("original changed: %v", err)
	}
	h.loadConversation(branch)
	sendEditTest(h, "new prompt")
	wire, _ := json.Marshal(lm.requests[4]["messages"])
	for _, bad := range []string{"discard", "later", "summary containing"} {
		if strings.Contains(string(wire), bad) {
			t.Fatalf("removed context leaked: %s", wire)
		}
	}
	for _, good := range []string{"keep prompt", "keep answer", "new prompt"} {
		if !strings.Contains(string(wire), good) {
			t.Fatalf("lost prior context: %s", wire)
		}
	}
}

func TestRemoveFirstPromptStopsBeforeForkAndPersistsEmptyBranch(t *testing.T) {
	for _, compacting := range []bool{false, true} {
		h := fakeAgentModel(t, &fakeLM{script: []string{streamText("answer", 20)}}, 100000)
		sendEditTest(h, "first prompt")
		originalID := h.conv.ID
		h.focusPrompt(true)
		h.Update(tea.KeyPressMsg{Code: tea.KeyDelete})
		if h.pal == nil || h.conv.ID != originalID {
			t.Fatal("Delete must open confirmation, not remove immediately")
		}
		h.pal.activate(h.model)
		if h.conv.ID != originalID {
			t.Fatal("default keyboard action must cancel")
		}
		h.running, h.compacting = true, compacting
		cancelled := false
		h.cancel = func() { cancelled = true }
		h.confirmPromptRemove(1)
		h.pal.sel = 1
		h.pal.activate(h.model)
		if !cancelled || h.conv.ID != originalID || h.removeOnStop != 1 {
			t.Fatal("fork raced active run")
		}
		if compacting {
			h.Update(compactDoneMsg{err: context.Canceled})
		} else {
			h.Update(runDoneMsg{err: context.Canceled})
		}
		if h.running || h.paused || h.conv.ID == originalID || h.removeOnStop != 0 {
			t.Fatal("did not finish removal after stopping")
		}
		branch, err := session.Load(h.o.Cwd, h.conv.ID)
		if err != nil || len(branch.Messages) != 0 || branch.ParentID != originalID {
			t.Fatalf("empty branch not persisted: %v", err)
		}
		h.loadConversation(branch)
		if len(h.sess.Agent.Messages()) != 0 || len(h.conv.Prompts) != 0 {
			t.Fatal("empty branch resurrected removed prompt")
		}
	}
}

func TestRemovePromptSaveConflictPreservesSource(t *testing.T) {
	h := fakeAgentModel(t, &fakeLM{script: []string{streamText("answer", 20)}}, 100000)
	sendEditTest(h, "original")
	id := h.conv.ID
	other, err := session.Load(h.o.Cwd, id)
	if err != nil {
		t.Fatal(err)
	}
	other.Title = "changed elsewhere"
	if err := other.Save(); err != nil {
		t.Fatal(err)
	}
	h.removePrompt(1)
	if h.conv.ID != id || len(h.sess.Agent.Messages()) != 2 {
		t.Fatal("failed removal changed active context")
	}
	list, err := session.List(h.o.Cwd)
	if err != nil || len(list) != 1 {
		t.Fatal("created branch despite source conflict")
	}
}
