// Package i18n provides translated UI messages. Languages are JSON files with
// flat "key": "text" pairs; English and Korean are built in and more languages
// (or overrides of built-in ones) are loaded from a directory at runtime.
package i18n

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"

	"svngo/internal/fsutil"
)

// Base is the reference language: every key must exist in it and it is the
// fallback for missing translations.
const Base = "en"

const (
	MaxFileSize  = 512 << 10
	MaxKeys      = 5000
	MaxLanguages = 50
	maxKeyLen    = 200
	maxValueLen  = 4000
	maxNameLen   = 64
)

//go:embed locales/*.json
var builtinFS embed.FS

var reCode = regexp.MustCompile(`^[a-z]{2,3}(-[a-z0-9]{2,8})?$`)

// NormalizeCode lower-cases a language tag and uses "-" as separator.
func NormalizeCode(s string) string {
	return strings.ToLower(strings.ReplaceAll(strings.TrimSpace(s), "_", "-"))
}

// ValidCode reports whether s is an acceptable (normalized) language code.
func ValidCode(s string) bool { return reCode.MatchString(s) }

type locale struct {
	code    string
	name    string
	builtin bool
	file    bool
	msgs    map[string]string
}

// Info describes an available language for the UI.
type Info struct {
	Code    string
	Name    string
	Builtin bool // shipped with the binary
	File    bool // loaded from the locales directory (overrides built-in keys)
	Keys    int  // reference keys translated
	Missing int  // reference keys missing (shown in the base language)
}

// Percent is the share of reference keys that are translated.
func (i Info) Percent() int {
	if total := i.Keys + i.Missing; total > 0 {
		return i.Keys * 100 / total
	}
	return 100
}

// Registry holds all languages. It is safe for concurrent use.
type Registry struct {
	dir     string
	builtin map[string]*locale

	mu      sync.RWMutex
	locales map[string]*locale
	errs    []error
}

var builtins = mustLoadBuiltins()

func mustLoadBuiltins() map[string]*locale {
	ents, err := builtinFS.ReadDir("locales")
	if err != nil {
		panic(err)
	}
	m := map[string]*locale{}
	for _, e := range ents {
		code := strings.TrimSuffix(e.Name(), ".json")
		b, err := builtinFS.ReadFile("locales/" + e.Name())
		if err != nil {
			panic(err)
		}
		l, err := parse(b, code)
		if err != nil {
			panic(fmt.Sprintf("built-in locale %s: %v", e.Name(), err))
		}
		l.builtin = true
		m[l.code] = l
	}
	if m[Base] == nil {
		panic("built-in locale " + Base + " missing")
	}
	return m
}

// New returns a registry with the built-in languages plus the files in dir
// (dir may be empty or missing). Invalid files are skipped and reported by
// LoadErrors.
func New(dir string) *Registry {
	r := &Registry{dir: dir, builtin: builtins}
	r.Reload()
	return r
}

// Reload re-reads the locales directory and returns problems with its files.
func (r *Registry) Reload() []error {
	next := map[string]*locale{}
	for code, l := range r.builtin {
		next[code] = l
	}
	var errs []error
	if r.dir != "" {
		ents, err := os.ReadDir(r.dir)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
		for _, e := range ents {
			if !e.Type().IsRegular() || !strings.HasSuffix(e.Name(), ".json") || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			code := strings.TrimSuffix(e.Name(), ".json")
			if !ValidCode(code) {
				errs = append(errs, Wrap(E("locale.err.file_code"), "locale.err.file", e.Name()))
				continue
			}
			if next[code] == nil && len(next) >= MaxLanguages {
				errs = append(errs, Wrap(E("locale.err.too_many_languages", MaxLanguages), "locale.err.file", e.Name()))
				continue
			}
			b, err := readLimited(filepath.Join(r.dir, e.Name()))
			if err != nil {
				errs = append(errs, Wrap(err, "locale.err.file", e.Name()))
				continue
			}
			l, err := parse(b, code)
			if err != nil {
				errs = append(errs, Wrap(err, "locale.err.file", e.Name()))
				continue
			}
			next[code] = merge(r.builtin[code], l)
		}
	}
	r.mu.Lock()
	r.locales, r.errs = next, errs
	r.mu.Unlock()
	return errs
}

func readLimited(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(io.LimitReader(f, MaxFileSize+1)); err != nil {
		return nil, err
	}
	if buf.Len() > MaxFileSize {
		return nil, E("locale.err.too_large", MaxFileSize>>10)
	}
	return buf.Bytes(), nil
}

