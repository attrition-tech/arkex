package tools

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// SandboxLimits optionally bound each root in preparation and publication.
// Zero means no configured cap; physical resources and cancellation still apply.
// Sources identify the effective config file or environment override in errors.
type SandboxLimits struct {
	MaxBytes      int64
	MaxEntries    int
	BytesSource   string
	EntriesSource string
}

func (l SandboxLimits) Effective() (SandboxLimits, error) {
	if l.MaxBytes < 0 || l.MaxBytes == math.MaxInt64 || l.MaxEntries < 0 {
		return l, fmt.Errorf("sandbox limits must be nonnegative; maxBytes must be less than %d", int64(math.MaxInt64))
	}
	if l.MaxBytes > 0 && l.BytesSource == "" {
		l.BytesSource = "tool settings"
	}
	if l.MaxEntries > 0 && l.EntriesSource == "" {
		l.EntriesSource = "tool settings"
	}
	return l, nil
}

func (l SandboxLimits) description() string {
	bytes, entries := "no byte cap configured", "no entry cap configured"
	if l.MaxBytes > 0 {
		bytes = fmt.Sprintf("%s (%d bytes, source: %s)", sandboxBytes(l.MaxBytes), l.MaxBytes, l.BytesSource)
	}
	if l.MaxEntries > 0 {
		entries = fmt.Sprintf("%d entries (source: %s)", l.MaxEntries, l.EntriesSource)
	}
	return bytes + "; " + entries
}

type sandboxContributor struct {
	path  string
	bytes int64
}

// Inventory is metadata-only: it can diagnose a large tree without copying or
// hashing its files. Bound the extra work, and label incomplete measurements.
func sandboxLimitError(ctx context.Context, root *os.Root, limits SandboxLimits, bound, crossing string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var total int64
	entries := 1 // include the root, as snapshotTree does
	files := []sandboxContributor{}
	dirs := map[string]int64{}
	note, err := walkFilesystem(ctx, root.Name(), true, 1_000_000, func(_ *os.Root, _, path string, info os.FileInfo) error {
		entries++
		if !info.Mode().IsRegular() {
			return nil
		}
		size := info.Size()
		if size > math.MaxInt64-total {
			return fmt.Errorf("logical byte count overflow")
		}
		total += size
		files = append(files, sandboxContributor{path, size})
		slices.SortFunc(files, compareContributors)
		if len(files) > 5 {
			files = files[:5]
		}
		first, _, nested := strings.Cut(path, string(filepath.Separator))
		if nested {
			dirs[first] += size
		}
		return nil
	})
	partial := ""
	if err != nil || note != "" {
		partial = "at least "
	}
	var out strings.Builder
	fmt.Fprintf(&out, "sandbox tree exceeds %s at %q in %q (the crossing entry, not necessarily the largest).\n", bound, crossing, root.Name())
	fmt.Fprintf(&out, "Measured %s%s (%d bytes), %d entries; budget %s.\n", partial, sandboxBytes(total), total, entries, limits.description())
	if partial != "" {
		fmt.Fprintf(&out, "Inventory incomplete: %s %v.\n", note, err)
	}
	if len(dirs) > 0 {
		contributors := make([]sandboxContributor, 0, len(dirs))
		for path, bytes := range dirs {
			contributors = append(contributors, sandboxContributor{path + "/", bytes})
		}
		slices.SortFunc(contributors, compareContributors)
		out.WriteString("Largest top-level directories (logical bytes):\n")
		for _, item := range contributors[:min(5, len(contributors))] {
			fmt.Fprintf(&out, "  %s (%d bytes) %q\n", sandboxBytes(item.bytes), item.bytes, item.path)
		}
	}
	out.WriteString("Largest files (logical bytes):\n")
	for _, item := range files {
		fmt.Fprintf(&out, "  %s (%d bytes) %q\n", sandboxBytes(item.bytes), item.bytes, item.path)
	}
	out.WriteString("Raise or remove the configured ARKEX_SANDBOX_MAX_BYTES / ARKEX_SANDBOX_MAX_ENTRIES, or sandbox.maxBytes / sandbox.maxEntries in config (README: Large workspaces). No size/count cap applies when omitted. workdir does not narrow the copied roots. Use bash read_only=true for confined diagnostics, or ls/grep without staging. Do not delete unrelated work to bypass this limit.")
	return fmt.Errorf("%s", out.String())
}

func compareContributors(a, b sandboxContributor) int {
	if a.bytes > b.bytes {
		return -1
	}
	if a.bytes < b.bytes {
		return 1
	}
	return strings.Compare(a.path, b.path)
}

func sandboxBytes(bytes int64) string {
	if bytes >= 1<<30 {
		return fmt.Sprintf("%.3g GiB", float64(bytes)/float64(1<<30))
	}
	if bytes >= 1<<20 {
		return fmt.Sprintf("%.3g MiB", float64(bytes)/float64(1<<20))
	}
	if bytes >= 1<<10 {
		return fmt.Sprintf("%.3g KiB", float64(bytes)/float64(1<<10))
	}
	return fmt.Sprintf("%d B", bytes)
}
