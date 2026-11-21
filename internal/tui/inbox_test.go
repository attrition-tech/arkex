package tui

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/fantasy"
	"github.com/attrition-tech/arkex/internal/session"
)

// Hold the first real HTTP response so input is admitted while inference owns
// history. Only the test goroutine runs Update, just like Bubble Tea.
func holdInboxResponse(t *testing.T, lm *fakeLM) (<-chan struct{}, func()) {
	t.Helper()
	started, release := make(chan struct{}), make(chan struct{})
	lm.beforeResponse = func(ctx context.Context, n int) {
		if n == 1 {
			close(started)
			select {
			case <-release:
			case <-ctx.Done():
			}
		}
	}
	released := false
	finish := func() {
		if !released {
			released = true
			close(release)
		}
	}
	t.Cleanup(finish)
	return started, finish
}

func waitInbox[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the input flow")
		var zero T
		return zero
	}
}

func inboxRun(h *harness, cmd tea.Cmd) <-chan []tea.Msg {
	done := make(chan []tea.Msg, 1)
	go func() { done <- runCmd(cmd) }()
	return done
}

func finishInboxRun(t *testing.T, h *harness, done <-chan []tea.Msg) {
	t.Helper()
	for _, msg := range waitInbox(t, done) {
		h.drive(func() tea.Msg { return msg })
	}
}

func queueKey() tea.KeyPressMsg { return tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModAlt} }

func TestSteeringAndQueuePreserveTurnsAndAttachments(t *testing.T) {
	for _, withTool := range []bool{false, true} {
		t.Run(map[bool]string{false: "final response", true: "tool boundary"}[withTool], func(t *testing.T) {
			first := streamText("Original answer", 20)
			if withTool {
				first = streamToolCall("read-once", "source.txt")
			}
			lm := &fakeLM{script: []string{first, streamText("Steered answer", 30), streamText("First queued answer", 40), streamText("Second queued answer", 50)}}
			h := fakeAgentModel(t, lm, 100000)
			started, release := holdInboxResponse(t, lm)
			path := filepath.Join(h.o.Cwd, "source.txt")
			if err := os.WriteFile(path, []byte("source-snapshot-37"), 0600); err != nil {
				t.Fatal(err)
			}
			done := inboxRun(h, h.submit("Original task", nil))
			waitInbox(t, started)
			h.setInput("Later one")
			h.Update(key("shift+enter"))
			h.Update(tea.PasteMsg{Content: "second line @source.txt"})
			if h.input.Value() != "Later one\nsecond line @source.txt" {
				t.Fatal("Shift+Enter did not remain a newline")
			}
			h.attachments = []attachment{{name: "image.png", mediaType: "image/png", data: []byte{1, 3, 5, 7}}}
			_, cmd := h.Update(queueKey())
			h.Update(tea.PasteMsg{Content: "New draft while saving"})
			h.drive(cmd)
			if h.input.Value() != "New draft while saving" {
				t.Fatal("typing during admission was lost or appended to the sent message")
			}
			for _, text := range []string{"Steer alpha", "Steer beta"} {
				h.setInput(text)
				_, cmd = h.Update(key("enter"))
				h.drive(cmd)
			}
			h.setInput("Later two")
			// Keep forwarded Cmd+Enter working for existing terminal overrides.
			_, cmd = h.Update(tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModSuper})
			h.drive(cmd)
			saved, err := session.Load(h.o.Cwd, h.conv.ID)
			if err != nil || len(saved.Pending) != 4 || len(saved.Messages) != 1 || !saved.RunPending {
				t.Fatalf("admission was not durable and separate from history: %+v %v", saved, err)
			}
			if !strings.Contains(h.statusBar(), "Steering pending: 2") || !strings.Contains(h.statusBar(), "Queued: 2") {
				t.Fatal("pending status is not visible", h.statusBar())
			}
			// The queued @file must use admission-time contents, even if the
			// file changes before the current tool and subsequent queue run.
			if err := os.WriteFile(path, []byte("changed-on-disk"), 0600); err != nil {
				t.Fatal(err)
			}
			release()
			finishInboxRun(t, h, done)
			if h.running || len(lm.requests) != 4 {
				t.Fatalf("running=%v requests=%d", h.running, len(lm.requests))
			}
			for n, request := range lm.requests {
				body, _ := json.Marshal(request["messages"])
				wire := string(body)
				if (strings.Contains(wire, "Steer alpha") != (n >= 1)) ||
					(strings.Contains(wire, "Later one") != (n >= 2)) ||
					(strings.Contains(wire, "Later two") != (n >= 3)) {
					t.Fatalf("wrong delivery boundary at request %d: %s", n, wire)
				}
				if n == 1 && strings.Index(wire, "Steer alpha") > strings.Index(wire, "Steer beta") {
					t.Fatal("steering order changed")
				}
				if n == 2 && (!strings.Contains(wire, "source-snapshot-37") || !strings.Contains(wire, "data:image/png;base64,AQMFBw==")) {
					t.Fatal("queued file or image snapshot lost", wire)
				}
			}
			saved, err = session.Load(h.o.Cwd, h.conv.ID)
			if err != nil || len(saved.Pending) != 0 || saved.RunPending || len(saved.Prompts) != 5 {
				t.Fatalf("delivery/checkpoints not committed: %+v %v", saved, err)
			}
			if !strings.Contains(transcript(h.model), "Steering applied to context") {
				t.Fatal("applied steering receipt missing")
			}
			if withTool {
				results := 0
				for _, msg := range saved.Messages {
					if msg.Role == fantasy.MessageRoleTool {
						results++
					}
				}
				if results != 1 {
					t.Fatalf("tool output lost or replayed: %d", results)
				}
			}
		})
	}
}

