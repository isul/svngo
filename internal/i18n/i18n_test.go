package i18n

import (
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

var rePlaceholder = regexp.MustCompile(`\{\d+\}`)

func placeholders(s string) []string {
	p := rePlaceholder.FindAllString(s, -1)
	slices.Sort(p)
	return slices.Compact(p)
}

// Every built-in language must define exactly the base keys with the same
// placeholders, so no screen silently falls back or drops an argument.
func TestBuiltinParity(t *testing.T) {
	base := builtins[Base]
	if base == nil || len(base.msgs) < 100 {
		t.Fatal("base language missing or too small")
	}
	for code, l := range builtins {
		if l.name == "" {
			t.Errorf("%s: empty name", code)
		}
		for k, v := range base.msgs {
			got, ok := l.msgs[k]
			if !ok {
				t.Errorf("%s: missing key %s", code, k)
				continue
			}
			if strings.TrimSpace(got) == "" {
				t.Errorf("%s: empty value for %s", code, k)
			}
			if !slices.Equal(placeholders(v), placeholders(got)) {
				t.Errorf("%s: placeholders of %s differ: %v vs %v", code, k, placeholders(v), placeholders(got))
			}
		}
		for k := range l.msgs {
			if _, ok := base.msgs[k]; !ok {
				t.Errorf("%s: key %s not in base language", code, k)
			}
		}
	}
	if builtins["ko"] == nil {
		t.Fatal("Korean is not built in")
	}
}

func TestFormat(t *testing.T) {
	cases := []struct {
		s    string
		args []any
		want string
	}{
		{"{0} / {1} / {0}", []any{"a", 2}, "a / 2 / a"},
		{"no args {0}", nil, "no args {0}"},
		{"missing {2}", []any{"x"}, "missing {2}"},
		{"brace { and {x} {0}", []any{"ok"}, "brace { and {x} ok"},
		{"{0}}", []any{1}, "1}"},
	}
	for _, c := range cases {
		if got := Format(c.s, c.args...); got != c.want {
			t.Errorf("Format(%q) = %q, want %q", c.s, got, c.want)
		}
	}
}

func TestMatch(t *testing.T) {
	r := New("")
	cases := map[string]string{
		"":                               "",
		"ko-KR,ko;q=0.9,en-US;q=0.8":     "ko",
		"en-US,en;q=0.9":                 "en",
		"fr-FR,fr;q=0.9":                 "",
		"fr;q=0.9, ko;q=0.5":             "ko",
		"ja;q=0.5, ko;q=0.8, en;q=0.1":   "ko",
		"ko;q=0, en":                     "en",
		"*":                              "",
		"KO_kr":                          "ko",
		strings.Repeat("xx,", 30) + "ko": "",
	}
	for h, want := range cases {
		if got := r.Match(h); got != want {
			t.Errorf("Match(%q) = %q, want %q", h, got, want)
		}
	}
}

func TestLookupFallback(t *testing.T) {
	r := New("")
	if got := r.T("ko", "nav.repos"); got != "저장소" {
		t.Fatalf("ko nav.repos = %q", got)
	}
	if got := r.T("xx", "nav.repos"); got != "Repositories" {
		t.Fatalf("unknown language should fall back to English, got %q", got)
	}
	if got := r.T("ko", "no.such.key"); got != "no.such.key" {
		t.Fatalf("unknown key = %q", got)
	}
	if got := r.T("ko", "pager.total", 7); got != "총 7건" {
		t.Fatalf("pager.total = %q", got)
	}
}

func writeFile(t *testing.T, dir, name, data string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDirectoryLanguages(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "ja.json", `{"_meta":{"code":"ja","name":"日本語"},"nav.repos":"リポジトリ"}`)
	writeFile(t, dir, "ko.json", `{"_meta":{"name":"한국어 (사내)"},"nav.repos":"레포"}`)
	writeFile(t, dir, "bad.json", `{not json`)
	writeFile(t, dir, "Bad Name.json", `{}`)
	writeFile(t, dir, "notes.txt", `ignored`)
	r := New(dir)

	if !r.Has("ja") || r.T("ja", "nav.repos") != "リポジトリ" {
		t.Fatal("ja not loaded from directory")
	}
	if got := r.T("ja", "nav.users"); got != "Users" {
		t.Fatalf("missing ja key should fall back to English, got %q", got)
	}
	if r.T("ko", "nav.repos") != "레포" || r.T("ko", "nav.users") != "사용자" {
		t.Fatal("ko override not merged with built-in translations")
	}
	errs := r.LoadErrors()
	if len(errs) != 2 {
		t.Fatalf("expected 2 load errors, got %v", errs)
	}
	var infos = map[string]Info{}
	for _, in := range r.List() {
		infos[in.Code] = in
	}
	if in := infos["ja"]; in.Builtin || !in.File || in.Keys != 1 || in.Missing == 0 || in.Percent() >= 100 {
		t.Fatalf("ja info = %+v", in)
	}
	if in := infos["ko"]; !in.Builtin || !in.File || in.Name != "한국어 (사내)" || in.Missing != 0 || in.Percent() != 100 {
		t.Fatalf("ko info = %+v", in)
	}
	if in := infos["en"]; !in.Builtin || in.File {
		t.Fatalf("en info = %+v", in)
	}

	os.Remove(filepath.Join(dir, "ja.json"))
	r.Reload()
	if r.Has("ja") {
		t.Fatal("ja still available after its file was removed and reloaded")
	}
}

func TestInstallRemoveExport(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "locales")
	r := New(dir)

	code, err := r.Install([]byte(`{"_meta":{"name":"Deutsch"},"nav.repos":"Repositorys"}`), "DE")
	if err != nil || code != "de" {
		t.Fatalf("Install = %q, %v", code, err)
	}
	if !r.Has("de") || r.T("de", "nav.repos") != "Repositorys" {
		t.Fatal("installed language not available")
	}
	if _, err := os.Stat(filepath.Join(dir, "de.json")); err != nil {
		t.Fatal("language file not written:", err)
	}

	data, err := r.Export("de")
	if err != nil {
		t.Fatal(err)
	}
	var exp map[string]any
	if err := json.Unmarshal(data, &exp); err != nil {
		t.Fatal(err)
	}
	if len(exp) != len(builtins[Base].msgs)+1 || exp["nav.repos"] != "Repositorys" || exp["nav.users"] != "Users" {
		t.Fatalf("export incomplete: %d keys", len(exp))
	}
	if !strings.HasPrefix(string(data), "{\n  \"_meta\": ") {
		t.Fatalf("export should start with _meta: %.40s", data)
	}
	// an exported file can be installed again as is
	if code, err := r.Install(data, ""); err != nil || code != "de" {
		t.Fatalf("reinstall export = %q, %v", code, err)
	}
	if _, err := r.Export("zz"); err == nil {
		t.Fatal("export of unknown language succeeded")
	}

	if err := r.Remove("de"); err != nil {
		t.Fatal(err)
	}
	if r.Has("de") {
		t.Fatal("language still available after Remove")
	}
	if err := r.Remove("en"); err == nil {
		t.Fatal("removing a built-in language without a file succeeded")
	}
	if err := r.Remove("../x"); err == nil {
		t.Fatal("Remove accepted an invalid code")
	}
}

