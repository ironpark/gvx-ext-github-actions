package mask

import (
	"encoding/base64"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

var ansiEscapeRe = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)`)

// Redactor masks registered secret values (and common encoded forms) with "***".
// It is safe for concurrent use.
type Redactor struct {
	mu       sync.RWMutex
	secrets  map[string]string
	patterns []string
}

// New creates an empty Redactor.
func New() *Redactor {
	return &Redactor{
		secrets: make(map[string]string),
	}
}

// SetAll replaces the secret map with values keyed by secret name.
func (r *Redactor) SetAll(values map[string]string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.secrets = make(map[string]string, len(values))
	for k, v := range values {
		if v != "" {
			r.secrets[k] = v
		}
	}
	r.rebuildLocked()
}

// Add registers a single secret value under name (or an ad-hoc value when name is empty).
func (r *Redactor) Add(name, value string) {
	if value == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.secrets == nil {
		r.secrets = make(map[string]string)
	}
	if name == "" {
		name = "__anon_" + value
	}
	r.secrets[name] = value
	r.rebuildLocked()
}

// Remove removes a named secret from the redactor.
func (r *Redactor) Remove(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.secrets, name)
	r.rebuildLocked()
}

func (r *Redactor) rebuildLocked() {
	uniq := make(map[string]struct{}, len(r.secrets)*3)
	for _, val := range r.secrets {
		val = strings.TrimSpace(val)
		if len(val) < 3 {
			continue
		}
		uniq[val] = struct{}{}
		if b64 := base64.StdEncoding.EncodeToString([]byte(val)); len(b64) >= 4 {
			uniq[b64] = struct{}{}
		}
		if esc := url.QueryEscape(val); esc != val && len(esc) >= 3 {
			uniq[esc] = struct{}{}
		}
	}
	pats := make([]string, 0, len(uniq))
	for p := range uniq {
		pats = append(pats, p)
	}
	sort.Slice(pats, func(i, j int) bool {
		if len(pats[i]) != len(pats[j]) {
			return len(pats[i]) > len(pats[j])
		}
		return pats[i] < pats[j]
	})
	r.patterns = pats
}

// Mask replaces every registered secret in s with "***".
func (r *Redactor) Mask(s string) string {
	if s == "" || r == nil {
		return s
	}
	r.mu.RLock()
	pats := r.patterns
	r.mu.RUnlock()
	if len(pats) == 0 {
		return s
	}
	out := s
	for _, p := range pats {
		if strings.Contains(out, p) {
			out = strings.ReplaceAll(out, p, "***")
		}
	}
	return out
}

// SanitizeLine strips ANSI escape sequences, normalizes whitespace, removes
// control/bidi-spoofing runes rejected by uiblock, and masks registered secrets.
func (r *Redactor) SanitizeLine(s string) string {
	s = r.Mask(s)
	return SanitizeInline(s)
}

// SanitizeInline strips ANSI sequences and control/bidi-spoofing runes so the
// string is always accepted by GVX Desktop's strict uiblock validator.
func SanitizeInline(s string) string {
	if s == "" {
		return ""
	}
	s = ansiEscapeRe.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\t", "  ")
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "?")
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, ru := range s {
		if isSpoofingOrControl(ru) {
			continue
		}
		b.WriteRune(ru)
	}
	return b.String()
}

// SanitizeMultiline strips ANSI escape codes and unsafe control/bidi characters
// while keeping newlines and tabs intact (for uiblock code/markdown nodes).
func (r *Redactor) SanitizeMultiline(s string) string {
	s = r.Mask(s)
	if s == "" {
		return ""
	}
	s = ansiEscapeRe.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "?")
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, ru := range s {
		if ru != '\n' && ru != '\t' && isSpoofingOrControl(ru) {
			continue
		}
		b.WriteRune(ru)
	}
	return b.String()
}

// TruncateRunes truncates s to at most maxRunes runes, appending "..." if trimmed.
func TruncateRunes(s string, maxRunes int) string {
	if maxRunes <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	runes := []rune(s)
	if maxRunes <= 3 {
		return string(runes[:maxRunes])
	}
	return string(runes[:maxRunes-3]) + "..."
}

func isSpoofingOrControl(r rune) bool {
	switch {
	case unicode.IsControl(r):
		return true
	case r >= 0x202A && r <= 0x202E, r >= 0x2066 && r <= 0x2069, r == 0x200E, r == 0x200F, r == 0x061C:
		return true
	case r == 0x200B || r == 0x200C || r == 0x200D || r == 0xFEFF:
		return true
	}
	return false
}
