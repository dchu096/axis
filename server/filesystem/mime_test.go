package filesystem

import "testing"

func TestMimeByName(t *testing.T) {
	cases := map[string]string{
		"server.properties": "text/x-java-properties",
		"config.yml":        "application/yaml",
		"config.yaml":       "application/yaml",
		"paper.toml":        "application/toml",
		"latest.log":        "text/plain",
		"script.lua":        "text/x-lua",
		"pack.mcmeta":       "application/json",
		"noext":             "application/octet-stream",

		"mods.zip":           "application/zip",
		"world.7z":           "application/x-7z-compressed",
		"dump.rar":           "application/vnd.rar",
		"files.tar":          "application/x-tar",
		"files.tar.gz":       "application/gzip",
		"files.tgz":          "application/gzip",
		"files.tar.zst":      "application/zstd",
		"files.zst":          "application/zstd",
		"files.tar.xz":       "application/x-xz",
		"files.xz":           "application/x-xz",
		"files.tar.bz2":      "application/x-bzip2",
		"files.bz2":          "application/x-bzip2",
		"files.br":           "application/x-br",
		"files.lz":           "application/x-lzip",
		"files.sz":           "application/x-sz",
		"files.lz4":          "application/x-lz4",
	}
	for name, want := range cases {
		if got := MimeByName(name); got != want {
			t.Errorf("MimeByName(%q) = %q, want %q", name, got, want)
		}
	}

	// Stdlib-backed types should not be empty; exact value can vary by OS mime db.
	if got := MimeByName("readme.txt"); got == "" || got == "application/octet-stream" {
		t.Errorf("MimeByName(readme.txt) = %q, want a text type from stdlib", got)
	}
}
