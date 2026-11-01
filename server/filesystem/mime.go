package filesystem

import (
	"mime"
	"path/filepath"
	"strings"
)

// listingMimeExtras covers common game-server / panel file types that the
// standard mime database may miss, plus archive types so Atlas isArchiveFile()
// can offer decompress (Go's mime.TypeByExtension often returns wrong or empty
// values for these — e.g. .zip → application/x-zip-compressed, .zst → x-compressed).
var listingMimeExtras = map[string]string{
	".yml":        "application/yaml",
	".yaml":       "application/yaml",
	".toml":       "application/toml",
	".properties": "text/x-java-properties",
	".cfg":        "text/plain",
	".conf":       "text/plain",
	".ini":        "text/plain",
	".log":        "text/plain",
	".lua":        "text/x-lua",
	".sk":         "text/plain",
	".mcmeta":     "application/json",
	".lang":       "text/plain",
	".nbt":        "application/octet-stream",
	".schematic":  "application/octet-stream",
	".litematic":  "application/octet-stream",

	// Archives — MIME values match Atlas resources/js/api/files.ts isArchiveFile().
	".zip":  "application/zip",
	".7z":   "application/x-7z-compressed",
	".rar":  "application/vnd.rar",
	".tar":  "application/x-tar",
	".gz":   "application/gzip",
	".tgz":  "application/gzip",
	".tbz":  "application/x-bzip2",
	".tbz2": "application/x-bzip2",
	".txz":  "application/x-xz",
	".zst":  "application/zstd",
	".xz":   "application/x-xz",
	".bz2":  "application/x-bzip2",
	".br":   "application/x-br",
	".lz":   "application/x-lzip",
	".sz":   "application/x-sz",
	".lz4":  "application/x-lz4",
}

// compoundArchiveSuffixes are checked before filepath.Ext so foo.tar.gz / foo.tar.zst
// get a stable type (Ext alone would only see .gz / .zst, which is usually fine,
// but .tgz-style names and odd OS mime dbs still need the map above).
var compoundArchiveSuffixes = []string{
	".tar.gz", ".tar.zst", ".tar.xz", ".tar.bz2", ".tar.br", ".tar.lz", ".tar.lz4", ".tar.sz",
}

// MimeByName returns a MIME type from the file name extension only.
// Unknown extensions fall back to application/octet-stream.
func MimeByName(name string) string {
	lower := strings.ToLower(name)
	for _, suffix := range compoundArchiveSuffixes {
		if strings.HasSuffix(lower, suffix) {
			// Outer compression type drives Atlas decompress affordance.
			ext := filepath.Ext(suffix) // e.g. .gz from .tar.gz
			if m, ok := listingMimeExtras[ext]; ok {
				return m
			}
		}
	}

	ext := strings.ToLower(filepath.Ext(name))
	if ext == "" {
		return "application/octet-stream"
	}
	if m, ok := listingMimeExtras[ext]; ok {
		return m
	}
	if m := mime.TypeByExtension(ext); m != "" {
		if i := strings.IndexByte(m, ';'); i >= 0 {
			m = strings.TrimSpace(m[:i])
		}
		return m
	}
	return "application/octet-stream"
}
