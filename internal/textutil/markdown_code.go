package textutil

import "strings"

// MapOutsideCode applies f to Markdown prose while leaving fenced and inline
// code byte-identical.
//
// Fence detection is deliberately literal rather than a CommonMark parse: a line
// whose first non-space run is ``` or ~~~ toggles a block of that character. An
// unterminated fence swallows the rest of the string — the safe direction, since
// unstripped prose merely shows a stray [1] while stripped code is broken code.
//
// f must not add or remove newlines.
func MapOutsideCode(s string, f func(string) string) string {
	if s == "" {
		return s
	}

	var out strings.Builder
	out.Grow(len(s))

	var prose []string
	flush := func() {
		if len(prose) == 0 {
			return
		}
		out.WriteString(mapInlineCode(strings.Join(prose, ""), f))
		prose = prose[:0]
	}

	inFence := false
	fenceChar := byte(0)

	for _, line := range splitKeepingNewlines(s) {
		body := strings.TrimRight(line, "\n")

		if c, ok := fenceMarker(body); ok && (!inFence || c == fenceChar) {
			flush()
			out.WriteString(line)
			inFence = !inFence
			if inFence {
				fenceChar = c
			} else {
				fenceChar = 0
			}
			continue
		}
		if inFence {
			flush()
			out.WriteString(line)
			continue
		}
		prose = append(prose, line)
	}
	flush()
	return out.String()
}

// splitKeepingNewlines splits into lines that still carry their own trailing
// "\n". Concatenating the result reproduces the input exactly, which is what
// lets the code passthrough be byte-identical.
func splitKeepingNewlines(s string) []string {
	var out []string
	for {
		i := strings.IndexByte(s, '\n')
		if i < 0 {
			if s != "" {
				out = append(out, s)
			}
			return out
		}
		out = append(out, s[:i+1])
		s = s[i+1:]
	}
}

// fenceMarker reports whether the line's first non-space content is a run of at
// least three backticks or tildes, and which character it was.
func fenceMarker(line string) (byte, bool) {
	t := strings.TrimLeft(line, " \t")
	if strings.HasPrefix(t, "```") {
		return '`', true
	}
	if strings.HasPrefix(t, "~~~") {
		return '~', true
	}
	return 0, false
}

// mapInlineCode applies f to a prose run while preserving inline `code` spans.
// Backticks are paired left to right within the run. An unpaired backtick is
// treated as ordinary prose: a stray tick in natural text must not cause the
// rest of the answer to keep its citation markers.
func mapInlineCode(chunk string, f func(string) string) string {
	if !strings.Contains(chunk, "`") {
		return f(chunk)
	}

	var out strings.Builder
	out.Grow(len(chunk))
	rest := chunk
	for {
		open := strings.IndexByte(rest, '`')
		if open < 0 {
			out.WriteString(f(rest))
			return out.String()
		}
		closing := strings.IndexByte(rest[open+1:], '`')
		if closing < 0 {
			out.WriteString(f(rest))
			return out.String()
		}
		closing += open + 1

		out.WriteString(f(rest[:open]))
		out.WriteString(rest[open : closing+1]) // the span, backticks included
		rest = rest[closing+1:]
	}
}
