package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/fantasy"
	"github.com/charmbracelet/x/ansi"

	"github.com/attrition-tech/arkex/internal/agent"
)

// tiny 1×1 PNG.
var pngBytes = []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0, 0, 0, 13, 'I', 'H', 'D', 'R', 0, 0, 0, 1, 0, 0, 0, 1, 8, 6, 0, 0, 0, 0x1f, 0x15, 0xc4, 0x89}

func writeImage(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, pngBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestImageMentionBecomesChipAndFilePart(t *testing.T) {
	m, _ := testModel(t)
	writeImage(t, m.o.Cwd, "shots/ui.png")
	if err := os.WriteFile(filepath.Join(m.o.Cwd, "notes.txt"), []byte("plain"), 0o644); err != nil {
		t.Fatal(err)
	}
	ag := &agent.Agent{}
	m.setSession(Connection{Agent: ag, Name: "fake/m"})

	// Typing the mention and pressing enter: the image is split out, the
	// text file is expanded inline as before.
	m.setInput("look at @shots/ui.png and @notes.txt please")
	typeKeys(m, "enter")
	if len(m.attachments) != 0 {
		t.Fatal("chips must be cleared after sending")
	}
	if got := m.blocks[len(m.blocks)-1]; got.kind != blockUser || got.text.String() != "look at and @notes.txt please" || len(got.files) != 1 || got.files[0] != "ui.png" {
		t.Fatalf("user block = %q files=%v", got.text.String(), got.files)
	}
	view := ansi.Strip(m.vp.View())
	if !strings.Contains(view, "▣ ui.png") {
		t.Fatalf("transcript must list the image:\n%s", view)
	}
	// The pending run's message is what startRun captured; run it against a
	// nil model to inspect the message instead: simulate via submit.
	m.running, m.cancel = false, nil
	m.attachments = nil
	if err := m.attach("shots/ui.png"); err != nil {
		t.Fatal(err)
	}
	parts := m.fileParts()
	if len(parts) != 1 || parts[0].MediaType != "image/png" || parts[0].Filename != "ui.png" || string(parts[0].Data) != string(pngBytes) {
		t.Fatalf("file parts = %+v", parts)
	}
	msg := fantasy.NewUserMessage("x", parts...)
	if len(msg.Content) != 2 {
		t.Fatalf("message parts = %d", len(msg.Content))
	}
	if _, ok := msg.Content[1].(fantasy.FilePart); !ok {
		t.Fatalf("second part = %T", msg.Content[1])
	}
}

func TestAttachmentChipsKeyboardAndMouse(t *testing.T) {
	m, _ := testModel(t)
	writeImage(t, m.o.Cwd, "a.png")
	writeImage(t, m.o.Cwd, "b.jpg")
	m.setSession(Connection{Agent: &agent.Agent{}, Name: "fake/m"})

	// Dropped file paths arrive as a paste.
	m.Update(tea.PasteMsg{Content: filepath.Join(m.o.Cwd, "a.png") + " "})
	m.Update(tea.PasteMsg{Content: "b.jpg"})
	if len(m.attachments) != 2 || m.attachments[0].name != "a.png" || m.attachments[1].mediaType != "image/jpeg" {
		t.Fatalf("attachments = %+v", m.attachments)
	}
	if m.input.Value() != "" {
		t.Fatalf("a dropped image must not land in the text: %q", m.input.Value())
	}
	// Ordinary pastes still go to the input.
	m.Update(tea.PasteMsg{Content: "hello.txt"})
	if m.input.Value() != "hello.txt" {
		t.Fatalf("plain paste = %q", m.input.Value())
	}
	m.input.Reset()

	// The chip row sits inside the input box and costs one row.
	view := ansi.Strip(m.inputView())
	if !strings.Contains(view, "Image 1") || !strings.Contains(view, "Image 2") || strings.Count(view, "×") != 2 || strings.Contains(view, "a.png") || strings.Contains(view, "b.jpg") {
		t.Fatalf("input view:\n%s", view)
	}
	// Box: top border, chip row, input row, bottom border; footer: 2 rows.
	if m.vp.Height() != m.height-(1+1+2)-footerRows {
		t.Fatalf("viewport height %d must leave room for the chip row", m.vp.Height())
	}
	if c := m.View().Cursor; c == nil || c.Y != m.vp.Height()+2 {
		t.Fatalf("cursor must move below the chip row: %+v", c)
	}

	// Click the first ×: the chip row is just under the top border, and
	// chips start after the border and its padding.
	if len(m.chipHits) != 2 {
		t.Fatalf("chip hits = %+v", m.chipHits)
	}
	normal := m.inputView()
	m.hoverAt(m.chipHits[1].x0+2, m.chipRowY())
	if m.hoverChip != 2 || m.inputView() == normal || ansi.Strip(m.inputView()) != ansi.Strip(normal) {
		t.Fatal("hover must change only the remove button style, without changing layout")
	}
	m.hoverAt(0, m.chipRowY())
	if m.hoverChip != 0 || m.inputView() != normal {
		t.Fatal("leaving the remove button must restore the normal style")
	}
	if m.chipClick(m.chipHits[0].x1) {
		t.Fatal("padding after the remove button must not remove an image")
	}
	click(m, m.chipHits[0].x0+2, m.chipRowY())
	if len(m.attachments) != 1 || m.attachments[0].name != "b.jpg" {
		t.Fatalf("after click: %+v", m.attachments)
	}
	if view := ansi.Strip(m.inputView()); !strings.Contains(view, "Image 1") || strings.Contains(view, "Image 2") {
		t.Fatalf("remaining images should be renumbered: %s", view)
	}
	if parts := m.fileParts(); len(parts) != 1 || parts[0].Filename != "b.jpg" {
		t.Fatalf("display labels must not replace the original filename: %+v", parts)
	}
	// Backspace on an empty input drops the last chip.
	typeKeys(m, "backspace")
	if len(m.attachments) != 0 || m.vp.Height() != m.height-(1+2)-footerRows {
		t.Fatalf("after backspace: %+v vp=%d", m.attachments, m.vp.Height())
	}
	// Backspace with text edits the text instead.
	if err := m.attach("a.png"); err != nil {
		t.Fatal(err)
	}
	m.setInput("ab")
	typeKeys(m, "backspace")
	if len(m.attachments) != 1 || m.input.Value() != "a" {
		t.Fatalf("backspace with text: %+v %q", m.attachments, m.input.Value())
	}
}

func TestAttachmentRejectsNonImagesAndOversize(t *testing.T) {
	m, _ := testModel(t)
	if err := os.WriteFile(filepath.Join(m.o.Cwd, "doc.pdf"), []byte("%PDF"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := m.attach("doc.pdf"); err == nil {
		t.Fatal("pdf must be rejected")
	}
	if err := m.attach("missing.png"); err == nil {
		t.Fatal("missing file must be rejected")
	}
	big := filepath.Join(m.o.Cwd, "big.png")
	f, err := os.Create(big)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(maxAttachmentBytes + 1); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := m.attach("big.png"); err == nil || !strings.Contains(err.Error(), "under 10.0 MB") {
		t.Fatalf("oversize err = %v", err)
	}
	// Non-image pastes that look like paths stay text.
	if _, ok := pastedImagePath(m.o.Cwd, "doc.pdf"); ok {
		t.Fatal("pdf path must not become a chip")
	}
	if _, ok := pastedImagePath(m.o.Cwd, "a.png\nb.png"); ok {
		t.Fatal("multi-line paste must not become a chip")
	}
}

func TestCompletionAcceptsImageAsChip(t *testing.T) {
	m, _ := testModel(t)
	writeImage(t, m.o.Cwd, "shot.png")
	m.files = []string{"shot.png", "main.go"}
	m.setInput("see @sh")
	m.comp = nil
	runCmd(m.updateCompletion())
	if m.comp == nil || len(m.comp.items) == 0 {
		t.Fatal("completion should offer shot.png")
	}
	if got := m.accept(); got != "@shot.png" {
		t.Fatalf("accept = %q", got)
	}
	if m.input.Value() != "see " || len(m.attachments) != 1 || m.attachments[0].name != "shot.png" {
		t.Fatalf("input=%q attachments=%+v", m.input.Value(), m.attachments)
	}
}

func TestResumedSessionShowsImages(t *testing.T) {
	msgs := []fantasy.Message{
		fantasy.NewUserMessage("what is this", fantasy.FilePart{Filename: "cat.png", MediaType: "image/png", Data: pngBytes}),
		{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{fantasy.TextPart{Text: "a cat"}}},
	}
	blocks := blocksFromMessages(msgs)
	if len(blocks) != 2 || len(blocks[0].files) != 1 || blocks[0].files[0] != "cat.png" {
		t.Fatalf("blocks = %+v", blocks[0])
	}
}