// merge overlays a file on top of the built-in language with the same code.
func merge(base, over *locale) *locale {
	over.file = true
	if base == nil {
		return over
	}
	m := make(map[string]string, len(base.msgs)+len(over.msgs))
	for k, v := range base.msgs {
		m[k] = v
	}
	for k, v := range over.msgs {
		m[k] = v
	}
	return &locale{code: base.code, name: over.name, builtin: true, file: true, msgs: m}
}

// parse validates a language file. code is used when _meta.code is absent
// and must match it otherwise.
func parse(b []byte, code string) (*locale, error) {
	if len(b) > MaxFileSize {
		return nil, E("locale.err.too_large", MaxFileSize>>10)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, Wrap(err, "locale.err.json")
	}
	var meta struct {
		Code string `json:"code"`
		Name string `json:"name"`
	}
	if m, ok := raw["_meta"]; ok {
		if err := json.Unmarshal(m, &meta); err != nil {
			return nil, E("locale.err.meta")
		}
	}
	code = NormalizeCode(code)
	if meta.Code != "" {
		mc := NormalizeCode(meta.Code)
		if code != "" && mc != code {
			return nil, E("locale.err.code_mismatch", mc, code)
		}
		code = mc
	}
	if code == "" {
		return nil, E("locale.err.no_code")
	}
	if !ValidCode(code) {
		return nil, E("locale.err.code", code)
	}
	meta.Name = strings.TrimSpace(meta.Name)
	if meta.Name == "" || len([]rune(meta.Name)) > maxNameLen {
		return nil, E("locale.err.name")
	}
	if len(raw) > MaxKeys+1 {
		return nil, E("locale.err.too_many_keys", MaxKeys)
	}
	msgs := make(map[string]string, len(raw))
	for k, v := range raw {
		if k == "_meta" {
			continue
		}
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			return nil, E("locale.err.value", k)
		}
		if k == "" || len(k) > maxKeyLen || len(s) > maxValueLen {
			return nil, E("locale.err.length", k)
		}
		msgs[k] = s
	}
	return &locale{code: code, name: meta.Name, msgs: msgs}, nil
}

// LoadErrors returns the problems found by the last Reload.
func (r *Registry) LoadErrors() []error {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return slices.Clone(r.errs)
}

// Has reports whether the language is available.
func (r *Registry) Has(code string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.locales[code] != nil
}

// Codes returns the available language codes, sorted.
func (r *Registry) Codes() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	codes := make([]string, 0, len(r.locales))
	for c := range r.locales {
		codes = append(codes, c)
	}
	sort.Strings(codes)
	return codes
}

// List describes the available languages, sorted by code.
func (r *Registry) List() []Info {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ref := r.locales[Base].msgs
	res := make([]Info, 0, len(r.locales))
	for _, l := range r.locales {
		in := Info{Code: l.code, Name: l.name, Builtin: l.builtin, File: l.file}
		for k := range ref {
			if _, ok := l.msgs[k]; ok {
				in.Keys++
			} else {
				in.Missing++
			}
		}
		res = append(res, in)
	}
	sort.Slice(res, func(i, j int) bool { return res[i].Code < res[j].Code })
	return res
}

// Lookup returns the text for key in code, falling back to the base language.
func (r *Registry) Lookup(code, key string) (string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if l := r.locales[code]; l != nil {
		if s, ok := l.msgs[key]; ok {
			return s, true
		}
	}
	if s, ok := r.locales[Base].msgs[key]; ok {
		return s, true
	}
	return "", false
}

// T translates key into code; unknown keys are returned as is.
func (r *Registry) T(code, key string, args ...any) string {
	s, ok := r.Lookup(code, key)
	if !ok {
		return key
	}
	return Format(s, args...)
}

// Format replaces {0}, {1}, ... with the arguments.
func Format(s string, args ...any) string {
	if len(args) == 0 || !strings.Contains(s, "{") {
		return s
	}
	var b strings.Builder
	for {
		i := strings.IndexByte(s, '{')
		if i < 0 {
			break
		}
		j := strings.IndexByte(s[i:], '}')
		if j < 0 {
			break
		}
		n, err := strconv.Atoi(s[i+1 : i+j])
		if err != nil || n < 0 || n >= len(args) {
			b.WriteString(s[:i+1])
			s = s[i+1:]
			continue
		}
		b.WriteString(s[:i])
		b.WriteString(fmt.Sprint(args[n]))
		s = s[i+j+1:]
	}
	b.WriteString(s)
	return b.String()
}

