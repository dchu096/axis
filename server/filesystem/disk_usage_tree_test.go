package filesystem

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDiskUsageTree(t *testing.T) {
	fs, rfs := NewFs()
	t.Cleanup(func() { _ = os.RemoveAll(rfs.root) })

	mustWrite := func(p, content string) {
		t.Helper()
		if err := rfs.CreateServerFileFromString(p, content); err != nil {
			t.Fatalf("create %s: %v", p, err)
		}
	}
	mustMkdir := func(p string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(rfs.root, "server", p), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", p, err)
		}
	}

	mustMkdir("plugins")
	mustMkdir("plugins/nested")
	mustWrite("server.jar", "abcdefghij")          // 10
	mustWrite("plugins/a.jar", "12345")            // 5
	mustWrite("plugins/nested/b.jar", "xyz")       // 3

	root, err := fs.DiskUsageTree("/", 6)
	if err != nil {
		t.Fatalf("DiskUsageTree: %v", err)
	}
	if root.Name != "/" || root.Path != "/" || !root.IsDir {
		t.Fatalf("root meta = %+v", root)
	}
	if root.Size != 18 {
		t.Fatalf("root size = %d, want 18", root.Size)
	}
	if len(root.Children) < 2 {
		t.Fatalf("expected children, got %d", len(root.Children))
	}
	// Sorted by size descending: plugins (8) then server.jar (10)? Wait plugins=8, jar=10
	if root.Children[0].Name != "server.jar" || root.Children[0].Size != 10 {
		t.Fatalf("first child = %+v, want server.jar size 10", root.Children[0])
	}
	if root.Children[1].Name != "plugins" || root.Children[1].Size != 8 {
		t.Fatalf("second child = %+v, want plugins size 8", root.Children[1])
	}

	plugins := root.Children[1]
	if len(plugins.Children) != 2 {
		t.Fatalf("plugins children = %d", len(plugins.Children))
	}

	// depth=0 → full size, no children
	shallow, err := fs.DiskUsageTree("/", 0)
	if err != nil {
		t.Fatalf("depth 0: %v", err)
	}
	if shallow.Size != 18 {
		t.Fatalf("depth 0 size = %d, want 18", shallow.Size)
	}
	if len(shallow.Children) != 0 {
		t.Fatalf("depth 0 should omit children, got %d", len(shallow.Children))
	}

	// Subtree root
	sub, err := fs.DiskUsageTree("/plugins", 6)
	if err != nil {
		t.Fatalf("subtree: %v", err)
	}
	if sub.Name != "plugins" || sub.Path != "/plugins" || sub.Size != 8 {
		t.Fatalf("subtree = %+v", sub)
	}

	// Non-directory rejected
	if _, err := fs.DiskUsageTree("/server.jar", 6); err == nil {
		t.Fatal("expected error for file root")
	}

	// Missing path
	if _, err := fs.DiskUsageTree("/nope", 6); err == nil {
		t.Fatal("expected error for missing path")
	}
}

func TestDiskUsageTreeSkipsSymlinkTargets(t *testing.T) {
	fs, rfs := NewFs()
	t.Cleanup(func() { _ = os.RemoveAll(rfs.root) })

	if err := rfs.CreateServerFileFromString("real.txt", "hello"); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(rfs.root, "server", "real.txt")
	link := filepath.Join(rfs.root, "server", "link.txt")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks not supported: %v", err)
	}

	root, err := fs.DiskUsageTree("/", 6)
	if err != nil {
		t.Fatal(err)
	}
	if root.Size != 5 {
		t.Fatalf("root size = %d, want 5 (symlink target not double-counted)", root.Size)
	}
	for _, c := range root.Children {
		if c.Name == "link.txt" {
			t.Fatal("symlink should be omitted from the tree")
		}
	}
}

func TestNormalizeDiskUsagePath(t *testing.T) {
	cases := map[string]string{
		"":           "/",
		"/":          "/",
		".":          "/",
		"plugins":    "/plugins",
		"/plugins/":  "/plugins",
		"/a/../b":    "/b",
	}
	for in, want := range cases {
		if got := normalizeDiskUsagePath(in); got != want {
			t.Errorf("normalizeDiskUsagePath(%q) = %q, want %q", in, got, want)
		}
	}
}
