// Package yamlite decodes a strict subset of YAML 1.2 into Go values using
// only the standard library.
//
// Supported: block mappings and sequences (including "- key: value" items),
// flow collections ([a, b] and {k: v}) that may span several lines, plain,
// single-quoted and double-quoted scalars, comments, and a leading "---".
//
// Rejected with an error, never guessed: anchors, aliases, tags, block
// scalars (| and >), complex keys, several documents, tabs in indentation,
// multi-line plain scalars and duplicate keys. Decoding is type-directed:
// a scalar decoded into a string field keeps its exact text, so `001` stays
// "001" and `99.9` stays "99.9"; unknown fields are rejected.
package yamlite

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Error reports a problem at a line of the input.
type Error struct {
	Line int
	Msg  string
}

func (e *Error) Error() string {
	if e.Line <= 0 {
		return e.Msg
	}
	return fmt.Sprintf("line %d: %s", e.Line, e.Msg)
}

func errAt(line int, format string, a ...any) error {
	return &Error{Line: line, Msg: fmt.Sprintf(format, a...)}
}

type kind int

const (
	nullNode kind = iota
	scalarNode
	mapNode
	seqNode
)

type node struct {
	kind   kind
	line   int
	text   string // scalar text (decoded for quoted scalars)
	quoted bool
	keys   []string
	vals   []*node
	items  []*node
}

func (n *node) describe() string {
	switch n.kind {
	case nullNode:
		return "an empty value"
	case scalarNode:
		return fmt.Sprintf("the scalar %q", n.text)
	case mapNode:
		return "a mapping"
	default:
		return "a sequence"
	}
}

type srcLine struct {
	no     int
	indent int
	text   string // content after the indentation, comment removed, right-trimmed
}

type parser struct {
	lines []srcLine
	pos   int
	depth int
}

func (p *parser) enter(line int) error {
	p.depth++
	if p.depth > maxDepth {
		return errAt(line, "blocks nested more than %d deep", maxDepth)
	}
	return nil
}

// parse turns YAML text into a node tree.
func parse(src []byte) (*node, error) {
	if !utf8.Valid(src) {
		return nil, &Error{Msg: "input is not valid UTF-8"}
	}
	lines, err := splitLines(string(src))
	if err != nil {
		return nil, err
	}
	if len(lines) == 0 {
		return &node{kind: nullNode, line: 1}, nil
	}
	p := &parser{lines: lines}
	root, err := p.parseBlock(lines[0].indent)
	if err != nil {
		return nil, err
	}
	if p.pos < len(p.lines) {
		return nil, errAt(p.lines[p.pos].no, "unexpected content (check the indentation)")
	}
	return root, nil
}

func splitLines(s string) ([]srcLine, error) {
	s = strings.TrimPrefix(s, "\ufeff")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	var out []srcLine
	sawContent := false
	for i, raw := range strings.Split(s, "\n") {
		no := i + 1
		j := 0
		hasTab := false
		for j < len(raw) && (raw[j] == ' ' || raw[j] == '\t') {
			if raw[j] == '\t' {
				hasTab = true
			}
			j++
		}
		content := strings.TrimRight(stripComment(raw[j:]), " \t")
		if content == "" {
			continue
		}
		if hasTab {
			return nil, errAt(no, "tabs are not allowed in indentation; use spaces")
		}
		if j == 0 && (content == "---" || strings.HasPrefix(content, "--- ")) {
			if sawContent {
				return nil, errAt(no, "several YAML documents in one file are not supported")
			}
			rest := strings.TrimSpace(content[3:])
			if rest == "" {
				continue
			}
			content = rest
		}
		if j == 0 && (content == "..." || strings.HasPrefix(content, "%")) {
			return nil, errAt(no, "YAML directives and document end markers are not supported")
		}
		sawContent = true
		out = append(out, srcLine{no: no, indent: j, text: content})
	}
	return out, nil
}

