// Field paths, identifiers and collection names: the syntactic rules the doc
// sets, and the segment parser both engines build their queries from. A
// path that passes IsPath contains only letters, digits, "_", "-", "." and
// "[]", which is what lets the MySQL builder inline a JSON path literal and
// the Mongo builder use it as a key without either ever quoting user text.
package protocol

import (
	"regexp"
	"strings"
)

const segmentPattern = `[A-Za-z_][A-Za-z0-9_\-]{0,63}`

var (
	pathRe       = regexp.MustCompile(`^` + segmentPattern + `(\[\])?(\.` + segmentPattern + `(\[\])?){0,7}$`)
	segmentRe    = regexp.MustCompile(`^` + segmentPattern + `$`)
	identifierRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)
	collectionRe = regexp.MustCompile(`^[^$. ][^. ]{0,119}$`)
	tokenRe      = regexp.MustCompile(`^fp_live_[A-Za-z0-9_-]{43}$`)
)

// IsPath reports whether s is a dotted field path: segments of letters,
// digits, underscore and dash, each optionally suffixed with "[]" for arrays,
// at most eight segments.
func IsPath(s string) bool { return pathRe.MatchString(s) }

// IsSegment reports whether s is a single path segment without "[]".
func IsSegment(s string) bool { return segmentRe.MatchString(s) }

// IsIdentifier is the MySQL rule for table and column names
// (^[A-Za-z_][A-Za-z0-9_]{0,63}$). Anything else is rejected before it can
// be quoted; that, plus backtick quoting, is the whole identifier story.
func IsIdentifier(s string) bool { return identifierRe.MatchString(s) }

// IsCollectionName accepts what MongoDB accepts (no leading "$", no "." or
// space, no NUL, at most 120 bytes) minus the system.* namespace. MySQL
// names are checked again with IsIdentifier by the MySQL engine.
func IsCollectionName(s string) bool {
	if strings.HasPrefix(s, "system.") || strings.ContainsRune(s, 0) {
		return false
	}
	return collectionRe.MatchString(s)
}

// IsToken reports whether s has the shape of a platform token.
func IsToken(s string) bool { return tokenRe.MatchString(s) }

// Segment is one step of a field path. Array is true when the segment was
// written "name[]", meaning "each element of the array at name".
type Segment struct {
	Name  string
	Array bool
}

// String renders the segment back to path syntax.
func (s Segment) String() string {
	if s.Array {
		return s.Name + "[]"
	}
	return s.Name
}

// ParsePath splits a validated path into its segments. It does not validate;
// call IsPath first.
func ParsePath(p string) []Segment {
	parts := strings.Split(p, ".")
	segs := make([]Segment, 0, len(parts))
	for _, part := range parts {
		seg := Segment{Name: part}
		if strings.HasSuffix(part, "[]") {
			seg.Name = strings.TrimSuffix(part, "[]")
			seg.Array = true
		}
		segs = append(segs, seg)
	}
	return segs
}

// StripArrays returns the path without any "[]" markers, which is the form
// MongoDB wants in filters, sorts and projections (it traverses arrays on
// its own).
func StripArrays(p string) string { return strings.ReplaceAll(p, "[]", "") }

// MetricKey is the output key of a metric: its "as" when given, otherwise
// "count" for count and "fn_field" with dots (and dashes, brackets) turned
// into underscores for the rest.
func MetricKey(m Metric) string {
	if m.As != "" {
		return m.As
	}
	if m.Fn == "count" {
		return "count"
	}
	field := strings.NewReplacer(".", "_", "-", "_", "[]", "").Replace(m.Field)
	return m.Fn + "_" + field
}
