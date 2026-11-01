package shadow

import "testing"

func TestNormalizeRelativePath(t *testing.T) {
	ok := []struct {
		in, want string
	}{
		{"world/level.dat", "world/level.dat"},
		{"./plugins/a.jar", "plugins/a.jar"},
		{"foo\\bar", "foo/bar"},
	}
	for _, tc := range ok {
		got, err := normalizeRelativePath(tc.in)
		if err != nil {
			t.Fatalf("%q: unexpected error: %v", tc.in, err)
		}
		if got != tc.want {
			t.Fatalf("%q: got %q want %q", tc.in, got, tc.want)
		}
	}

	// a/../b cleans to b (safe). a/../../b cleans to ../b (rejected).
	if got, err := normalizeRelativePath("a/../b"); err != nil || got != "b" {
		t.Fatalf("a/../b: got %q err=%v", got, err)
	}

	bad := []string{"", ".", "/", "/abs", "..", "../x", "a/../../b"}
	for _, in := range bad {
		if _, err := normalizeRelativePath(in); err == nil {
			t.Fatalf("%q: expected error", in)
		}
	}
}
