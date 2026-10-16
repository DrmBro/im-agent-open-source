package bridge

import "strings"

// splitMarkdown先保持块结构，再处理超长块；仅用于新生成的展示分片。
// 不重写已入账分片，因此原消息ID、顺序和未知送达保护不变。
func splitMarkdown(text string, limit int) []string {
	if limit < 120 {
		limit = 120
	}
	lines := strings.SplitAfter(text, "\n")
	var parts []string
	var pending string
	flush := func() {
		if pending != "" {
			parts = append(parts, pending)
			pending = ""
		}
	}
	add := func(block string) {
		if len([]rune(pending))+len([]rune(block)) > limit {
			flush()
		}
		pending += block
	}
	for i := 0; i < len(lines); {
		start := i
		fence, info := markdownFence(lines[i])
		if fence != "" {
			i++
			for i < len(lines) && !markdownFenceEnd(lines[i], fence) {
				i++
			}
			inner := strings.Join(lines[start+1:i], "")
			if i < len(lines) {
				i++
			}
			// 每片都有闭合围栏；原回复未闭合时也避免后续标签落进代码块。
			opening, closing := fence+info+"\n", "\n"+fence+"\n"
			block := opening + inner + closing
			if len([]rune(block)) <= limit {
				add(block)
				continue
			}
			budget := limit - len([]rune(opening)) - len([]rune(closing))
			if budget < 32 {
				for _, piece := range literalMarkdown(strings.Join(lines[start:i], ""), limit) {
					flush()
					add(piece)
					flush()
				}
				continue
			}
			flush()
			for _, piece := range splitDisplayLines(inner, budget) {
				parts = append(parts, opening+piece+closing)
			}
			continue
		}
		if i+1 < len(lines) && strings.Contains(lines[i], "|") && markdownTableDivider(lines[i+1]) {
			i += 2
			for i < len(lines) && strings.TrimSpace(lines[i]) != "" && strings.Contains(lines[i], "|") {
				i++
			}
			header := lines[start] + lines[start+1]
			block := "\n" + strings.Join(lines[start:i], "")
			if len([]rune(block))+2 <= limit {
				add(block + "\n\n")
				continue
			}
			valid := len([]rune(header))+2 < limit
			for _, row := range lines[start+2 : i] {
				if len([]rune(header+row))+2 > limit {
					valid = false
				}
			}
			if !valid {
				for _, piece := range literalMarkdown(block, limit) {
					flush()
					add(piece)
					flush()
				}
				continue
			}
			flush()
			table := header
			for _, row := range lines[start+2 : i] {
				if len([]rune(table+row))+2 > limit {
					parts = append(parts, table+"\n\n")
					table = header
				}
				table += row
			}
			parts = append(parts, table+"\n\n")
			continue
		}
		// 普通段落优先整体保留，避免将强调或链接直接按字符劈开。
		i++
		for i < len(lines) && strings.TrimSpace(lines[i-1]) != "" {
			if f, _ := markdownFence(lines[i]); f != "" {
				break
			}
			if i+1 < len(lines) && strings.Contains(lines[i], "|") && markdownTableDivider(lines[i+1]) {
				break
			}
			i++
		}
		block := strings.Join(lines[start:i], "")
		if len([]rune(block)) <= limit {
			add(block)
			continue
		}
		// 单个段落本身过长：按字面转义展示，避免截断链接、HTML或内联代码后误渲染。
		flush()
		parts = append(parts, literalMarkdown(block, limit)...)
	}
	flush()
	return parts
}

func markdownFence(line string) (string, string) {
	line = strings.TrimRight(line, "\r\n")
	indent := len(line) - len(strings.TrimLeft(line, " "))
	if indent > 3 {
		return "", ""
	}
	line = line[indent:]
	if len(line) < 3 || line[0] != '`' && line[0] != '~' {
		return "", ""
	}
	n := 0
	for n < len(line) && line[n] == line[0] {
		n++
	}
	if n < 3 || line[0] == '`' && strings.Contains(line[n:], "`") {
		return "", ""
	}
	return line[:n], line[n:]
}
func markdownFenceEnd(line, opening string) bool {
	fence, tail := markdownFence(line)
	return len(fence) >= len(opening) && fence[0] == opening[0] && strings.TrimSpace(tail) == ""
}
func markdownTableDivider(line string) bool {
	line = strings.TrimSpace(line)
	if !strings.Contains(line, "|") {
		return false
	}
	cells := strings.Split(strings.Trim(line, "|"), "|")
	for _, cell := range cells {
		cell = strings.Trim(strings.TrimSpace(cell), ":")
		if len(cell) < 3 || strings.Trim(cell, "-") != "" {
			return false
		}
	}
	return len(cells) > 0
}
func splitDisplayLines(text string, limit int) []string {
	var out []string
	pending := ""
	for _, line := range strings.SplitAfter(text, "\n") {
		if len([]rune(pending+line)) > limit && pending != "" {
			out = append(out, pending)
			pending = ""
		}
		chars := []rune(line)
		for len(chars) > limit {
			out = append(out, string(chars[:limit]))
			chars = chars[limit:]
		}
		pending += string(chars)
	}
	if pending != "" {
		out = append(out, pending)
	}
	return out
}
func literalMarkdown(text string, limit int) []string {
	var out []string
	var b strings.Builder
	size := 0
	for _, r := range text {
		token := string(r)
		switch r {
		case '&':
			token = "&amp;"
		case '<':
			token = "&lt;"
		case '>':
			token = "&gt;"
		default:
			if strings.ContainsRune("\\`*_{}[]()#+-.!|~", r) {
				token = "\\" + token
			}
		}
		n := len([]rune(token))
		if size+n > limit {
			out = append(out, b.String())
			b.Reset()
			size = 0
		}
		b.WriteString(token)
		size += n
	}
	if b.Len() > 0 {
		out = append(out, b.String())
	}
	return out
}
