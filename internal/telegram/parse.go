package telegram

import "strings"

// parseCommand splits a raw bot message into a command and its arguments.
// Commands are lower-cased; quoted arguments keep their inner spaces intact.
func parseCommand(text string) (cmd string, args []string) {
	fields := tokenize(text)
	if len(fields) == 0 {
		return "", nil
	}
	cmd = strings.ToLower(fields[0])
	args = fields[1:]
	return cmd, args
}

// tokenize is a small shell-like splitter that respects single and double
// quotes, so names with spaces (rare) and reasons with spaces survive.
func tokenize(s string) []string {
	var out []string
	var cur []rune
	inSingle := false
	inDouble := false
	flush := func() {
		if len(cur) > 0 {
			out = append(out, string(cur))
			cur = nil
		}
	}
	for _, r := range s {
		switch {
		case r == '\'' && !inDouble:
			inSingle = !inSingle
		case r == '"' && !inSingle:
			inDouble = !inDouble
		case (r == ' ' || r == '\t' || r == '\n') && !inSingle && !inDouble:
			flush()
		default:
			cur = append(cur, r)
		}
	}
	flush()
	return out
}
