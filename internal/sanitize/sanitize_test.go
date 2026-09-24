package sanitize

import "testing"

func TestTerminal(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"plain", "hello\n\tworld", "hello\n\tworld"},
		{"unicode kept", "héllo — ✓ 你好", "héllo — ✓ 你好"},
		{"osc52 clipboard", "a\x1b]52;c;SGVsbG8=\x07b", "ab"},
		{"osc8 link with ST", "a\x1b]8;;https://evil\x1b\\click\x1b]8;;\x1b\\b", "aclickb"},
		{"csi clear", "a\x1b[2Jb", "ab"},
		{"csi with intermediates", "a\x1b[?25lb\x1b[1;31mc", "abc"},
		{"malformed csi stops at stray byte", "a\x1b[12\x07zb", "azb"},
		{"two-byte escape", "a\x1b7b\x1b(Bc", "abc"},
		{"dcs apc pm sos", "a\x1bPq\x1b\\b\x1b_x\x07c\x1b^y\x07d\x1bXz\x07e", "abcde"},
		{"truncated osc keeps text", "a\x1b]52;c;SGVs", "a]52;c;SGVs"},
		{"truncated csi keeps text", "a\x1b[1;", "a[1;"},
		{"lone esc at end", "a\x1b", "a"},
		{"c0 and del", "a\x00b\x07c\rd\x7fe\x08f", "abcdef"},
		{"c1 utf8", "a\u0085b\u009cc", "abc"},
		{"c2 followed by non-control kept", "a\u00a0b", "a\u00a0b"},
		{"osc then new escape", "a\x1b]0;title\x1b[2Jb", "ab"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Terminal(tc.in); got != tc.want {
				t.Fatalf("Terminal(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestTerminalDoesNotAllocateForCleanText(t *testing.T) {
	s := "just some ordinary text\nwith lines\tand tabs"
	n := testing.AllocsPerRun(100, func() { Terminal(s) })
	if n != 0 {
		t.Fatalf("clean text allocated %v times", n)
	}
}