func TestPendingSurvivesStopPauseAndRestart(t *testing.T) {
	for _, stop := range []bool{true, false} {
		t.Run(map[bool]string{true: "stop", false: "safety pause"}[stop], func(t *testing.T) {
			lm := &fakeLM{script: []string{streamToolCall("once", "a.txt"), streamText("Resumed queue", 20)}}
			started, release := holdInboxResponse(t, lm)
			h := fakeAgentModel(t, lm, 100000)
			h.sess.Agent.MaxSteps = 1
			done := inboxRun(h, h.submit("First task", nil))
			waitInbox(t, started)
			h.setInput("Preserve this steer")
			_, cmd := h.Update(key("enter"))
			h.drive(cmd)
			if stop {
				h.cancelRun()
			}
			release()
			finishInboxRun(t, h, done)
			saved, err := session.Load(h.o.Cwd, h.conv.ID)
			if err != nil || !saved.QueuePaused || len(saved.Pending) != 1 || saved.Pending[0].Steer || len(lm.requests) != 1 {
				t.Fatalf("pending work ran after stopping: %+v %v", saved, err)
			}
			h.loadConversation(saved)
			h.drive(h.startQueued(false))
			if h.running || len(lm.requests) != 1 || !strings.Contains(h.statusBar(), "Paused pending: 1") {
				t.Fatal("restart silently dispatched work")
			}
			shown := strings.Join(strings.Fields(transcript(h.model)), " ")
			if !strings.Contains(shown, "pending messages restored paused") || !strings.Contains(shown, "Previous work may be incomplete") || !strings.Contains(shown, "external effects are not replay-safe") {
				t.Fatal("restart recovery instructions must remain visible in the transcript", shown)
			}
			h.drive(h.startQueued(true))
			if len(lm.requests) != 2 || len(h.conv.Pending) != 0 || h.conv.RunPending {
				t.Fatal("explicit resume failed")
			}
		})
	}
}

