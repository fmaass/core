// Package sanitize provides shared, stateless input policies.
//
// Policies are selected by field type. Their length caps are the primary
// request-size defense: plain text is 256 runes, identifiers 100, and long
// text 256 KiB.
package sanitize

import (
	"fmt"
	"html"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/microcosm-cc/bluemonday"
)

// Policy is the input-sanitization contract. Implementations must be
// stateless and safe for concurrent use; the package's exported policies
// are package-level singletons.
type Policy interface {
	Sanitize(input string) string
}

// PolicyFunc adapts a plain function into a Policy.
type PolicyFunc func(string) string

// Sanitize implements Policy.
func (f PolicyFunc) Sanitize(input string) string { return f(input) }

// Apply runs the policy in-place on a string pointer. Convenience for
// the common "sanitize this struct field before persisting" pattern;
// no-op when target is nil.
func Apply(target *string, policy Policy) {
	if target == nil {
		return
	}
	*target = policy.Sanitize(*target)
}

// Pair binds a target pointer with the policy that should clean it.
// Use with ApplyAll to express a service's input policy declaratively.
//
// Label is the user-facing name for the field ("Title", "Description",
// "Group name"). It's optional — ApplyAll ignores it entirely;
// ApplyAllWithWarnings uses it to build the user-friendly warning
// strings handlers can stamp on a response.warnings field. Empty
// label means no warning is produced even if the value was modified.
type Pair struct {
	Target *string
	Policy Policy
	Label  string
}

// ApplyAll runs each (target, policy) pair in order. The canonical
// "sanitize this entity's text fields" call site:
//
//	sanitize.ApplyAll(
//	    sanitize.Pair{Target: &req.Title, Policy: sanitize.PlainTextField},
//	    sanitize.Pair{Target: &req.Description, Policy: sanitize.RichText},
//	    sanitize.Pair{Target: &req.Tag, Policy: sanitize.ShortIdentifier},
//	)
func ApplyAll(pairs ...Pair) {
	for _, p := range pairs {
		Apply(p.Target, p.Policy)
	}
}

// ApplyAllWithWarnings runs each policy and returns user-friendly
// warning strings for every field that was modified AND has a
// non-empty Label. Returned warnings are safe to drop directly into a
// response.warnings field so the frontend toast machinery can surface
// them at info severity. Empty slice when nothing changed.
//
// Callers that don't yet want warning surfacing should keep using
// ApplyAll — the value semantics are identical, ApplyAll just throws
// the warning information away.
func ApplyAllWithWarnings(pairs ...Pair) []string {
	var warnings []string
	for _, p := range pairs {
		if p.Target == nil {
			continue
		}
		before := *p.Target
		*p.Target = p.Policy.Sanitize(before)
		if p.Label == "" || *p.Target == before {
			continue
		}
		warnings = append(warnings, describeMutation(before, *p.Target, p.Label))
	}
	return warnings
}

// describeMutation returns a user-facing truncation/HTML-removal warning.
// Marker-based HTML detection is heuristic and falls back to generic copy.
func describeMutation(before, after, label string) string {
	beforeRunes := utf8.RuneCountInString(before)
	afterRunes := utf8.RuneCountInString(after)
	truncated := afterRunes < beforeRunes
	hadHTMLMarkers := strings.ContainsAny(before, "<>&")
	switch {
	case truncated && hadHTMLMarkers:
		return fmt.Sprintf("%s had HTML formatting removed and was shortened to %d characters.", label, afterRunes)
	case truncated:
		return fmt.Sprintf("%s was shortened to %d characters to fit the maximum length.", label, afterRunes)
	case hadHTMLMarkers:
		return fmt.Sprintf("%s had HTML formatting removed.", label)
	default:
		return fmt.Sprintf("%s was cleaned up for safe storage.", label)
	}
}

// PlainTextField — short, single-line user-facing label or title:
// item / asset / milestone / workspace / page / label names + titles.
// Strips every HTML tag (any HTML here is an injection attempt), trims
// surrounding whitespace, caps at 256 runes (titles surface across
// board cards, breadcrumbs, picker chips, browser tabs — pathological
// lengths break layout).
var PlainTextField Policy = PolicyFunc(plainTextField)

// ShortIdentifier — short identifier-like value (asset_tag, slug, code,
// link-type name). Same shape as PlainTextField with a 100-rune cap;
// intentionally tighter because these fields are identifier-shaped
// (e.g. "LAP-001", URL slugs) rather than free-form titles.
var ShortIdentifier Policy = PolicyFunc(shortIdentifier)

// RichText — multi-line body content (descriptions, notes, test-step
// actual results). Stores the user-authored Markdown VERBATIM
// (byte-for-byte) and applies only a 256 KiB byte cap. The XSS trust
// boundary for this content is enforced at RENDER time — client-side
// DOMPurify (render-markdown.js), the Milkdown editor + its link
// sanitizer, or Go html/template auto-escape — never by stripping on
// the way in. See INFRA-28. The former decode-then-HTML-strip pipeline
// silently deleted inline-code spans such as `<port>` (INFRA-12) and
// is gone.
var RichText Policy = PolicyFunc(richText)

// LongDocument — long-form Markdown document (workspace knowledge
// pages, runbooks). Same policy as RichText: verbatim storage, same
// 256 KiB cap, render-time XSS boundary. Kept as a distinct policy so
// callers can express the intent ("this is a document, not a
// description") at the call site, and so the cap can diverge in future
// without churning every caller.
var LongDocument Policy = PolicyFunc(longDocument)

