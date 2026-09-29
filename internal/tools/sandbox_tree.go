package tools

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// These bounds also apply to hostile output (including concurrent writers).
// Do not follow links, reproduce special nodes, or read an unbounded stream.
const sandboxTreeBytes int64 = 8 << 30
const sandboxTreeEntries = 250_000

type sandboxEntry struct {
	mode     os.FileMode
	hash     [sha256.Size]byte
	link     string
	modified int64 // regular files only; directory mtimes change during copying
}

type sandboxTree struct {
	path, execution string
	host            *os.Root
	before          map[string]sandboxEntry
}

type sandboxReader struct {
	ctx context.Context
	io.Reader
}

func (r sandboxReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.Reader.Read(p)
}

func snapshotDirectory(source *os.Root, name string, dir *os.File) (*os.Root, error) {
	// The literal /. forces a directory open even if a descendant swaps name
	// for a FIFO. filepath.Join would incorrectly remove this suffix.
	root, err := source.OpenRoot(name + "/.")
	if err != nil {
		return nil, err
	}
	held, statErr := dir.Stat()
	pinned, pinErr := root.Stat(".")
	if statErr != nil || pinErr != nil || !os.SameFile(held, pinned) {
		_ = root.Close()
		return nil, fmt.Errorf("directory changed during snapshot: %s", name)
	}
	return root, nil
}

