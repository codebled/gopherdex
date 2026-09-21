// Package module validates, escapes and orders Go module paths and versions
// the way the GOPROXY protocol expects (see `go help goproxy`).
package module

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

var (
	// ErrNotFound reports that a module or version does not exist in a
	// source. Proxy handlers turn it into 404 so the go command can fall
	// through to the next entry in GOPROXY.
	ErrNotFound = errors.New("not found")

	// ErrInvalid reports a malformed module path or version.
	ErrInvalid = errors.New("invalid module path or version")
)

// Info is the JSON object served at @v/<version>.info and @latest.
type Info struct {
	Version string    `json:"Version"`
	Time    time.Time `json:"Time"`
	Origin  *Origin   `json:"Origin,omitempty"`
}

// Origin records where a version came from. The go command accepts it next
// to Version and Time; Hash is the commit the version was tagged at.
type Origin struct {
	VCS  string `json:"VCS,omitempty"`
	URL  string `json:"URL,omitempty"`
	Hash string `json:"Hash,omitempty"`
	Ref  string `json:"Ref,omitempty"`
}

const maxPathLen = 512

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// CheckPath reports whether path is a well-formed module path: slash
// separated elements of letters, digits and "-._~", where the first element
// is a domain name containing a dot.
func CheckPath(path string) error {
	if path == "" {
		return invalid("module path is empty")
	}
	if len(path) > maxPathLen {
		return invalid("module path is longer than %d bytes", maxPathLen)
	}
	if strings.HasPrefix(path, "/") || strings.HasSuffix(path, "/") {
		return invalid("module path %q has a leading or trailing slash", path)
	}
	for i, elem := range strings.Split(path, "/") {
		if elem == "" {
			return invalid("module path %q has an empty element", path)
		}
		if elem[0] == '.' || elem[len(elem)-1] == '.' {
			return invalid("module path %q: element %q begins or ends with a dot", path, elem)
		}
		for _, r := range elem {
			if !isPathChar(r) {
				return invalid("module path %q: element %q contains %q", path, elem, r)
			}
		}
		if i == 0 && (!strings.Contains(elem, ".") || elem[0] == '-') {
			return invalid("module path %q must start with a domain name such as example.com", path)
		}
	}
	return nil
}

func isPathChar(r rune) bool {
	return 'a' <= r && r <= 'z' || 'A' <= r && r <= 'Z' || '0' <= r && r <= '9' ||
		r == '-' || r == '.' || r == '_' || r == '~'
}

// CheckVersion reports whether v is a canonical semantic version:
// vMAJOR.MINOR.PATCH with an optional -PRERELEASE and optional +incompatible.
func CheckVersion(v string) error {
	if _, ok := parse(v); !ok {
		return invalid("version %q is not canonical semver (want vMAJOR.MINOR.PATCH[-PRERELEASE])", v)
	}
	return nil
}

// EscapePath encodes path for use in proxy URLs and on case-insensitive file
// systems: each upper-case letter becomes "!" followed by its lower-case form.
func EscapePath(path string) (string, error) {
	if err := CheckPath(path); err != nil {
		return "", err
	}
	return escape(path), nil
}

// UnescapePath reverses EscapePath and validates the result.
func UnescapePath(escaped string) (string, error) {
	path, err := unescape(escaped)
	if err != nil {
		return "", err
	}
	if err := CheckPath(path); err != nil {
		return "", err
	}
	return path, nil
}

// EscapeVersion encodes a version the same way as EscapePath.
func EscapeVersion(v string) (string, error) {
	if err := CheckVersion(v); err != nil {
		return "", err
	}
	return escape(v), nil
}

// UnescapeVersion reverses EscapeVersion and validates the result.
func UnescapeVersion(escaped string) (string, error) {
	v, err := unescape(escaped)
	if err != nil {
		return "", err
	}
	if err := CheckVersion(v); err != nil {
		return "", err
	}
	return v, nil
}

func escape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if 'A' <= c && c <= 'Z' {
			b.WriteByte('!')
			c += 'a' - 'A'
		}
		b.WriteByte(c)
	}
	return b.String()
}

