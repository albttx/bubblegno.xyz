package ingest

import (
	"strconv"
	"strings"
	"unicode"
)

// ParseCoin parses "1000000ugnot" (first coin only if comma-separated).
func ParseCoin(s string) (amount int64, denom string) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, ""
	}
	if i := strings.IndexByte(s, ','); i >= 0 {
		s = s[:i]
	}
	i := 0
	for i < len(s) && unicode.IsDigit(rune(s[i])) {
		i++
	}
	if i == 0 {
		return 0, s
	}
	n, err := strconv.ParseInt(s[:i], 10, 64)
	if err != nil {
		return 0, s[i:]
	}
	return n, s[i:]
}