// snapshotTree copies to fresh inodes, or only hashes when destination is empty.
// With a baseline, it captures only changed regular files. The manifest still
// contains every entry; absent capture payloads must never imply deletions.
// The returned manifest describes the bytes actually captured, not a separate
// earlier stat/read. An active hostile writer can produce inconsistent bytes,
// but cannot retain a writable descriptor to the captured files.
func snapshotTree(ctx context.Context, source *os.Root, destination string, baseline map[string]sandboxEntry) (map[string]sandboxEntry, error) {
	var dest *os.Root
	if destination != "" {
		if err := os.Mkdir(destination, 0o700); err != nil {
			return nil, err
		}
		var err error
		dest, err = os.OpenRoot(destination)
		if err != nil {
			return nil, err
		}
		defer func() { _ = dest.Close() }()
	}
	entries := make(map[string]sandboxEntry)
	remaining := sandboxTreeBytes
	buffer := make([]byte, 32*1024)
	var walk func(*os.Root, *os.Root, string, string) error
	walk = func(source, dest *os.Root, name, path string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if len(entries) >= sandboxTreeEntries {
			return errors.New("sandbox tree exceeds 250000 entries")
		}
		info, err := source.Lstat(name)
		if err != nil {
			return err
		}
		entry := sandboxEntry{mode: info.Mode().Type() | info.Mode().Perm()}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			entry.link, err = source.Readlink(name)
			if err == nil && dest != nil && baseline == nil {
				err = dest.Symlink(entry.link, name)
			}
		case info.IsDir():
			var dir *os.File
			dir, err = source.OpenFile(name, os.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
			if err != nil {
				break
			}
			defer func() { _ = dir.Close() }()
			// Keep traversal relative to pinned directories, not the tree root.
			childSource, openErr := snapshotDirectory(source, name, dir)
			if openErr != nil {
				return openErr
			}
			defer func() { _ = childSource.Close() }()
			childDest := dest
			if dest != nil && baseline == nil && name != "." {
				if err = dest.Mkdir(name, 0o700); err != nil {
					break
				}
				childDest, err = dest.OpenRoot(name + "/.")
				if err != nil {
					break
				}
				defer func() { _ = childDest.Close() }()
			}
			// Record before descending, so the entry bound includes directories.
			entries[path] = entry
			for err == nil {
				children, readErr := dir.ReadDir(128)
				for _, child := range children {
					if err != nil {
						break
					}
					err = walk(childSource, childDest, child.Name(), filepath.Join(path, child.Name()))
				}
				if readErr != nil {
					if !errors.Is(readErr, io.EOF) {
						err = errors.Join(err, readErr)
					}
					break
				}
			}
			if err == nil && dest != nil && baseline == nil && name != "." {
				err = dest.Chmod(name, info.Mode().Perm())
			}
		case info.Mode().IsRegular():
			var file *os.File
			// O_NONBLOCK prevents a swapped FIFO from hanging the parent.
			file, err = source.OpenFile(name, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
			if err != nil {
				break
			}
			var opened os.FileInfo
			opened, err = file.Stat()
			if err == nil && !opened.Mode().IsRegular() {
				err = fmt.Errorf("not a regular file: %s", name)
			}
			if err == nil && opened.Size() > remaining {
				err = errors.New("sandbox tree exceeds 8 GiB")
			}
			if err == nil {
				entry.mode = opened.Mode().Perm()
				entry.modified = opened.ModTime().UnixNano()
				var output *os.File
				if dest != nil && baseline == nil {
					output, err = dest.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
				}
				if err == nil {
					hash := sha256.New()
					var writer io.Writer = hash
					if output != nil {
						writer = io.MultiWriter(hash, output)
					}
					var size int64
					size, err = io.CopyBuffer(writer, sandboxReader{ctx, io.LimitReader(file, remaining+1)}, buffer)
					remaining -= size
					if remaining < 0 {
						err = errors.New("sandbox tree exceeds 8 GiB")
					}
					copy(entry.hash[:], hash.Sum(nil))
					if err == nil && dest != nil && baseline != nil && baseline[path] != entry {
						// Re-read the same FD into a trusted, fresh inode. A late
						// writer cannot alter captured bytes or make us publish
						// a payload different from the manifest we compared.
						if _, err = file.Seek(0, io.SeekStart); err == nil {
							err = dest.MkdirAll(filepath.Dir(path), 0o700)
						}
						if err == nil {
							output, err = dest.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
						}
						if err == nil {
							hash.Reset()
							var copied int64
							copied, err = io.CopyBuffer(io.MultiWriter(hash, output), sandboxReader{ctx, io.LimitReader(file, size+1)}, buffer)
							if err == nil && (copied != size || string(hash.Sum(nil)) != string(entry.hash[:])) {
								err = fmt.Errorf("file changed during capture: %s", path)
							}
						}
					}
				}
				if output != nil {
					err = errors.Join(err, output.Chmod(entry.mode), output.Close())
					if err == nil {
						outputName := name
						if baseline != nil {
							outputName = path
						}
						err = dest.Chtimes(outputName, opened.ModTime(), opened.ModTime())
					}
				}
			}
			_ = file.Close()
		default:
			err = fmt.Errorf("unsupported sandbox filesystem entry %s (%s)", name, info.Mode().Type())
		}
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		entries[path] = entry
		return nil
	}
	if err := walk(source, dest, ".", "."); err != nil {
		return nil, err
	}
	return entries, nil
}

type sandboxChanges struct {
	tree     *sandboxTree
	captured *os.Root
	after    map[string]sandboxEntry
	live     map[string]sandboxEntry
	names    []string
}

// Publish is optimistic, not a multi-file transaction. Preflight all trees
// before any write, then recheck individual entries during application. A host
// editor can still race check-and-rename; a cooperating malicious host that
// moves open roots or inserts aliases into private copies is outside the model.
func (s *sandboxRun) Publish(ctx context.Context) error {
	plans := []*sandboxChanges{}
	defer func() {
		for _, plan := range plans {
			_ = plan.captured.Close()
		}
	}()
	for i, tree := range s.trees {
		execution, err := os.OpenRoot(tree.execution)
		if err != nil {
			return err
		}
		path := filepath.Join(s.temp, fmt.Sprintf("capture-%d", i))
		after, err := snapshotTree(ctx, execution, path, tree.before)
		_ = execution.Close()
		if err != nil {
			return fmt.Errorf("capture failed; command changes discarded: %w", err)
		}
		captured, err := os.OpenRoot(path)
		if err != nil {
			return err
		}
		plan := &sandboxChanges{tree: tree, captured: captured, after: after}
		plans = append(plans, plan)
		for name, entry := range after {
			if name != "." && tree.before[name] != entry {
				plan.names = append(plan.names, name)
			}
		}
		for name := range tree.before {
			if _, exists := after[name]; !exists && name != "." {
				plan.names = append(plan.names, name)
			}
		}
		if len(plan.names) == 0 {
			continue // No writes proposed: leave host files and concurrent edits alone.
		}
		plan.live, err = snapshotTree(ctx, tree.host, "", nil)
		if err != nil {
			return fmt.Errorf("publication preflight failed; command changes discarded: %w", err)
		}
		for _, name := range plan.names {
			for ancestor := name; ancestor != "."; ancestor = filepath.Dir(ancestor) {
				if !sameSandboxEntry(tree.before, plan.live, ancestor) {
					return fmt.Errorf("workspace changed during execution at %s; command changes discarded", filepath.Join(tree.path, ancestor))
				}
			}
			before, was := tree.before[name]
			entry, exists := after[name]
			if was && exists && before.mode.IsDir() != entry.mode.IsDir() {
				return fmt.Errorf("directory type transition at %s is not supported; command changes discarded", name)
			}
			if entry.link != "" && filepath.IsAbs(entry.link) && within(s.temp, filepath.Clean(entry.link)) {
				return fmt.Errorf("symlink %s targets a private execution path; use a relative target; command changes discarded", name)
			}
			if was && before.mode.IsDir() && !exists {
				for child := range plan.live {
					if within(name, child) && !sameSandboxEntry(tree.before, plan.live, child) {
						return fmt.Errorf("directory changed during execution at %s; command changes discarded", child)
					}
				}
			}
		}
	}
	applied := 0
	for _, plan := range plans {
		// Remove children before parents; create parents before children.
		slices.SortFunc(plan.names, func(a, b string) int {
			_, aExists := plan.after[a]
			_, bExists := plan.after[b]
			if aExists != bExists {
				if !aExists {
					return -1
				}
				return 1
			}
			order := strings.Compare(a, b)
			if !aExists {
				return -order
			}
			return order
		})
		for _, name := range plan.names {
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("publication interrupted after %d entries: %w", applied, err)
			}
			if err := plan.apply(ctx, name); err != nil {
				return fmt.Errorf("publication stopped at %s after %d entries (earlier changes remain): %w", filepath.Join(plan.tree.path, name), applied, err)
			}
			applied++
		}
		// Populate new directories while writable, then restore their intended
		// permissions deepest-first. Never chmod a regular file in place.
		for _, name := range slices.Backward(plan.names) {
			entry, exists := plan.after[name]
			if exists && entry.mode.IsDir() {
				dir, err := plan.tree.host.OpenFile(name, os.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
				if err == nil {
					err = errors.Join(dir.Chmod(entry.mode.Perm()), dir.Close())
				}
				if err != nil {
					return fmt.Errorf("directory permissions failed at %s after %d entries (earlier changes remain): %w", name, applied, err)
				}
			}
		}
	}
	return nil
}

func sameSandboxEntry(a, b map[string]sandboxEntry, name string) bool {
	left, leftOK := a[name]
	right, rightOK := b[name]
	return leftOK == rightOK && left == right
}

func (p *sandboxChanges) apply(ctx context.Context, name string) error {
	// Never traverse a symlink ancestor, even one pointing inside the root.
	for ancestor := filepath.Dir(name); ancestor != "."; ancestor = filepath.Dir(ancestor) {
		info, err := p.tree.host.Lstat(ancestor)
		if err != nil || !info.IsDir() {
			return fmt.Errorf("parent changed: %s", ancestor)
		}
	}
	entry, exists := p.after[name]
	before, was := p.live[name]
	info, err := p.tree.host.Lstat(name)
	if errors.Is(err, os.ErrNotExist) && !was {
		err = nil
	} else if err == nil {
		if !was || info.Mode().Type()|info.Mode().Perm() != before.mode {
			return errors.New("destination changed after preflight")
		}
		if info.Mode().IsRegular() {
			file, openErr := p.tree.host.OpenFile(name, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
			if openErr != nil {
				return openErr
			}
			opened, statErr := file.Stat()
			if statErr != nil || !opened.Mode().IsRegular() {
				_ = file.Close()
				return errors.New("destination is no longer a regular file")
			}
			hash := sha256.New()
			_, err = io.Copy(hash, sandboxReader{ctx, io.LimitReader(file, sandboxTreeBytes+1)})
			_ = file.Close()
			if err == nil && (string(hash.Sum(nil)) != string(before.hash[:]) || opened.ModTime().UnixNano() != before.modified) {
				err = errors.New("destination contents changed after preflight")
			}
		} else if info.Mode()&os.ModeSymlink != 0 {
			var link string
			link, err = p.tree.host.Readlink(name)
			if err == nil && link != before.link {
				err = errors.New("destination link changed after preflight")
			}
		}
	}
	if err != nil {
		return err
	}
	if !exists {
		// Remove, never RemoveAll: a concurrently added child must survive.
		return p.tree.host.Remove(name)
	}
	if entry.mode.IsDir() {
		if !was {
			return p.tree.host.Mkdir(name, 0o700)
		}
		return nil
	}
	temp := filepath.Join(filepath.Dir(name), ".arkex-publish-"+rand.Text())
	defer func() { _ = p.tree.host.Remove(temp) }()
	if entry.mode&os.ModeSymlink != 0 {
		err = p.tree.host.Symlink(entry.link, temp)
	} else {
		var source, dest *os.File
		source, err = p.captured.Open(name)
		if err != nil {
			return err
		}
		defer func() { _ = source.Close() }()
		dest, err = p.tree.host.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			_, err = io.Copy(dest, sandboxReader{ctx, source})
			err = errors.Join(err, dest.Chmod(entry.mode.Perm()), dest.Close())
			if err == nil {
				stamp := time.Unix(0, entry.modified)
				err = p.tree.host.Chtimes(temp, stamp, stamp)
			}
		}
	}
	if err != nil {
		return err
	}
	// Never rename an execution inode: descendants may still hold writable FDs.
	return p.tree.host.Rename(temp, name)
}