// Comment — user-submitted comment content (Markdown editor input).
// Same policy as RichText: stores the Markdown verbatim, caps at
// 256 KiB, and relies on the render-time XSS boundary. One uniform
// upper bound for any long-form user text.
var Comment Policy = PolicyFunc(commentPolicy)

// MarkdownURLOnly neutralizes dangerous URL schemes (javascript:,
// vbscript:, data:) in Markdown link / image syntax without touching
// anything else. Standalone URL-scheme guard for callers that need it
// without the render-time boundary; the RichText / LongDocument /
// Comment policies store content verbatim and do NOT apply this.
var MarkdownURLOnly Policy = PolicyFunc(markdownURLOnly)

// --- internals ---

var (
	strictPolicy = bluemonday.StrictPolicy()
	// Match dangerous Markdown link/image schemes, including single-level
	// parentheses in payloads such as javascript:alert(1). Used only by
	// MarkdownURLOnly now that the Markdown-preserving policies store
	// content verbatim.
	dangerousMarkdownURLRegex = regexp.MustCompile(`(?i)(!?\[[^\]]*\])\(\s*(javascript|vbscript|data)\s*:(?:\([^)]*\)|[^)])*\)`)
)

// stripAndCap is the common path for PlainTextField + ShortIdentifier:
// decode entities, strip every HTML tag, preserve safe decoded prose,
// trim whitespace, length-cap by rune count.
func stripAndCap(input string, maxRunes int) string {
	if input == "" {
		return input
	}
	s := sanitizeDecoded(input, strictPolicy)
	s = strings.TrimSpace(s)
	if maxRunes > 0 && utf8.RuneCountInString(s) > maxRunes {
		s = string([]rune(s)[:maxRunes])
	}
	return s
}

// Length caps — see the package doc + per-policy comments for rationale.
const (
	// PlainTextFieldMaxRunes bounds titles + names. 256 runes keeps
	// pathological lengths from breaking board cards / breadcrumbs /
	// picker chips that render these fields verbatim.
	PlainTextFieldMaxRunes = 256
	// ShortIdentifierMaxRunes bounds identifier-shaped values
	// (asset_tag, slug, link-type name). Tighter on purpose — these
	// aren't free-form titles.
	ShortIdentifierMaxRunes = 100
	// LongTextMaxBytes is the unified upper bound on any long-form
	// user-supplied text (descriptions, page bodies, comments). One
	// number, one place to evolve it.
	LongTextMaxBytes = 256 * 1024
)

func plainTextField(s string) string  { return stripAndCap(s, PlainTextFieldMaxRunes) }
func shortIdentifier(s string) string { return stripAndCap(s, ShortIdentifierMaxRunes) }

// preserveAndCap is the shared path for the Markdown-preserving policies
// (RichText, LongDocument, Comment). These fields hold user-authored
// Markdown that is neutralized at RENDER time — client-side DOMPurify
// (render-markdown.js), the Milkdown editor + its link sanitizer, or Go
// html/template auto-escape — and is never emitted as raw HTML on the
// server (template.HTML appears nowhere in the tree). The server therefore
// stores the content byte-for-byte and applies exactly one transform: a
// hard byte cap, the primary request-size defense. See INFRA-28.
//
// The former decode-then-HTML-strip pipeline (unescapeRepeated +
// bluemonday) silently deleted inline-code spans such as `<port>` and
// literal entities (INFRA-12) and mis-capped restored content
// (INFRA-25); it is gone. Empty input caps to "" naturally.
func preserveAndCap(input string, maxBytes int) string {
	return capBytes(input, maxBytes)
}

// capBytes truncates s to at most maxBytes bytes without splitting a
// UTF-8 rune: if the cut lands inside a multi-byte rune it backs off to
// that rune's start byte, so the result is always <= maxBytes and valid
// UTF-8 (given valid input). maxBytes <= 0 disables the cap. This is the
// only mutation the Markdown-preserving policies perform.
func capBytes(s string, maxBytes int) string {
	if maxBytes <= 0 || len(s) <= maxBytes {
		return s
	}
	end := maxBytes
	// A continuation byte has the top bits 10xxxxxx (0x80..0xBF). Walk
	// left off any continuation bytes to the start of the straddling rune.
	for end > 0 && s[end]&0xC0 == 0x80 {
		end--
	}
	return s[:end]
}

func richText(s string) string      { return preserveAndCap(s, LongTextMaxBytes) }
func longDocument(s string) string  { return preserveAndCap(s, LongTextMaxBytes) }
func commentPolicy(s string) string { return preserveAndCap(s, LongTextMaxBytes) }

// sanitizeDecoded fully decodes HTML entities before sanitizing so nested
// entity payloads (for example "&amp;lt;img ...&amp;gt;") cannot survive as
// encoded markup that a later decode could reanimate. After sanitizing, it
// decodes only when doing so is stable under the same policy; that preserves
// legit prose like "5 < 6 > 4" without turning escaped tags back into HTML.
func sanitizeDecoded(input string, policy *bluemonday.Policy) string {
	s := unescapeRepeated(input)
	s = policy.Sanitize(s)

	decoded := html.UnescapeString(s)
	if policy.Sanitize(decoded) == s {
		return decoded
	}
	return s
}

func unescapeRepeated(s string) string {
	for i := 0; i < 8; i++ {
		u := html.UnescapeString(s)
		if u == s {
			return s
		}
		s = u
	}
	return s
}

func markdownURLOnly(s string) string {
	if s == "" {
		return ""
	}
	return dangerousMarkdownURLRegex.ReplaceAllString(s, "${1}(#unsafe-link-removed)")
}
