package main

// rc and tokenize(2) quoting

import "strings"

func firstRune(s string) string {
	for _, r := range s {
		return string(r)
	}
	return ""
}

// tokenize splits a line as tokenize(2) does: blanks between fields, single
// quotes around a field, a quote doubled inside.
func tokenize(s string) []string {
	var f []string
	var cur strings.Builder
	in, have := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\'' && in && i+1 < len(s) && s[i+1] == '\'':
			cur.WriteByte('\'')
			i++
		case c == '\'':
			in = !in
			have = true
		case (c == ' ' || c == '\t') && !in:
			if have {
				f = append(f, cur.String())
				cur.Reset()
				have = false
			}
		default:
			cur.WriteByte(c)
			have = true
		}
	}
	if have {
		f = append(f, cur.String())
	}
	return f
}

func rcQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

func unquote(s string) string {
	if len(s) >= 2 && s[0] == '\'' && s[len(s)-1] == '\'' {
		return strings.ReplaceAll(s[1:len(s)-1], "''", "'")
	}
	return s
}
