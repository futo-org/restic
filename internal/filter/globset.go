package filter

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Glob is a glob pattern with the matching semantics of the Rust globset crate
// 0.4.18 (crates/globset/src/glob.rs), as built by
// GlobBuilder::new(pattern).case_insensitive(true): `*` and `?` match `/`,
// backslash escapes the next character, and empty alternates are dropped.
//
// globset matches a non-Unicode regex against the raw bytes of a path, so `?`
// matches one byte and case folding applies to ASCII letters only. Go regexps
// match runes, so the pattern and the path are both expressed with one rune per
// byte, which keeps the semantics identical for any path, including paths that
// are not valid UTF-8.
type Glob struct {
	re *regexp.Regexp
	// requiredLiteral is the longest run of literal bytes, lowercased, that every
	// matching path contains. Checking it first skips the regex for most paths,
	// the same shortcut globset takes with its literal strategies.
	requiredLiteral []byte
}

// ParseGlob compiles pattern. Errors carry the same messages as globset.
func ParseGlob(pattern string) (Glob, error) {
	p := globParser{
		glob:     pattern,
		chars:    []rune(pattern),
		prev:     noRune,
		cur:      noRune,
		branches: [][]globToken{nil},
	}
	err := p.parse()
	if err != nil {
		return Glob{}, err
	}

	if len(p.branches) > 1 {
		return Glob{}, p.error("unclosed alternate group; missing '}' (maybe escape '{' with '[{]'?)")
	}

	var re strings.Builder
	re.WriteString("(?s)^")
	tokens := p.branches[0]
	if len(tokens) == 1 && tokens[0].kind == globRecursivePrefix {
		re.WriteString(".*")
	} else {
		writeGlobRegex(&re, tokens)
	}
	re.WriteString("$")

	compiled, err := regexp.Compile(re.String())
	if err != nil {
		return Glob{}, p.error(err.Error())
	}

	var requiredLiteral, run []byte
	for _, token := range tokens {
		if token.kind != globLiteral {
			run = nil
			continue
		}

		for _, b := range utf8Bytes(token.literal) {
			run = append(run, asciiLower(b))
		}

		if len(run) > len(requiredLiteral) {
			requiredLiteral = run
		}
	}

	return Glob{re: compiled, requiredLiteral: requiredLiteral}, nil
}

// Match reports whether path matches the whole glob.
func (g Glob) Match(path string) bool {
	if !containsASCIIFold(path, g.requiredLiteral) {
		return false
	}

	var subject strings.Builder
	subject.Grow(len(path))
	for i := 0; i < len(path); i++ {
		subject.WriteRune(rune(asciiLower(path[i])))
	}

	return g.re.MatchString(subject.String())
}

type globTokenKind int

const (
	globLiteral globTokenKind = iota
	globAny
	globZeroOrMore
	globRecursivePrefix
	globRecursiveSuffix
	globRecursiveZeroOrMore
	globClass
	globAlternates
)

type globToken struct {
	kind       globTokenKind
	literal    rune
	negated    bool
	ranges     [][2]rune
	alternates [][]globToken
}

const noRune rune = -1

type globParser struct {
	glob            string
	chars           []rune
	pos             int
	prev            rune
	cur             rune
	alternatesStack []int
	branches        [][]globToken
}

func (p *globParser) error(kind string) error {
	return fmt.Errorf("error parsing glob '%s': %s", p.glob, kind)
}