// stripComment removes a trailing "# comment". A '#' starts a comment only at
// the start of the content or after whitespace, and never inside quotes. A
// quote opens a quoted scalar only where a scalar can start, so apostrophes
// in plain text ("Lab's cluster") do not hide a comment.
func stripComment(s string) string {
	inSingle, inDouble := false, false
	prev := byte(' ')
	lastSignificant := byte(0) // last non-space byte outside quotes; 0 = none yet
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case inDouble:
			if c == '\\' {
				i++
				continue
			}
			if c == '"' {
				inDouble = false
			}
		case inSingle:
			if c == '\'' {
				if i+1 < len(s) && s[i+1] == '\'' {
					i++
					continue
				}
				inSingle = false
			}
		default:
			if c == '#' && (i == 0 || prev == ' ' || prev == '\t') {
				return s[:i]
			}
			if (c == '"' || c == '\'') && canStartScalar(lastSignificant) {
				if c == '"' {
					inDouble = true
				} else {
					inSingle = true
				}
			}
			if c != ' ' && c != '\t' {
				lastSignificant = c
			}
		}
		prev = c
	}
	return s
}

func canStartScalar(prev byte) bool {
	switch prev {
	case 0, ':', '-', '[', '{', ',':
		return true
	}
	return false
}

func isSeqItem(text string) bool { return text == "-" || strings.HasPrefix(text, "- ") }

// splitKey recognises "key: value" and "key:" lines.
func splitKey(text string, line int) (key, rest string, ok bool, err error) {
	if text == "" || text[0] == '[' || text[0] == '{' {
		return "", "", false, nil
	}
	if text[0] == '?' && (len(text) == 1 || text[1] == ' ') {
		return "", "", false, errAt(line, "complex keys (\"? \") are not supported")
	}
	if text[0] == '"' || text[0] == '\'' {
		val, end, qerr := parseQuoted(text, 0, line)
		if qerr != nil {
			return "", "", false, qerr
		}
		after := strings.TrimLeft(text[end:], " ")
		if strings.HasPrefix(after, ":") && (len(after) == 1 || after[1] == ' ') {
			return val, strings.TrimSpace(after[1:]), true, nil
		}
		return "", "", false, nil
	}
	for i := 0; i < len(text); i++ {
		if text[i] == ':' && (i == len(text)-1 || text[i+1] == ' ') {
			key = strings.TrimSpace(text[:i])
			if key == "" {
				return "", "", false, errAt(line, "empty mapping key")
			}
			if strings.ContainsAny(key[:1], "&*!|>%@`") {
				return "", "", false, errAt(line, "unsupported YAML feature in key %q (anchors, aliases, tags and block scalars are not supported)", key)
			}
			return key, strings.TrimSpace(text[i+1:]), true, nil
		}
	}
	return "", "", false, nil
}

func (p *parser) parseBlock(indent int) (*node, error) {
	l := p.lines[p.pos]
	if isSeqItem(l.text) {
		return p.parseSeq(indent)
	}
	if _, _, ok, err := splitKey(l.text, l.no); err != nil {
		return nil, err
	} else if ok {
		return p.parseMap(indent)
	}
	return p.parseInline(l.text, l.no, indent-1)
}

func (p *parser) parseMap(indent int) (*node, error) {
	if err := p.enter(p.lines[p.pos].no); err != nil {
		return nil, err
	}
	defer func() { p.depth-- }()
	m := &node{kind: mapNode, line: p.lines[p.pos].no}
	seen := map[string]int{}
	for p.pos < len(p.lines) {
		l := p.lines[p.pos]
		if l.indent < indent {
			break
		}
		if l.indent > indent {
			return nil, errAt(l.no, "unexpected indentation (multi-line plain scalars are not supported; quote long text on one line)")
		}
		if isSeqItem(l.text) {
			return nil, errAt(l.no, "a sequence item (\"- \") appears where a mapping key was expected")
		}
		key, rest, ok, err := splitKey(l.text, l.no)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, errAt(l.no, "expected \"key: value\", got %q", l.text)
		}
		if first, dup := seen[key]; dup {
			return nil, errAt(l.no, "duplicate key %q (first used on line %d)", key, first)
		}
		seen[key] = l.no
		var val *node
		if rest == "" {
			p.pos++
			switch {
			case p.pos < len(p.lines) && p.lines[p.pos].indent > indent:
				val, err = p.parseBlock(p.lines[p.pos].indent)
			case p.pos < len(p.lines) && p.lines[p.pos].indent == indent && isSeqItem(p.lines[p.pos].text):
				val, err = p.parseSeq(indent)
			default:
				val = &node{kind: nullNode, line: l.no}
			}
		} else {
			val, err = p.parseInline(rest, l.no, indent)
		}
		if err != nil {
			return nil, err
		}
		m.keys = append(m.keys, key)
		m.vals = append(m.vals, val)
	}
	return m, nil
}