// Match picks the best available language for an Accept-Language header.
// It returns "" when nothing matches.
func (r *Registry) Match(header string) string {
	tags := parseAcceptLanguage(header)
	if len(tags) == 0 {
		return ""
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, t := range tags {
		if r.locales[t] != nil {
			return t
		}
		base, _, _ := strings.Cut(t, "-")
		if r.locales[base] != nil {
			return base
		}
		var region []string
		for c := range r.locales {
			if strings.HasPrefix(c, base+"-") {
				region = append(region, c)
			}
		}
		if len(region) > 0 {
			sort.Strings(region)
			return region[0]
		}
	}
	return ""
}

func parseAcceptLanguage(h string) []string {
	if len(h) > 1024 {
		h = h[:1024]
	}
	type tag struct {
		code string
		q    float64
	}
	var tags []tag
	for i, part := range strings.Split(h, ",") {
		if i >= 20 {
			break
		}
		code, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		code = NormalizeCode(code)
		if code == "" || code == "*" {
			continue
		}
		q := 1.0
		for _, p := range strings.Split(params, ";") {
			if v, ok := strings.CutPrefix(strings.TrimSpace(p), "q="); ok {
				if f, err := strconv.ParseFloat(v, 64); err == nil {
					q = f
				}
			}
		}
		if q <= 0 {
			continue
		}
		tags = append(tags, tag{code, q})
	}
	sort.SliceStable(tags, func(i, j int) bool { return tags[i].q > tags[j].q })
	res := make([]string, len(tags))
	for i, t := range tags {
		res[i] = t.code
	}
	return res
}

// Install validates a language file and stores it in the locales directory.
// fallbackCode (e.g. from the uploaded file name) is used when the file has no
// _meta.code. It returns the language code.
func (r *Registry) Install(data []byte, fallbackCode string) (string, error) {
	if r.dir == "" {
		return "", E("locale.err.no_dir")
	}
	l, err := parse(data, "")
	if err != nil && fallbackCode != "" {
		var e *Error
		if errors.As(err, &e) && e.Msg.Key == "locale.err.no_code" {
			l, err = parse(data, fallbackCode)
		}
	}
	if err != nil {
		return "", err
	}
	if !r.Has(l.code) && len(r.Codes()) >= MaxLanguages {
		return "", E("locale.err.too_many_languages", MaxLanguages)
	}
	out, err := marshal(l.code, l.name, l.msgs)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(r.dir, 0o750); err != nil {
		return "", err
	}
	if err := fsutil.AtomicWrite(filepath.Join(r.dir, l.code+".json"), out); err != nil {
		return "", err
	}
	r.Reload()
	return l.code, nil
}

// Remove deletes a language file. Built-in languages fall back to their
// shipped translations.
func (r *Registry) Remove(code string) error {
	if r.dir == "" || !ValidCode(code) {
		return E("locale.err.not_file", code)
	}
	if err := os.Remove(filepath.Join(r.dir, code+".json")); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return E("locale.err.not_file", code)
		}
		return err
	}
	r.Reload()
	return nil
}

// Export returns a complete language file for code: every reference key with
// the current translation, or the base text where it is missing.
func (r *Registry) Export(code string) ([]byte, error) {
	r.mu.RLock()
	l := r.locales[code]
	ref := r.locales[Base].msgs
	var msgs map[string]string
	if l != nil {
		msgs = make(map[string]string, len(ref))
		for k, v := range ref {
			msgs[k] = v
		}
		for k, v := range l.msgs {
			msgs[k] = v
		}
	}
	r.mu.RUnlock()
	if l == nil {
		return nil, E("locale.err.unknown", code)
	}
	return marshal(l.code, l.name, msgs)
}

func marshal(code, name string, msgs map[string]string) ([]byte, error) {
	keys := make([]string, 0, len(msgs))
	for k := range msgs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b bytes.Buffer
	enc := func(v any) error {
		var vb bytes.Buffer
		e := json.NewEncoder(&vb)
		e.SetEscapeHTML(false)
		if err := e.Encode(v); err != nil {
			return err
		}
		b.Write(bytes.TrimRight(vb.Bytes(), "\n"))
		return nil
	}
	b.WriteString("{\n  \"_meta\": ")
	if err := enc(map[string]string{"code": code, "name": name}); err != nil {
		return nil, err
	}
	for _, k := range keys {
		b.WriteString(",\n  ")
		if err := enc(k); err != nil {
			return nil, err
		}
		b.WriteString(": ")
		if err := enc(msgs[k]); err != nil {
			return nil, err
		}
	}
	b.WriteString("\n}\n")
	return b.Bytes(), nil
}
