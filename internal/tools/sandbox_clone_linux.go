package tools

import "os"

// Linux retains the existing streaming copy into fresh private inodes.
func cloneSandboxFile(_ *os.File, _ *os.Root, _ string) (*os.File, error) {
	return nil, nil
}
