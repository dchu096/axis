package shadow

import (
	"fmt"
	"path/filepath"
	"strings"
)

// normalizeRelativePath rejects absolute paths, empty paths, and any ".."
// segment so promote/list targets stay under the shadow and live roots.
func normalizeRelativePath(p string) (string, error) {
	p = strings.TrimSpace(p)
	p = strings.ReplaceAll(p, "\\", "/")
	p = strings.TrimPrefix(p, "./")
	if p == "" || p == "." {
		return "", fmt.Errorf("shadow: path must not be empty")
	}
	if filepath.IsAbs(p) || strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("shadow: absolute paths are not allowed")
	}
	cleaned := filepath.ToSlash(filepath.Clean(p))
	if cleaned == "." || cleaned == "" {
		return "", fmt.Errorf("shadow: path must not be empty")
	}
	if strings.HasPrefix(cleaned, "../") || cleaned == ".." || strings.Contains(cleaned, "/../") {
		return "", fmt.Errorf("shadow: path must not contain parent directory references")
	}
	for _, part := range strings.Split(cleaned, "/") {
		if part == ".." {
			return "", fmt.Errorf("shadow: path must not contain parent directory references")
		}
		if part == "" {
			return "", fmt.Errorf("shadow: path is invalid")
		}
	}
	return cleaned, nil
}