func (p *parser) parseSeq(indent int) (*node, error) {
	if err := p.enter(p.lines[p.pos].no); err != nil {
		return nil, err
	}
	defer func() { p.depth-- }()
	s := &node{kind: seqNode, line: p.lines[p.pos].no}
	for p.pos < len(p.lines) {
		l := p.lines[p.pos]
		if l.indent < indent {
			break
		}
		if l.indent > indent {
			return nil, errAt(l.no, "unexpected indentation inside a sequence")
		}
		if !isSeqItem(l.text) {
			break
		}
		itemText := strings.TrimLeft(l.text[1:], " ")
		itemIndent := l.indent + len(l.text) - len(itemText)
		var item *node
		var err error
		switch {
		case itemText == "":
			p.pos++
			if p.pos < len(p.lines) && p.lines[p.pos].indent > indent {
				item, err = p.parseBlock(p.lines[p.pos].indent)
			} else {
				item = &node{kind: nullNode, line: l.no}
			}
		case isSeqItem(itemText):
			return nil, errAt(l.no, "nested inline sequences (\"- - x\") are not supported")
		default:
			_, _, isKey, kerr := splitKey(itemText, l.no)
			if kerr != nil {
				return nil, kerr
			}
			if isKey {
				// "- key: value": a mapping whose first key sits on this line.
				p.lines[p.pos] = srcLine{no: l.no, indent: itemIndent, text: itemText}
				item, err = p.parseMap(itemIndent)
			} else {
				item, err = p.parseInline(itemText, l.no, indent)
			}
		}
		if err != nil {
			return nil, err
		}
		s.items = append(s.items, item)
	}
	return s, nil
}

// parseInline parses a value written on the current line (a scalar or a flow
// collection, which may continue on following lines) and advances past it.
func (p *parser) parseInline(text string, line, parentIndent int) (*node, error) {
	switch text[0] {
	case '[', '{':
		parts := []string{text}
		depth, err := flowDepth(text, line, 0)
		if err != nil {
			return nil, err
		}
		for depth > 0 {
			if p.pos+1 >= len(p.lines) {
				return nil, errAt(line, "flow collection is never closed")
			}
			next := p.lines[p.pos+1]
			if next.indent <= parentIndent {
				return nil, errAt(next.no, "a continued flow collection must be indented more than its key")
			}
			if depth, err = flowDepth(next.text, next.no, depth); err != nil {
				return nil, err
			}
			p.pos++
			parts = append(parts, next.text)
		}
		p.pos++
		f := &flowParser{s: strings.Join(parts, " "), line: line}
		n, err := f.value()
		if err != nil {
			return nil, err
		}
		f.skipSpaces()
		if f.i < len(f.s) {
			return nil, errAt(line, "unexpected text %q after a flow collection", f.s[f.i:])
		}
		return n, nil
	case '"', '\'':
		val, end, err := parseQuoted(text, 0, line)
		if err != nil {
			return nil, err
		}
		if rest := strings.TrimSpace(text[end:]); rest != "" {
			return nil, errAt(line, "unexpected text %q after a quoted scalar", rest)
		}
		p.pos++
		return &node{kind: scalarNode, line: line, text: val, quoted: true}, nil
	case '&', '*', '!', '|', '>', '%', '@', '`':
		return nil, errAt(line, "unsupported YAML feature %q (anchors, aliases, tags and block scalars are not supported)", text[:1])
	}
	if strings.Contains(text, ": ") || strings.HasSuffix(text, ":") {
		return nil, errAt(line, "plain value %q contains \": \"; put it in quotes", text)
	}
	if strings.Contains(text, " #") {
		return nil, errAt(line, "plain value %q contains \" #\"; put it in quotes", text)
	}
	p.pos++
	return plainNode(text, line), nil
}

