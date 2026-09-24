package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

func sampleForm() *form {
	f := newForm()
	f.add(choiceField("kind", "Kind", []choiceOpt{{label: "A"}, {label: "B"}, {label: "C", disabled: true}}, 0))
	f.add(textField("name", "Name", "", "your name"))
	f.add(secretField("key", "API key", "", ""))
	f.add(buttonField("reveal", "show")).inline = true
	f.add(checkField("on", "Enabled", true))
	f.add(buttonField("ok", "OK"))
	f.add(buttonField("cancel", "Cancel")).inline = true
	_ = f.setFocus(0)
	return f
}

func TestFormFocusAndKeys(t *testing.T) {
	f := sampleForm()
	press := func(k string) string {
		a, _ := f.key(key(k))
		return a
	}
	// Disabled choice options are skipped in both directions.
	if press("right"); f.get("kind").sel != 1 {
		t.Fatalf("sel = %d", f.get("kind").sel)
	}
	if press("right"); f.get("kind").sel != 0 {
		t.Fatalf("right skipped the disabled option wrongly: sel = %d", f.get("kind").sel)
	}
	if press("left"); f.get("kind").sel != 1 {
		t.Fatalf("left: sel = %d", f.get("kind").sel)
	}
	// tab walks every focusable field in order and stops at the end.
	var order []string
	for i := 0; i < 8; i++ {
		press("tab")
		order = append(order, f.focused().id)
	}
	if got := strings.Join(order, ","); got != "name,key,reveal,on,ok,cancel,cancel,cancel" {
		t.Fatalf("tab order = %s", got)
	}
	if a := press("enter"); a != "cancel" {
		t.Fatalf("enter on button = %q", a)
	}
	press("shift+tab")
	press("shift+tab")
	if f.focused().id != "on" {
		t.Fatalf("shift+tab landed on %s", f.focused().id)
	}
	if a := press("space"); a != "changed:on" || f.get("on").on {
		t.Fatalf("space on check: action=%q on=%v", a, f.get("on").on)
	}
	// Text input takes characters and paste; enter advances.
	_ = f.focusID("name")
	f.key(tea.KeyPressMsg{Code: 'j', Text: "j"})
	f.update(tea.PasteMsg{Content: "oe"})
	if f.value("name") != "joe" || !f.get("name").input.Focused() || f.get("key").input.Focused() {
		t.Fatalf("name = %q", f.value("name"))
	}
	press("enter")
	if f.focused().id != "key" {
		t.Fatalf("enter did not advance: %s", f.focused().id)
	}
}

func TestFormRenderLayoutsAndClicks(t *testing.T) {
	f := sampleForm()
	wide := f.render(70)
	text := ansi.Strip(strings.Join(wide, "\n"))
	if f.narrow || !strings.Contains(text, "Kind") || !strings.Contains(wide[0], "›") {
		t.Fatalf("wide render:\n%s", text)
	}
	if !strings.Contains(text, "  show  ") || !strings.Contains(text, "  OK      Cancel  ") || strings.Contains(text, "[ show ]") {
		t.Fatalf("buttons not laid out inline:\n%s", text)
	}
	for _, l := range wide {
		if w := ansi.StringWidth(l); w > 70 {
			t.Fatalf("line wider than the form (%d): %q", w, ansi.Strip(l))
		}
	}
	// Click on option B selects it; on the disabled C nothing changes.
	rb, rc := f.get("kind").rects[1], f.get("kind").rects[2]
	if a, _ := f.click(rb.x, rb.y); a != "changed:kind" || f.get("kind").sel != 1 {
		t.Fatalf("click B: %q sel=%d", a, f.get("kind").sel)
	}
	if a, _ := f.click(rc.x+1, rc.y); a != "" || f.get("kind").sel != 1 {
		t.Fatalf("click on disabled option changed selection: %q", a)
	}
	// Click on the text field focuses it; on the check toggles; on a button acts.
	r := f.get("name").rects[0]
	if a, _ := f.click(r.x+3, r.y); a != "" || f.focused().id != "name" {
		t.Fatal("click did not focus the text field")
	}
	if c := f.cursor(); c == nil || c.Y != r.y || c.X != r.x {
		t.Fatalf("cursor = %+v, want at field start %+v", c, r)
	}
	r = f.get("on").rects[0]
	if a, _ := f.click(r.x, r.y); a != "changed:on" || f.get("on").on {
		t.Fatal("click did not toggle the check")
	}
	r = f.get("cancel").rects[0]
	if a, _ := f.click(r.x+r.w-1, r.y); a != "cancel" {
		t.Fatalf("button click = %q", a)
	}
	if a, _ := f.click(0, 99); a != "" {
		t.Fatal("click outside any field acted")
	}

	// Narrow: labels move above their fields, nothing exceeds the width.
	narrow := f.render(40)
	if !f.narrow {
		t.Fatal("40 columns should be the narrow layout")
	}
	ntext := ansi.Strip(strings.Join(narrow, "\n"))
	if len(narrow) <= len(wide) || !strings.Contains(ntext, "\n") {
		t.Fatalf("narrow render did not add label rows:\n%s", ntext)
	}
	for _, l := range narrow {
		if w := ansi.StringWidth(l); w > 40 {
			t.Fatalf("narrow line too wide (%d): %q", w, ansi.Strip(l))
		}
	}
	// Errors and help render under their field; hidden fields vanish.
	f.get("name").err = "taken"
	f.get("key").hidden = true
	f.get("reveal").hidden = true
	out := ansi.Strip(strings.Join(f.render(70), "\n"))
	if !strings.Contains(out, "taken") || strings.Contains(out, "API key") || strings.Contains(out, "show") {
		t.Fatalf("err/hidden render:\n%s", out)
	}
	if f.get("key").focusable() {
		t.Fatal("hidden field still focusable")
	}
}
