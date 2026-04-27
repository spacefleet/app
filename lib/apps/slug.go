package apps

import (
	"strconv"
	"strings"
	"unicode"
)

// SlugMaxLen is the maximum length of an app slug. 50 keeps URLs
// readable and prevents any single field from dominating the
// (org_slug, slug) unique index.
const SlugMaxLen = 50

// reserved is the set of slugs we refuse outright. They'd otherwise
// collide with route segments in the SPA (`/:org/apps/new`,
// `/:org/apps/settings`) or with future API conventions. The set
// is small and explicit on purpose — an enum we can audit.
var reserved = map[string]struct{}{
	"new":      {},
	"settings": {},
	"builds":   {},
	"delete":   {},
	"api":      {},
	"admin":    {},
}

// IsReserved reports whether s is one of the slugs we refuse.
func IsReserved(s string) bool {
	_, ok := reserved[s]
	return ok
}

// BaseSlug normalizes name into a URL-safe slug:
//   - lowercase
//   - characters outside [a-z0-9-] become '-'
//   - runs of '-' collapse to one
//   - leading/trailing '-' trimmed
//   - truncated to SlugMaxLen
//
// Returns "" if the name has no slug-worthy characters at all
// (e.g. all punctuation, or empty); the caller decides what to do
// with that — typically reject as a bad-input error.
func BaseSlug(name string) string {
	var b strings.Builder
	b.Grow(len(name))
	prevDash := false
	for _, r := range name {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			lr := unicode.ToLower(r)
			// Only ASCII letters/digits land in the slug — anything
			// else (accented letters, kanji, etc.) becomes a dash so
			// we don't end up with mixed-script slugs that breaks
			// URL stability.
			if lr <= 0x7f && (isASCIIAlnum(byte(lr))) {
				b.WriteByte(byte(lr))
				prevDash = false
			} else if !prevDash {
				b.WriteByte('-')
				prevDash = true
			}
		default:
			if !prevDash {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > SlugMaxLen {
		out = strings.TrimRight(out[:SlugMaxLen], "-")
	}
	return out
}

func isASCIIAlnum(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
}

// SlugWithSuffix returns base, base-2, base-3, ... clamped so the
// suffix never pushes the slug past SlugMaxLen. n=1 returns base
// unchanged (the no-collision case).
//
// Truncation logic: when base+"-N" would overflow, we trim base to
// fit. This guarantees the result stays within SlugMaxLen and
// preserves as much of the original as possible. Trailing dashes
// produced by truncation get trimmed so we don't end up with
// "abc--3".
func SlugWithSuffix(base string, n int) string {
	if n <= 1 {
		return base
	}
	suffix := "-" + strconv.Itoa(n)
	if len(base)+len(suffix) <= SlugMaxLen {
		return base + suffix
	}
	keep := SlugMaxLen - len(suffix)
	if keep <= 0 {
		// Pathological: collision count alone >= SlugMaxLen-1
		// chars. Fall back to just the suffix without the dash so
		// we still produce something. In practice we'll never get
		// near this — it'd take ~1e48 collisions.
		return strings.TrimLeft(suffix, "-")
	}
	return strings.TrimRight(base[:keep], "-") + suffix
}
