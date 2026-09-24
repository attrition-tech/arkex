package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"charm.land/fantasy"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// attachment is an image waiting to ride along with the next prompt. It is
// shown as a chip above the input and sent as an image part of the user
// message, which the OpenAI, compatible and Anthropic providers all turn into
// their image content block.
type attachment struct {
	name      string // as the user referred to it (relative when possible)
	mediaType string
	data      []byte
}

// maxAttachmentBytes caps a single image; providers reject much beyond this
// and the whole conversation (with the image) is sent on every turn.
const maxAttachmentBytes = 10 << 20

var imageTypes = map[string]string{
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".gif":  "image/gif",
	".webp": "image/webp",
}

// imageMediaType returns the media type for an image path by extension, or
// "" when the file is not a supported image.
func imageMediaType(path string) string {
	return imageTypes[strings.ToLower(filepath.Ext(path))]
}

// loadAttachment reads an image for sending. name is what the user typed;
// relative names resolve under cwd.
func loadAttachment(cwd, name string) (attachment, error) {
	mt := imageMediaType(name)
	if mt == "" {
		return attachment{}, fmt.Errorf("%s: not a supported image (png, jpg, gif, webp)", name)
	}
	path := name
	if !filepath.IsAbs(path) {
		path = filepath.Join(cwd, name)
	}
	st, err := os.Stat(path)
	if err != nil {
		return attachment{}, err
	}
	if st.IsDir() {
		return attachment{}, fmt.Errorf("%s is a directory", name)
	}
	if st.Size() > maxAttachmentBytes {
		return attachment{}, fmt.Errorf("%s is %s; images must be under %s", name, humanBytes(st.Size()), humanBytes(maxAttachmentBytes))
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return attachment{}, err
	}
	return attachment{name: name, mediaType: mt, data: data}, nil
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%d KB", n/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// attach adds an image chip, replacing a chip with the same name.
func (m *model) attach(name string) error {
	a, err := loadAttachment(m.o.Cwd, name)
	if err != nil {
		return err
	}
	for i, old := range m.attachments {
		if old.name == a.name {
			m.attachments[i] = a
			return nil
		}
	}
	m.attachments = append(m.attachments, a)
	return nil
}

func (m *model) removeAttachment(i int) {
	if i < 0 || i >= len(m.attachments) {
		return
	}
	m.attachments = append(m.attachments[:i], m.attachments[i+1:]...)
	m.hoverChip = 0
	m.layout()
}

// fileParts converts the chips to message parts.
func (m *model) fileParts() []fantasy.FilePart {
	if len(m.attachments) == 0 {
		return nil
	}
	parts := make([]fantasy.FilePart, 0, len(m.attachments))
	for _, a := range m.attachments {
		parts = append(parts, fantasy.FilePart{Filename: filepath.Base(a.name), MediaType: a.mediaType, Data: a.data})
	}
	return parts
}

// attachmentNames lists the chips for the transcript.
func attachmentNames(parts []fantasy.FilePart) []string {
	var names []string
	for _, p := range parts {
		if strings.HasPrefix(p.MediaType, "image/") {
			names = append(names, p.Filename)
		}
	}
	return names
}

// imageMentions splits @image mentions out of text: they become chips rather
// than inline file contents. Other words are left alone.
func (m *model) takeImageMentions(text string) string {
	words := strings.Fields(text)
	kept := make([]string, 0, len(words))
	changed := false
	for _, w := range words {
		if strings.HasPrefix(w, "@") && len(w) > 1 {
			name := strings.TrimRight(w[1:], ".,:;)")
			if imageMediaType(name) != "" {
				if err := m.attach(name); err == nil {
					changed = true
					continue
				}
			}
		}
		kept = append(kept, w)
	}
	if !changed {
		return text
	}
	return strings.Join(kept, " ")
}

// pastedImagePath reports whether a paste is a single path to an image file,
// as terminals produce when a file is dropped onto them (macOS escapes
// spaces with backslashes and quotes paths with special characters).
func pastedImagePath(cwd, s string) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" || strings.ContainsAny(s, "\n\r") {
		return "", false
	}
	s = strings.Trim(s, `'"`)
	s = strings.ReplaceAll(s, `\ `, " ")
	if imageMediaType(s) == "" {
		return "", false
	}
	path := s
	if !filepath.IsAbs(path) {
		path = filepath.Join(cwd, s)
	}
	st, err := os.Stat(path)
	if err != nil || st.IsDir() {
		return "", false
	}
	if rel, err := filepath.Rel(cwd, path); err == nil && !strings.HasPrefix(rel, "..") {
		return rel, true
	}
	return path, true
}

// ---- chips ----

// chipHit records where a chip's × sits in the input box so a click can
// remove it. x is relative to the box's left edge, on the chip row.
type chipHit struct {
	x0, x1 int // column range of the × (inclusive, exclusive)
	i      int // attachment index
}

var (
	chipStyle       lipgloss.Style
	chipXStyle      lipgloss.Style
	chipXHoverStyle lipgloss.Style
)

// chipRow draws the attachment chips as a single line no wider than width
// and records the × positions for mouse hits.
func (m *model) chipRow(width int) string {
	m.chipHits = m.chipHits[:0]
	if len(m.attachments) == 0 {
		return ""
	}
	var sb strings.Builder
	x := 0
	for i := range m.attachments {
		label := fmt.Sprintf(" Image %d ", i+1)
		xStyle := chipXStyle
		if m.hoverChip == i+1 {
			xStyle = chipXHoverStyle
		}
		chip := chipStyle.Render(label) + xStyle.Render("×") + chipStyle.Render(" ")
		w := lipgloss.Width(chip) + 1
		if x+w > width {
			rest := len(m.attachments) - i
			if room := width - x; room > 4 {
				sb.WriteString(dimStyle.Render(ansi.Truncate(fmt.Sprintf("+%d more", rest), room, "")))
			}
			break
		}
		if x > 0 {
			sb.WriteByte(' ')
			x++
		}
		sb.WriteString(chip)
		m.chipHits = append(m.chipHits, chipHit{x0: x + lipgloss.Width(chip) - 2, x1: x + lipgloss.Width(chip) - 1, i: i})
		x += lipgloss.Width(chip)
	}
	return sb.String()
}

// chipClick removes the chip whose × is at column x, reporting whether the
// click was consumed.
func (m *model) chipClick(x int) bool {
	for _, h := range m.chipHits {
		if x >= h.x0 && x < h.x1 {
			m.removeAttachment(h.i)
			return true
		}
	}
	return false
}