func TestLateSteerAndSaveConflict(t *testing.T) {
	lm := &fakeLM{script: []string{streamText("Finished", 20), streamText("Follow-up", 20)}}
	h := fakeAgentModel(t, lm, 100000)
	// Finish the worker, but withhold its UI completion event. This is the
	// other side of the finalization/admission race tested above.
	finished := runCmd(h.submit("First", nil))
	h.setInput("Late steer")
	_, cmd := h.Update(key("enter"))
	h.drive(cmd)
	if len(h.conv.Pending) != 1 || h.conv.Pending[0].Steer {
		t.Fatal("late input silently steered a completed run")
	}
	h.command("/queue")
	for _, msg := range finished {
		h.drive(func() tea.Msg { return msg })
	}
	if len(lm.requests) != 2 || len(h.conv.Pending) != 0 {
		t.Fatal("late input disappeared instead of starting a new turn")
	}
	if !strings.Contains(h.View().Content, "No pending messages") || strings.Contains(h.View().Content, "1. Late steer") {
		t.Fatal("open pending list did not update after delivery")
	}
	h.closePalette()
	// Preserve an existing pending entry and the new draft on a CAS conflict.
	h.conv.QueuePaused = true
	h.setInput("Saved pending")
	_, cmd = h.Update(queueKey())
	h.drive(cmd)
	h.setInput("Must stay in composer")
	_, cmd = h.Update(queueKey())
	external, err := session.Load(h.o.Cwd, h.conv.ID)
	if err != nil {
		t.Fatal(err)
	}
	external.Title = "Changed by another process"
	if err = external.Save(); err != nil {
		t.Fatal(err)
	}
	h.drive(cmd)
	if h.input.Value() != "Must stay in composer" || !strings.Contains(transcript(h.model), "Message not saved:") {
		t.Fatal("failed admission consumed the draft")
	}
	saved, err := session.Load(h.o.Cwd, h.conv.ID)
	if err != nil || saved.Title != external.Title || len(saved.Pending) != 1 || saved.Pending[0].Text != "Saved pending" {
		t.Fatal("conflict overwrote persisted work", err)
	}
}

func TestPendingControlsAndModelBinding(t *testing.T) {
	lm := &fakeLM{}
	h := fakeAgentModel(t, lm, 100000)
	s := session.New(h.o.Cwd)
	s.Model = "fake/m"
	s.Pending = []session.PendingInput{
		{ID: "restore", Text: "Edit this", Model: "fake/m", Message: fantasy.NewUserMessage("Edit this", fantasy.FilePart{Filename: "image.png", MediaType: "image/png", Data: []byte{3, 7}})},
		{ID: "delete", Text: "Delete this", Model: "fake/m", Message: fantasy.NewUserMessage("Delete this")},
	}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	h.loadConversation(s)
	h.sess.Name = "other/m"
	h.drive(h.startQueued(true))
	if len(lm.requests) != 0 || len(h.conv.Pending) != 2 || !strings.Contains(transcript(h.model), "saved for fake/m") {
		t.Fatal("queue changed provider without permission")
	}
	h.sess.Name = "fake/m"
	choose := func(title string) {
		t.Helper()
		for i, item := range h.pal.view {
			if item.title == title {
				h.pal.sel = i
				_, cmd := h.Update(key("enter"))
				h.drive(cmd)
				return
			}
		}
		t.Fatal("missing queue action", title)
	}
	h.command("/queue")
	choose("1. Edit this")
	choose("Restore to composer")
	if h.input.Value() != "Edit this" || len(h.attachments) != 1 || string(h.attachments[0].data) != string([]byte{3, 7}) {
		t.Fatal("restoring lost the editable text or snapshotted image")
	}
	h.command("/queue")
	choose("1. Delete this")
	choose("Delete pending message")
	saved, err := session.Load(h.o.Cwd, s.ID)
	if err != nil || len(saved.Pending) != 0 || len(saved.Messages) != 0 || len(lm.requests) != 0 || h.input.Value() != "Edit this" {
		t.Fatal("queue controls changed history, sent a request, or lost the current draft", err)
	}
}

func TestTypingDuringAdmissionCannotAnswerNewApproval(t *testing.T) {
	lm := &fakeLM{script: []string{streamText("Original", 20), streamText("Steered", 20)}}
	started, release := holdInboxResponse(t, lm)
	h := fakeAgentModel(t, lm, 100000)
	done := inboxRun(h, h.submit("First", nil))
	waitInbox(t, started)
	h.setInput("First steer")
	_, cmd := h.Update(key("enter"))
	reply := make(chan bool, 1)
	h.Update(approvalMsg{retry: true, reply: reply})
	h.Update(tea.PasteMsg{Content: "Second steer"})
	h.Update(key("enter"))
	h.drive(cmd)
	if len(reply) != 0 || h.pending == nil {
		t.Fatal("deferred composer Enter answered a newly arriving approval")
	}
	saved, err := session.Load(h.o.Cwd, h.conv.ID)
	if err != nil || len(saved.Pending) != 2 || saved.Pending[1].Text != "Second steer" {
		t.Fatal("typing was lost while displaying the approval", err)
	}
	h.answerPending(false)
	h.cancelRun()
	release()
	finishInboxRun(t, h, done)
}