func unescape(s string) (string, error) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '!':
			if i+1 == len(s) || s[i+1] < 'a' || s[i+1] > 'z' {
				return "", invalid("%q has an invalid \"!\" escape", s)
			}
			i++
			b.WriteByte(s[i] - 'a' + 'A')
		case 'A' <= c && c <= 'Z':
			return "", invalid("%q has an unescaped upper-case letter", s)
		default:
			b.WriteByte(c)
		}
	}
	return b.String(), nil
}

type semver struct {
	major, minor, patch, prerelease string
}

func parse(v string) (semver, bool) {
	var p semver
	rest, ok := strings.CutPrefix(v, "v")
	if !ok {
		return p, false
	}
	if i := strings.IndexByte(rest, '+'); i >= 0 {
		if rest[i:] != "+incompatible" {
			return p, false
		}
		rest = rest[:i]
	}
	if i := strings.IndexByte(rest, '-'); i >= 0 {
		p.prerelease = rest[i+1:]
		rest = rest[:i]
		if !validPrerelease(p.prerelease) {
			return p, false
		}
	}
	parts := strings.Split(rest, ".")
	if len(parts) != 3 {
		return p, false
	}
	for _, n := range parts {
		if !isNum(n) || (len(n) > 1 && n[0] == '0') {
			return p, false
		}
	}
	p.major, p.minor, p.patch = parts[0], parts[1], parts[2]
	return p, true
}

func isNum(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func validPrerelease(s string) bool {
	if s == "" {
		return false
	}
	for _, id := range strings.Split(s, ".") {
		if id == "" {
			return false
		}
		for i := 0; i < len(id); i++ {
			c := id[i]
			if !('0' <= c && c <= '9' || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || c == '-') {
				return false
			}
		}
		if isNum(id) && len(id) > 1 && id[0] == '0' {
			return false
		}
	}
	return true
}

// Compare orders two versions by semver precedence. Invalid versions sort
// before valid ones.
func Compare(a, b string) int {
	pa, okA := parse(a)
	pb, okB := parse(b)
	switch {
	case !okA && !okB:
		return strings.Compare(a, b)
	case !okA:
		return -1
	case !okB:
		return 1
	}
	if c := compareNum(pa.major, pb.major); c != 0 {
		return c
	}
	if c := compareNum(pa.minor, pb.minor); c != 0 {
		return c
	}
	if c := compareNum(pa.patch, pb.patch); c != 0 {
		return c
	}
	return comparePrerelease(pa.prerelease, pb.prerelease)
}

// compareNum compares decimal strings without leading zeros, so it never
// overflows on huge version numbers.
func compareNum(a, b string) int {
	if len(a) != len(b) {
		return cmp.Compare(len(a), len(b))
	}
	return strings.Compare(a, b)
}

func comparePrerelease(a, b string) int {
	switch {
	case a == b:
		return 0
	case a == "":
		return 1 // a release outranks any of its pre-releases
	case b == "":
		return -1
	}
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) && i < len(bs); i++ {
		x, y := as[i], bs[i]
		if x == y {
			continue
		}
		xn, yn := isNum(x), isNum(y)
		switch {
		case xn && yn:
			return compareNum(x, y)
		case xn:
			return -1
		case yn:
			return 1
		default:
			return strings.Compare(x, y)
		}
	}
	return cmp.Compare(len(as), len(bs))
}

// IsPrerelease reports whether v is a valid version with a pre-release
// suffix. Pseudo-versions count as pre-releases.
func IsPrerelease(v string) bool {
	p, ok := parse(v)
	return ok && p.prerelease != ""
}

// Sort orders versions from lowest to highest.
func Sort(versions []string) {
	slices.SortFunc(versions, Compare)
}

// Latest returns the highest release, or the highest pre-release when there
// are no releases, mirroring how the go command resolves @latest.
func Latest(versions []string) string {
	latest := ""
	for _, v := range versions {
		if _, ok := parse(v); !ok {
			continue
		}
		switch {
		case latest == "":
			latest = v
		case IsPrerelease(latest) && !IsPrerelease(v):
			latest = v
		case IsPrerelease(latest) == IsPrerelease(v) && Compare(v, latest) > 0:
			latest = v
		}
	}
	return latest
}