func TestInstallRejects(t *testing.T) {
	r := New(t.TempDir())
	big := `{"_meta":{"code":"xx","name":"X"},"k":"` + strings.Repeat("a", MaxFileSize) + `"}`
	cases := map[string]struct{ data, fallback, key string }{
		"json":                    {`{bad`, "xx", "locale.err.json"},
		"no code":                 {`{"_meta":{"name":"X"}}`, "", "locale.err.no_code"},
		"bad code with file name": {`{"_meta":{"code":"../etc","name":"X"}}`, "etc", "locale.err.code"},
		"bad file name":           {`{"_meta":{"name":"X"}}`, "a b", "locale.err.code"},
		"bad code":                {`{"_meta":{"code":"../etc","name":"X"}}`, "", "locale.err.code"},
		"no name":                 {`{"_meta":{"code":"xx"}}`, "", "locale.err.name"},
		"meta":                    {`{"_meta":"xx"}`, "", "locale.err.meta"},
		"value":                   {`{"_meta":{"code":"xx","name":"X"},"k":1}`, "", "locale.err.value"},
		"too big":                 {big, "", "locale.err.too_large"},
	}
	for name, c := range cases {
		_, err := r.Install([]byte(c.data), c.fallback)
		var e *Error
		if !errors.As(err, &e) || e.Msg.Key != c.key {
			t.Errorf("%s: err = %v, want %s", name, err, c.key)
		}
	}
	if len(r.Codes()) != len(builtins) {
		t.Fatalf("rejected files changed the language list: %v", r.Codes())
	}
	if _, err := New("").Install([]byte(`{"_meta":{"code":"xx","name":"X"}}`), ""); err == nil {
		t.Fatal("Install without a directory succeeded")
	}
}

