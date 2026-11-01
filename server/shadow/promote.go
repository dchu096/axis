package shadow

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"emperror.dev/errors"

	"github.com/0x7d8/wings/internal/ufs"
	"github.com/0x7d8/wings/server/filesystem"
)

// PromotePaths copies each relative path from the shadow root onto the live
// server filesystem (overwrite/add, recursive for directories). Does not start
// or power the container.
func PromotePaths(shadowRoot string, live *filesystem.Filesystem, paths []string) error {
	if live == nil {
		return errors.New("shadow: live filesystem is nil")
	}
	for _, raw := range paths {
		rel, err := normalizeRelativePath(raw)
		if err != nil {
			return err
		}
		src := filepath.Join(shadowRoot, filepath.FromSlash(rel))
		info, err := os.Lstat(src)
		if err != nil {
			if os.IsNotExist(err) {
				return fmt.Errorf("shadow: path %q does not exist in shadow", rel)
			}
			return errors.Wrap(err, "shadow: failed to stat promote source")
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("shadow: refusing to promote symlink %q", rel)
		}
		if info.IsDir() {
			if err := promoteDirectory(src, rel, live); err != nil {
				return err
			}
			continue
		}
		if err := promoteFile(src, rel, info, live); err != nil {
			return err
		}
	}
	return nil
}

func promoteDirectory(srcRoot, relRoot string, live *filesystem.Filesystem) error {
	return filepath.WalkDir(srcRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(srcRoot, path)
		if err != nil {
			return err
		}
		targetRel := relRoot
		if rel != "." {
			targetRel = filepath.ToSlash(filepath.Join(relRoot, rel))
		}
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("shadow: refusing to promote symlink %q", targetRel)
		}
		if d.IsDir() {
			if targetRel == "" || targetRel == "." {
				return nil
			}
			parent := filepath.ToSlash(filepath.Dir(targetRel))
			name := filepath.Base(targetRel)
			if parent == "." {
				parent = "/"
			}
			if err := live.CreateDirectory(name, parent); err != nil && !errors.Is(err, os.ErrExist) && !os.IsExist(err) {
				return errors.Wrapf(err, "shadow: failed to create directory %q on live", targetRel)
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		return promoteFile(path, targetRel, info, live)
	})
}

func promoteFile(src, rel string, info os.FileInfo, live *filesystem.Filesystem) error {
	f, err := os.Open(src)
	if err != nil {
		return errors.Wrap(err, "shadow: failed to open promote source")
	}
	defer f.Close()

	if err := live.Write(rel, f, info.Size(), ufs.FileMode(info.Mode().Perm())); err != nil {
		return errors.Wrapf(err, "shadow: failed to write %q to live filesystem", rel)
	}
	atime := info.ModTime()
	if err := live.Chtimes(rel, atime, atime); err != nil {
		return errors.Wrapf(err, "shadow: failed to set times on %q", rel)
	}
	return nil
}
