package links

import "strings"

// SemanticBody replaces relative Markdown links with their visible label.
// Link destinations are navigation metadata; labels and surrounding prose
// remain part of the document's semantic content.
func SemanticBody(body string) string {
	var out strings.Builder
	fenced := false
	fence := ""
	for _, line := range strings.SplitAfter(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			mark := trimmed[:3]
			if !fenced {
				fenced, fence = true, mark
			} else if mark == fence {
				fenced = false
			}
			out.WriteString(line)
			continue
		}
		if fenced {
			out.WriteString(line)
			continue
		}
		out.WriteString(semanticLine(line))
	}
	return out.String()
}

func semanticLine(line string) string {
	blanked := inlineRaw.ReplaceAllStringFunc(line, func(code string) string {
		return strings.Repeat(" ", len(code))
	})
	type splice struct {
		start, end int
		label      string
	}
	var edits []splice
	for _, match := range mdLink.FindAllStringSubmatchIndex(blanked, -1) {
		separator := strings.Index(blanked[match[0]:match[2]], "](")
		if separator < 0 {
			continue
		}
		open := match[0] + separator + 1
		target, close, ok := markdownDestination(blanked, open)
		if !ok || !isRelativeTarget(target) {
			continue
		}
		start := match[0]
		labelStart := match[0] + 1
		labelEnd := match[0] + separator
		if start > 0 && line[start-1] == '!' {
			start--
		}
		edits = append(edits, splice{
			start: start,
			end:   close + 1,
			label: line[labelStart:labelEnd],
		})
	}
	for i := len(edits) - 1; i >= 0; i-- {
		edit := edits[i]
		line = line[:edit.start] + edit.label + line[edit.end:]
	}
	return line
}

func markdownDestination(line string, open int) (string, int, bool) {
	if open < 0 || open >= len(line) || line[open] != '(' {
		return "", 0, false
	}
	depth, quote := 1, byte(0)
	angle := false
	for i := open + 1; i < len(line); i++ {
		ch := line[i]
		if ch == '\\' {
			i++
			continue
		}
		if quote != 0 {
			if ch == quote {
				quote = 0
			}
			continue
		}
		if (ch == '"' || ch == '\'') && !angle {
			quote = ch
			continue
		}
		switch ch {
		case '<':
			angle = true
		case '>':
			angle = false
		case '(':
			if !angle {
				depth++
			}
		case ')':
			if angle {
				continue
			}
			depth--
			if depth == 0 {
				target := strings.TrimSpace(line[open+1 : i])
				if target == "" {
					return "", i, true
				}
				if target[0] == '<' {
					end := strings.IndexByte(target[1:], '>')
					if end < 0 {
						return "", i, false
					}
					target = target[1 : end+1]
				} else {
					target = strings.Fields(target)[0]
				}
				return target, i, true
			}
		}
	}
	return "", 0, false
}

func isRelativeTarget(target string) bool {
	return target == "" || (!strings.Contains(target, "://") &&
		!strings.HasPrefix(target, "mailto:") && !strings.HasPrefix(target, "/"))
}
