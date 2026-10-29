package i18n

import (
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"strings"
)

// Msg is a translatable message: a key with arguments, or raw text that is
// shown as is (tool output, user data). Inner is a localized cause appended
// as ": cause".
type Msg struct {
	Key   string   `json:"k,omitempty"`
	Args  []string `json:"a,omitempty"`
	Raw   string   `json:"r,omitempty"`
	Inner *Msg     `json:"i,omitempty"`
}

// M creates a message. Arguments are formatted with fmt.Sprint.
func M(key string, args ...any) Msg {
	m := Msg{Key: key}
	if len(args) > 0 {
		m.Args = make([]string, len(args))
		for i, a := range args {
			m.Args[i] = fmt.Sprint(a)
		}
	}
	return m
}

// Raw creates a message that is not translated.
func Raw(s string) Msg { return Msg{Raw: s} }

// IsZero reports whether the message is empty.
func (m Msg) IsZero() bool { return m.Key == "" && m.Raw == "" && m.Inner == nil }

func (m Msg) args() []any {
	a := make([]any, len(m.Args))
	for i, s := range m.Args {
		a[i] = s
	}
	return a
}

// String renders the message in the base language (for logs).
func (m Msg) String() string {
	return render(m, func(key string, args ...any) string { return builtinT(Base, key, args...) })
}

func render(m Msg, t func(string, ...any) string) string {
	s := m.Raw
	if m.Key != "" {
		s = t(m.Key, m.args()...)
	}
	if m.Inner != nil {
		if inner := render(*m.Inner, t); inner != "" {
			if s != "" {
				s += ": "
			}
			s += inner
		}
	}
	return s
}

func builtinT(code, key string, args ...any) string {
	if l := builtins[code]; l != nil {
		if s, ok := l.msgs[key]; ok {
			return Format(s, args...)
		}
	}
	return key
}

// Error is an error with a translatable message. Error() renders English so
// logs stay in one language; the web UI translates it with Localizer.Err.
type Error struct {
	Msg Msg
	Err error // optional cause
}

// E creates a translatable error.
func E(key string, args ...any) error { return &Error{Msg: M(key, args...)} }

// Wrap creates a translatable error with a cause ("message: cause").
func Wrap(err error, key string, args ...any) error {
	return &Error{Msg: M(key, args...), Err: err}
}

func (e *Error) Error() string {
	s := e.Msg.String()
	if e.Err != nil {
		s += ": " + e.Err.Error()
	}
	return s
}

func (e *Error) Unwrap() error { return e.Err }

// FromErr converts an error into a message, keeping translatable parts.
func FromErr(err error) Msg {
	if err == nil {
		return Msg{}
	}
	var e *Error
	if errors.As(err, &e) {
		m := e.Msg
		if e.Err != nil {
			inner := FromErr(e.Err)
			m.Inner = &inner
		}
		return m
	}
	return Raw(err.Error())
}

const encodedPrefix = `{"k":`

// Encode serializes a message for storage (e.g. job results).
func Encode(m Msg) string {
	if m.Key == "" && m.Inner == nil {
		return m.Raw
	}
	b, err := json.Marshal(m)
	if err != nil {
		return m.String()
	}
	return string(b)
}

// Decode reverses Encode; plain text is returned as a raw message.
func Decode(s string) Msg {
	if strings.HasPrefix(s, encodedPrefix) {
		var m Msg
		if json.Unmarshal([]byte(s), &m) == nil {
			return m
		}
	}
	return Raw(s)
}

// Localizer translates into one language.
type Localizer struct {
	reg  *Registry
	Lang string
}

// For returns a localizer for code (which should be available).
func (r *Registry) For(code string) *Localizer { return &Localizer{reg: r, Lang: code} }

// T translates key; unknown keys are returned as is.
func (l *Localizer) T(key string, args ...any) string { return l.reg.T(l.Lang, key, args...) }

// TOr translates key or returns fallback when no language defines it.
func (l *Localizer) TOr(key, fallback string, args ...any) string {
	s, ok := l.reg.Lookup(l.Lang, key)
	if !ok {
		return fallback
	}
	return Format(s, args...)
}

// HTML translates key into escaped HTML. Arguments of type template.HTML are
// inserted unescaped (they come from templates), everything else is escaped.
// Translation text itself is always escaped.
func (l *Localizer) HTML(key string, args ...any) template.HTML {
	s, ok := l.reg.Lookup(l.Lang, key)
	if !ok {
		s = key
	}
	safe := make([]any, len(args))
	for i, a := range args {
		if h, ok := a.(template.HTML); ok {
			safe[i] = string(h)
		} else {
			safe[i] = template.HTMLEscapeString(fmt.Sprint(a))
		}
	}
	return template.HTML(Format(template.HTMLEscapeString(s), safe...))
}

// Msg translates a message.
func (l *Localizer) Msg(m Msg) string { return render(m, l.T) }

// Err translates an error.
func (l *Localizer) Err(err error) string {
	if err == nil {
		return ""
	}
	return l.Msg(FromErr(err))
}

// Any translates a Msg, *Msg, error or returns a string as is.
func (l *Localizer) Any(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case Msg:
		return l.Msg(x)
	case *Msg:
		if x == nil {
			return ""
		}
		return l.Msg(*x)
	case error:
		return l.Err(x)
	case string:
		return x
	}
	return fmt.Sprint(v)
}