func (p *globParser) parse() error {
	for c := p.bump(); c != noRune; c = p.bump() {
		var err error
		switch c {
		case '?':
			p.push(globToken{kind: globAny})
		case '*':
			p.parseStar()
		case '[':
			err = p.parseClass()
		case '{':
			p.alternatesStack = append(p.alternatesStack, len(p.branches))
			p.branches = append(p.branches, nil)
		case '}':
			err = p.popAlternate()
		case ',':
			if len(p.alternatesStack) == 0 {
				p.push(globToken{kind: globLiteral, literal: ','})
			} else {
				p.branches = append(p.branches, nil)
			}
		case '\\':
			escaped := p.bump()
			if escaped == noRune {
				return p.error(`dangling '\'`)
			}
			p.push(globToken{kind: globLiteral, literal: escaped})
		default:
			p.push(globToken{kind: globLiteral, literal: c})
		}

		if err != nil {
			return err
		}
	}

	return nil
}

func (p *globParser) popAlternate() error {
	if len(p.alternatesStack) == 0 {
		return p.error("unopened alternate group; missing '{' (maybe escape '}' with '[}]'?)")
	}

	start := p.alternatesStack[len(p.alternatesStack)-1]
	p.alternatesStack = p.alternatesStack[:len(p.alternatesStack)-1]
	alternates := append([][]globToken(nil), p.branches[start:]...)
	p.branches = p.branches[:start]
	p.push(globToken{kind: globAlternates, alternates: alternates})
	return nil
}

func (p *globParser) parseStar() {
	prev := p.prev
	zeroOrMoreTwice := func() {
		p.push(globToken{kind: globZeroOrMore})
		p.push(globToken{kind: globZeroOrMore})
	}

	if p.peek() != '*' {
		p.push(globToken{kind: globZeroOrMore})
		return
	}

	p.bump()
	if len(p.branches[len(p.branches)-1]) == 0 {
		if p.peek() != noRune && p.peek() != '/' {
			zeroOrMoreTwice()
			return
		}

		p.push(globToken{kind: globRecursivePrefix})
		p.bump()
		return
	}

	if prev != '/' && (len(p.branches) <= 1 || (prev != ',' && prev != '{')) {
		zeroOrMoreTwice()
		return
	}

	isSuffix := false
	switch next := p.peek(); {
	case next == noRune:
		p.bump()
		isSuffix = true
	case (next == ',' || next == '}') && len(p.branches) >= 2:
		isSuffix = true
	case next == '/':
		p.bump()
	default:
		zeroOrMoreTwice()
		return
	}

	branch := p.branches[len(p.branches)-1]
	last := branch[len(branch)-1]
	p.branches[len(p.branches)-1] = branch[:len(branch)-1]
	switch {
	case last.kind == globRecursivePrefix || last.kind == globRecursiveSuffix:
		p.push(last)
	case isSuffix:
		p.push(globToken{kind: globRecursiveSuffix})
	default:
		p.push(globToken{kind: globRecursiveZeroOrMore})
	}
}

func (p *globParser) parseClass() error {
	var ranges [][2]rune
	negated := false
	if p.peek() == '!' || p.peek() == '^' {
		p.bump()
		negated = true
	}

	extendLastRange := func(end rune) error {
		last := &ranges[len(ranges)-1]
		last[1] = end
		if last[1] < last[0] {
			return p.error(fmt.Sprintf("invalid range; '%c' > '%c'", last[0], last[1]))
		}

		return nil
	}

	first := true
	inRange := false
	for {
		c := p.bump()
		if c == noRune {
			return p.error("unclosed character class; missing ']'")
		}

		switch {
		case c == ']' && first:
			ranges = append(ranges, [2]rune{']', ']'})
		case c == ']':
			if inRange {
				ranges = append(ranges, [2]rune{'-', '-'})
			}

			p.push(globToken{kind: globClass, negated: negated, ranges: ranges})
			return nil
		case c == '-' && first:
			ranges = append(ranges, [2]rune{'-', '-'})
		case c == '-' && inRange:
			err := extendLastRange('-')
			if err != nil {
				return err
			}

			inRange = false
		case c == '-':
			inRange = true
		case inRange:
			err := extendLastRange(c)
			if err != nil {
				return err
			}

			inRange = false
		default:
			ranges = append(ranges, [2]rune{c, c})
		}

		first = false
	}
}