func TestInstallLanguageLimit(t *testing.T) {
	r := New(t.TempDir())
	for i := len(r.Codes()); i < MaxLanguages; i++ {
		code := fmt.Sprintf("x%c%c", 'a'+i/26, 'a'+i%26)
		if _, err := r.Install([]byte(`{"_meta":{"code":"`+code+`","name":"X"}}`), ""); err != nil {
			t.Fatalf("install %s: %v", code, err)
		}
	}
	_, err := r.Install([]byte(`{"_meta":{"code":"zz","name":"X"}}`), "")
	var e *Error
	if !errors.As(err, &e) || e.Msg.Key != "locale.err.too_many_languages" {
		t.Fatalf("language limit not enforced: %v", err)
	}
	// replacing an existing language is still allowed
	if _, err := r.Install([]byte(`{"_meta":{"code":"ko","name":"한국어"}}`), ""); err != nil {
		t.Fatalf("override at the limit: %v", err)
	}
}

func TestHTMLEscapesTranslations(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "xx.json", `{"_meta":{"code":"xx","name":"X"},"repo.rename_hint":"<script>alert(1)</script> {0} {1}"}`)
	l := New(dir).For("xx")
	got := l.HTML("repo.rename_hint", template.HTML("<code>svn</code>"), "<b>")
	want := template.HTML("&lt;script&gt;alert(1)&lt;/script&gt; <code>svn</code> &lt;b&gt;")
	if got != want {
		t.Fatalf("HTML = %q", got)
	}
}

func TestMsgEncodingAndErrors(t *testing.T) {
	r := New("")
	ko := r.For("ko")

	m := M("job.restore_ok", "proj", 12)
	enc := Encode(m)
	if !strings.HasPrefix(enc, encodedPrefix) {
		t.Fatalf("Encode = %q", enc)
	}
	if got := ko.Msg(Decode(enc)); got != "'proj' 복원 완료 (r12)" {
		t.Fatalf("decoded = %q", got)
	}
	if got := ko.Msg(Decode("legacy plain text")); got != "legacy plain text" {
		t.Fatalf("legacy = %q", got)
	}
	if Encode(Raw("plain")) != "plain" || Encode(Msg{}) != "" {
		t.Fatal("raw messages should be stored as plain text")
	}

	err := Wrap(E("valid.backup_name"), "upload.err.failed")
	if err.Error() != "Upload failed: Invalid backup file name" {
		t.Fatalf("Error() = %q", err.Error())
	}
	if got := ko.Err(err); got != "업로드 실패: 잘못된 백업 파일 이름입니다" {
		t.Fatalf("ko.Err = %q", got)
	}
	plain := fmt.Errorf("context: %w", errors.New("disk full"))
	if got := ko.Err(Wrap(plain, "job.err.backup")); got != "백업 실패: context: disk full" {
		t.Fatalf("wrapped plain error = %q", got)
	}
	wrappedTwice := fmt.Errorf("outer: %w", E("valid.access"))
	if got := ko.Err(wrappedTwice); !strings.Contains(got, "권한은") {
		t.Fatalf("errors.As through fmt wrapping = %q", got)
	}
	if ko.Any(nil) != "" || ko.Any("raw") != "raw" || ko.Any(M("nav.repos")) != "저장소" || ko.Any(errors.New("x")) != "x" {
		t.Fatal("Any conversions")
	}
	if got := ko.TOr("audit.unknown.action", "unknown.action"); got != "unknown.action" {
		t.Fatalf("TOr fallback = %q", got)
	}
}
