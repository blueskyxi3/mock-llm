package server

import (
	"strings"
	"unicode"
)

// TokenUnits preserves UTF-8 and whitespace. This is a deterministic synthetic
// estimator, not a vendor tokenizer. ASCII alphanumeric runs are split at 4.
func TokenUnits(s string) []string {
	r := []rune(s)
	units := []string{}
	space := ""
	for i := 0; i < len(r); {
		if unicode.IsSpace(r[i]) {
			space += string(r[i])
			i++
			continue
		}
		start := i
		i++
		if asciiWord(r[start]) {
			for i < len(r) && i-start < 4 && asciiWord(r[i]) {
				i++
			}
		}
		units = append(units, space+string(r[start:i]))
		space = ""
	}
	if space != "" && len(units) > 0 {
		units[len(units)-1] += space
	}
	return units
}
func asciiWord(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
}
func CountTokens(s string) int {
	count, asciiRun := 0, 0
	for _, r := range s {
		if asciiWord(r) {
			if asciiRun%4 == 0 {
				count++
			}
			asciiRun++
			continue
		}
		asciiRun = 0
		if !unicode.IsSpace(r) {
			count++
		}
	}
	return count
}
func outputChunk(units []string, start, end int) string {
	var b strings.Builder
	for i := start; i < end; i++ {
		if i > 0 && i%len(units) == 0 {
			b.WriteByte('\n')
		}
		b.WriteString(units[i%len(units)])
	}
	return b.String()
}
