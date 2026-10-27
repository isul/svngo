package server

import (
	"strings"
	"testing"
)

func TestDiffParser(t *testing.T) {
	lines := []string{
		"Modified: trunk/a.txt",
		"===================================================================",
		"--- trunk/a.txt\t(rev 1)",
		"+++ trunk/a.txt\t(rev 2)",
		"@@ -1 +1,2 @@",
		" hello",
		"+world",
		"",
		"Property changes on: trunk/dir",
		"___________________________________________________________________",
		"Added: svn:ignore",
		"## -0,0 +1 ##",
		"+*.o",
		"Added: secret/key.txt",
		"===================================================================",
		"+top secret",
	}
	var headers []string
	content := map[string][]string{}
	cur := ""
	dp := &diffParser{
		onHeader: func(h, p string) { headers = append(headers, p); cur = p },
		onLine:   func(l string) { content[cur] = append(content[cur], l) },
	}
	for _, l := range lines {
		dp.feed(l)
	}
	dp.flush()
	want := []string{"/trunk/a.txt", "/trunk/dir", "/secret/key.txt"}
	if strings.Join(headers, ",") != strings.Join(want, ",") {
		t.Fatalf("headers = %v, want %v", headers, want)
	}
	found := false
	for _, l := range content["/trunk/dir"] {
		if l == "Added: svn:ignore" {
			found = true
		}
	}
	if !found {
		t.Fatal("property line inside section was treated as a header")
	}
}