func plainNode(text string, line int) *node {
	if isNullText(text) {
		return &node{kind: nullNode, line: line}
	}
	return &node{kind: scalarNode, line: line, text: text}
}

func isNullText(s string) bool {
	switch s {
	case "", "~", "null", "Null", "NULL":
		return true
	}
	return false
}

// maxDepth bounds nesting so hostile input cannot exhaust the stack.
const maxDepth = 64

// flowDepth returns how many brackets are open at the end of s, given that
// depth were open at its start. Quoted scalars may not span lines.
func flowDepth(s string, line, depth int) (int, error) {
	inSingle, inDouble := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case inDouble:
			if c == '\\' {
				i++
			} else if c == '"' {
				inDouble = false
			}
		case inSingle:
			if c == '\'' {
				if i+1 < len(s) && s[i+1] == '\'' {
					i++
				} else {
					inSingle = false
				}
			}
		default:
			switch c {
			case '"':
				inDouble = true
			case '\'':
				inSingle = true
			case '[', '{':
				depth++
				if depth > maxDepth {
					return 0, errAt(line, "flow collections nested more than %d deep", maxDepth)
				}
			case ']', '}':
				depth--
				if depth < 0 {
					return 0, errAt(line, "unbalanced %q", string(c))
				}
			}
		}
	}
	if inSingle || inDouble {
		return 0, errAt(line, "unterminated quoted scalar")
	}
	return depth, nil
}

// parseQuoted decodes the quoted scalar starting at s[start] and returns the
// value and the index just after the closing quote.
func parseQuoted(s string, start, line int) (string, int, error) {
	q := s[start]
	var b strings.Builder
	for i := start + 1; i < len(s); i++ {
		c := s[i]
		if q == '\'' {
			if c == '\'' {
				if i+1 < len(s) && s[i+1] == '\'' {
					b.WriteByte('\'')
					i++
					continue
				}
				return b.String(), i + 1, nil
			}
			b.WriteByte(c)
			continue
		}
		switch c {
		case '"':
			return b.String(), i + 1, nil
		case '\\':
			if i+1 >= len(s) {
				return "", 0, errAt(line, "unterminated escape sequence")
			}
			i++
			switch s[i] {
			case '"', '\\', '/':
				b.WriteByte(s[i])
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			case 'b':
				b.WriteByte('\b')
			case 'f':
				b.WriteByte('\f')
			case 'u':
				if i+4 >= len(s) {
					return "", 0, errAt(line, "short \\u escape")
				}
				r, err := strconv.ParseUint(s[i+1:i+5], 16, 32)
				if err != nil {
					return "", 0, errAt(line, "bad \\u escape %q", s[i-1:i+5])
				}
				b.WriteRune(rune(r))
				i += 4
			default:
				return "", 0, errAt(line, "unsupported escape \\%c", s[i])
			}
		default:
			b.WriteByte(c)
		}
	}
	return "", 0, errAt(line, "unterminated quoted scalar")
}

type flowParser struct {
	s    string
	i    int
	line int
}

func (f *flowParser) skipSpaces() {
	for f.i < len(f.s) && f.s[f.i] == ' ' {
		f.i++
	}
}