func (p *globParser) push(token globToken) {
	last := len(p.branches) - 1
	p.branches[last] = append(p.branches[last], token)
}

func (p *globParser) bump() rune {
	p.prev = p.cur
	p.cur = p.peek()
	if p.cur != noRune {
		p.pos++
	}

	return p.cur
}

func (p *globParser) peek() rune {
	if p.pos >= len(p.chars) {
		return noRune
	}

	return p.chars[p.pos]
}

func writeGlobRegex(re *strings.Builder, tokens []globToken) {
	for _, token := range tokens {
		switch token.kind {
		case globLiteral:
			for _, b := range utf8Bytes(token.literal) {
				writeRegexByte(re, asciiLower(b))
			}
		case globAny:
			re.WriteString(".")
		case globZeroOrMore:
			re.WriteString(".*")
		case globRecursivePrefix:
			re.WriteString("(?:/?|.*/)")
		case globRecursiveSuffix:
			re.WriteString("/.*")
		case globRecursiveZeroOrMore:
			re.WriteString("(?:/|/.*/)")
		case globClass:
			writeGlobClass(re, token)
		case globAlternates:
			var parts []string
			for _, alternate := range token.alternates {
				var part strings.Builder
				writeGlobRegex(&part, alternate)
				if part.Len() > 0 {
					parts = append(parts, part.String())
				}
			}

			if len(parts) > 0 {
				re.WriteString("(?:" + strings.Join(parts, "|") + ")")
			}
		}
	}
}

// writeGlobClass mirrors how globset writes a class: each range bound is
// spelled as its UTF-8 bytes and the regex parser reads the result as a class
// of single bytes, so `[a-é]` is the bytes a-\xc3 plus \xa9.
func writeGlobClass(re *strings.Builder, token globToken) {
	var byteRanges [][2]byte
	for _, r := range token.ranges {
		start, end := utf8Bytes(r[0]), utf8Bytes(r[1])
		if r[0] == r[1] {
			for _, b := range start {
				byteRanges = append(byteRanges, [2]byte{b, b})
			}

			continue
		}

		for _, b := range start[:len(start)-1] {
			byteRanges = append(byteRanges, [2]byte{b, b})
		}

		byteRanges = append(byteRanges, [2]byte{start[len(start)-1], end[0]})
		for _, b := range end[1:] {
			byteRanges = append(byteRanges, [2]byte{b, b})
		}
	}

	re.WriteString("[")
	if token.negated {
		re.WriteString("^")
	}

	for _, r := range byteRanges {
		writeClassRange(re, r[0], r[1])
		upperStart, upperEnd := max(r[0], 'A'), min(r[1], 'Z')
		if upperStart <= upperEnd {
			writeClassRange(re, asciiLower(upperStart), asciiLower(upperEnd))
		}
	}

	re.WriteString("]")
}

func writeClassRange(re *strings.Builder, start, end byte) {
	fmt.Fprintf(re, `\x{%02x}-\x{%02x}`, start, end)
}

func writeRegexByte(re *strings.Builder, b byte) {
	if b < utf8.RuneSelf {
		re.WriteString(regexp.QuoteMeta(string(rune(b))))
		return
	}

	fmt.Fprintf(re, `\x{%02x}`, b)
}

func utf8Bytes(r rune) []byte {
	return utf8.AppendRune(nil, r)
}

func containsASCIIFold(s string, lowerSubstr []byte) bool {
	for start := 0; start+len(lowerSubstr) <= len(s); start++ {
		i := 0
		for i < len(lowerSubstr) && asciiLower(s[start+i]) == lowerSubstr[i] {
			i++
		}

		if i == len(lowerSubstr) {
			return true
		}
	}

	return false
}

func asciiLower(b byte) byte {
	if 'A' <= b && b <= 'Z' {
		return b + 'a' - 'A'
	}

	return b
}
