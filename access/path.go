package access

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Do not clean a path into an allowed form. Reject ambiguous input before any
// backend call. Percent is rejected even in JSON to avoid double-decoding aliases.
func validPath(name string) bool {
	if len(name) == 0 || len(name) > 4096 || !utf8.ValidString(name) {
		return false
	}
	parts := strings.Split(name, "/")
	if len(parts) > 64 {
		return false
	}
	for _, part := range parts {
		if !validComponent(part) {
			return false
		}
	}
	return true
}
func validShare(name string) bool {
	return len(name) <= 255 && !strings.Contains(name, "/") && validComponent(name)
}
func validComponent(s string) bool {
	if !utf8.ValidString(s) || s == "" || s == "." || s == ".." || strings.HasSuffix(s, ".") || strings.TrimSpace(s) != s {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) || strings.ContainsRune(`\:*?"<>|%`, r) {
			return false
		}
	}
	base, _, _ := strings.Cut(strings.ToUpper(s), ".")
	switch base {
	case "CON", "PRN", "AUX", "NUL", "CONIN$", "CONOUT$":
		return false
	}
	if len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) && base[3] >= '0' && base[3] <= '9' {
		return false
	}
	// Win32 also recognizes superscript digits in COM/LPT device aliases.
	if strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT") {
		if strings.ContainsAny(strings.TrimPrefix(strings.TrimPrefix(base, "COM"), "LPT"), "¹²³") {
			return false
		}
	}
	return true
}
