package repopath

import "testing"

func TestClean(t *testing.T) {
	paths := map[string]string{"": "/", "/": "/", "trunk": "/trunk", "/trunk//a/": "/trunk/a", "/한글 폴더/x": "/한글 폴더/x"}
	for in, want := range paths {
		got, err := Clean(in)
		if err != nil || got != want {
			t.Errorf("Clean(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, p := range []string{"/a/../b", "..", "/./x", "/a\nb", "/a\x00"} {
		if _, err := Clean(p); err == nil {
			t.Errorf("Clean(%q) should fail", p)
		}
	}
}

func TestHelpers(t *testing.T) {
	for _, c := range []struct{ in, parent, base string }{
		{"/", "/", "/"}, {"/a", "/", "a"}, {"/a/b", "/a", "b"},
	} {
		if got := Parent(c.in); got != c.parent {
			t.Errorf("Parent(%q) = %q, want %q", c.in, got, c.parent)
		}
		if got := Base(c.in); got != c.base {
			t.Errorf("Base(%q) = %q, want %q", c.in, got, c.base)
		}
	}
	if Join("/", "a") != "/a" || Join("/a", "b") != "/a/b" {
		t.Error("Join")
	}
}
