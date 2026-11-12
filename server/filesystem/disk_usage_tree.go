package filesystem

import (
	"cmp"
	"path"
	"slices"
	"strings"

	"emperror.dev/errors"
	"golang.org/x/sys/unix"

	"github.com/0x7d8/wings/internal/ufs"
)

// ErrNotDirectory is returned when DiskUsageTree is asked to walk a non-directory path.
var ErrNotDirectory = errors.New("filesystem: disk usage root must be a directory")

// DiskUsageNode is a recursive TreeSize-style storage breakdown for a server
// directory. Size is always the full recursive total in bytes (regular files
// only). Children are omitted when the depth budget is exhausted, but Size is
// still computed for that directory.
type DiskUsageNode struct {
	Name     string           `json:"name"`
	Path     string           `json:"path"`
	IsDir    bool             `json:"is_dir"`
	Size     int64            `json:"size"`
	Children []*DiskUsageNode `json:"children"`
}

const (
	defaultDiskUsageDepth = 6
	maxDiskUsageDepth     = 64
)

// DiskUsageTree walks directory and returns a recursive size tree.
//
// Symlinks are not followed and are omitted from the tree.
// Unreadable nested directories are skipped. Hard links are counted once.
func (fs *Filesystem) DiskUsageTree(directory string, depth int) (*DiskUsageNode, error) {
	directory = normalizeDiskUsagePath(directory)
	depth = clampDiskUsageDepth(depth)

	st, err := fs.unixFS.Lstat(directory)
	if err != nil {
		return nil, err
	}
	if !st.IsDir() {
		return nil, ErrNotDirectory
	}

	seen := make(map[uint64]struct{})
	node, err := fs.buildDiskUsageTree(directory, directory, depth, seen)
	if err != nil {
		return nil, errors.WrapIf(err, "server/filesystem: disk usage tree failed")
	}
	return node, nil
}

func normalizeDiskUsagePath(directory string) string {
	directory = strings.TrimSpace(directory)
	if directory == "" || directory == "." {
		return "/"
	}
	return path.Clean("/" + strings.TrimLeft(directory, "/"))
}

func clampDiskUsageDepth(depth int) int {
	if depth < 0 {
		return defaultDiskUsageDepth
	}
	if depth > maxDiskUsageDepth {
		return maxDiskUsageDepth
	}
	return depth
}

func diskUsageDisplayName(displayPath string) string {
	if displayPath == "/" {
		return "/"
	}
	return path.Base(displayPath)
}

func (fs *Filesystem) buildDiskUsageTree(fsPath, displayPath string, depth int, seen map[uint64]struct{}) (*DiskUsageNode, error) {
	node := &DiskUsageNode{
		Name:     diskUsageDisplayName(displayPath),
		Path:     displayPath,
		IsDir:    true,
		Children: []*DiskUsageNode{},
	}

	// Depth exhausted: still report full recursive size, omit nested children.
	if depth <= 0 {
		node.Size = fs.directorySizeLenient(fsPath, seen)
		return node, nil
	}

	entries, err := fs.unixFS.ReadDir(fsPath)
	if err != nil {
		return nil, err
	}

	var total int64
	for _, e := range entries {
		name := e.Name()
		if name == ".upload-tmp" {
			continue
		}
		childFs := path.Join(fsPath, name)
		childDisplay := path.Join(displayPath, name)

		// Don't follow symlinks (avoids loops / double-counting). Match
		// DirectorySize and omit them from the tree entirely.
		if e.Type()&ufs.ModeSymlink != 0 {
			continue
		}

		if e.IsDir() {
			child, cerr := fs.buildDiskUsageTree(childFs, childDisplay, depth-1, seen)
			if cerr != nil {
				// Skip unreadable directories instead of failing the whole walk.
				continue
			}
			node.Children = append(node.Children, child)
			total += child.Size
			continue
		}

		if !e.Type().IsRegular() {
			continue
		}

		info, lerr := fs.unixFS.Lstat(childFs)
		if lerr != nil {
			continue
		}

		size := fileSizeOnce(info, seen)
		child := &DiskUsageNode{
			Name:     name,
			Path:     childDisplay,
			IsDir:    false,
			Size:     size,
			Children: []*DiskUsageNode{},
		}
		node.Children = append(node.Children, child)
		total += size
	}

	slices.SortStableFunc(node.Children, func(a, b *DiskUsageNode) int {
		return cmp.Or(cmp.Compare(b.Size, a.Size), strings.Compare(a.Name, b.Name))
	})

	node.Size = total
	return node, nil
}

// directorySizeLenient sums regular-file sizes under root, skipping unreadable
// entries and not following symlinks. Hard links are counted once via seen.
func (fs *Filesystem) directorySizeLenient(root string, seen map[uint64]struct{}) int64 {
	var total int64

	entries, err := fs.unixFS.ReadDir(root)
	if err != nil {
		return 0
	}

	for _, e := range entries {
		if e.Name() == ".upload-tmp" {
			continue
		}
		child := path.Join(root, e.Name())

		if e.Type()&ufs.ModeSymlink != 0 {
			continue
		}
		if e.IsDir() {
			total += fs.directorySizeLenient(child, seen)
			continue
		}
		if !e.Type().IsRegular() {
			continue
		}

		info, err := fs.unixFS.Lstat(child)
		if err != nil {
			continue
		}
		total += fileSizeOnce(info, seen)
	}

	return total
}

func fileSizeOnce(info ufs.FileInfo, seen map[uint64]struct{}) int64 {
	sys, ok := info.Sys().(*unix.Stat_t)
	if ok && sys.Nlink > 1 {
		if _, exists := seen[sys.Ino]; exists {
			return 0
		}
		seen[sys.Ino] = struct{}{}
	}
	return info.Size()
}