func (f *flowParser) value() (*node, error) {
	f.skipSpaces()
	if f.i >= len(f.s) {
		return nil, errAt(f.line, "missing value in flow collection")
	}
	switch f.s[f.i] {
	case '[':
		return f.seq()
	case '{':
		return f.mapping()
	case '"', '\'':
		val, end, err := parseQuoted(f.s, f.i, f.line)
		if err != nil {
			return nil, err
		}
		f.i = end
		return &node{kind: scalarNode, line: f.line, text: val, quoted: true}, nil
	case '&', '*', '!', '|', '>', '?', '%', '@', '`':
		return nil, errAt(f.line, "unsupported YAML feature %q in flow collection", f.s[f.i:f.i+1])
	}
	text, err := f.plain(false)
	if err != nil {
		return nil, err
	}
	if text == "" {
		return nil, errAt(f.line, "empty entry in flow collection")
	}
	return plainNode(text, f.line), nil
}

// plain reads a plain scalar inside a flow collection. In key context it
// stops at ':'; in value context a ':' followed by a space is an error.
func (f *flowParser) plain(key bool) (string, error) {
	start := f.i
	for f.i < len(f.s) {
		c := f.s[f.i]
		if c == ',' || c == ']' || c == '}' || c == '[' || c == '{' {
			break
		}
		if c == ':' && (f.i+1 == len(f.s) || strings.ContainsRune(" ,]}", rune(f.s[f.i+1]))) {
			if key {
				break
			}
			return "", errAt(f.line, "unexpected ':' in flow value %q; put it in quotes", strings.TrimSpace(f.s[start:f.i+1]))
		}
		f.i++
	}
	return strings.TrimSpace(f.s[start:f.i]), nil
}

func (f *flowParser) seq() (*node, error) {
	n := &node{kind: seqNode, line: f.line}
	f.i++ // '['
	for {
		f.skipSpaces()
		if f.i >= len(f.s) {
			return nil, errAt(f.line, "flow sequence is never closed")
		}
		if f.s[f.i] == ']' {
			f.i++
			return n, nil
		}
		item, err := f.value()
		if err != nil {
			return nil, err
		}
		n.items = append(n.items, item)
		f.skipSpaces()
		if f.i < len(f.s) && f.s[f.i] == ',' {
			f.i++
			continue
		}
		if f.i < len(f.s) && f.s[f.i] == ']' {
			f.i++
			return n, nil
		}
		return nil, errAt(f.line, "expected ',' or ']' in flow sequence")
	}
}

func (f *flowParser) mapping() (*node, error) {
	n := &node{kind: mapNode, line: f.line}
	seen := map[string]bool{}
	f.i++ // '{'
	for {
		f.skipSpaces()
		if f.i >= len(f.s) {
			return nil, errAt(f.line, "flow mapping is never closed")
		}
		if f.s[f.i] == '}' {
			f.i++
			return n, nil
		}
		var key string
		if c := f.s[f.i]; c == '"' || c == '\'' {
			val, end, err := parseQuoted(f.s, f.i, f.line)
			if err != nil {
				return nil, err
			}
			key, f.i = val, end
		} else {
			k, err := f.plain(true)
			if err != nil {
				return nil, err
			}
			if k == "" {
				return nil, errAt(f.line, "empty key in flow mapping")
			}
			key = k
		}
		if seen[key] {
			return nil, errAt(f.line, "duplicate key %q", key)
		}
		seen[key] = true
		f.skipSpaces()
		if f.i >= len(f.s) || f.s[f.i] != ':' {
			return nil, errAt(f.line, "expected ':' after key %q", key)
		}
		f.i++
		f.skipSpaces()
		var val *node
		if f.i < len(f.s) && (f.s[f.i] == ',' || f.s[f.i] == '}') {
			val = &node{kind: nullNode, line: f.line}
		} else {
			v, err := f.value()
			if err != nil {
				return nil, err
			}
			val = v
		}
		n.keys = append(n.keys, key)
		n.vals = append(n.vals, val)
		f.skipSpaces()
		if f.i < len(f.s) && f.s[f.i] == ',' {
			f.i++
			continue
		}
		if f.i < len(f.s) && f.s[f.i] == '}' {
			f.i++
			return n, nil
		}
		return nil, errAt(f.line, "expected ',' or '}' in flow mapping")
	}
}
