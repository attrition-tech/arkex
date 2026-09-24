package session

import (
	"fmt"
	"reflect"
	"testing"

	"charm.land/fantasy"
)

func TestCheckpointHistorySharesPrefixesAndForksIndependently(t *testing.T) {
	t.Setenv("ARKEX_HOME", t.TempDir())
	s := New(t.TempDir())
	var messages []fantasy.Message
	for i := 0; i < 30; i++ {
		text := fmt.Sprintf("prompt %d", i)
		msg := fantasy.NewUserMessage(text)
		s.Checkpoint(messages, text, msg, "fake/model")
		messages = append(messages, msg, fantasy.Message{Role: fantasy.MessageRoleAssistant,
			Content: []fantasy.MessagePart{fantasy.TextPart{Text: fmt.Sprintf("answer %d", i)}}})
		s.Update(messages, "fake/model", "auto", 0, 0)
	}
	if len(s.History) != 60 {
		t.Fatalf("duplicated prefixes: %d", len(s.History))
	}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	again, err := Load(s.Cwd, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	fork, err := again.Fork(12)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fork.Messages, messages[:22]) || fork.ParentID != s.ID {
		t.Fatal("wrong checkpoint boundary")
	}
	for index, count := range map[int]int{1: 0, 30: 58} {
		branch, err := again.Fork(index)
		if err != nil || len(branch.Messages) != count {
			t.Fatalf("boundary %d: messages=%v error=%v", index, branch, err)
		}
	}
	fork.Checkpoint(fork.Messages, "replacement", fantasy.NewUserMessage("replacement"), "fake/model")
	fork.Messages = append(fork.Messages, fantasy.NewUserMessage("replacement"))
	fork.Update(fork.Messages, "fake/model", "auto", 0, 0)
	if UserText(again.History[22]) != "prompt 11" || again.Prompts[11].Text != "prompt 11" {
		t.Fatal("fork overwrote original backing arrays")
	}
	for _, bad := range []int{0, -1, 31} {
		if _, err := again.Fork(bad); err == nil {
			t.Fatal("invalid checkpoint accepted")
		}
	}
	again.Prompts[0].End = len(again.History) + 1
	if _, err := again.Fork(1); err == nil {
		t.Fatal("corrupt checkpoint accepted")
	}
}
