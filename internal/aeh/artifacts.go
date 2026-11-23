package aeh

import (
	"bytes"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// artifactError reports only a position and a fixed rule, never untrusted content.
// Columns count Unicode code points, starting at one.
func artifactError(s string, at int, rule string) error {
	line := strings.Count(s[:at], "\n") + 1
	start := strings.LastIndexByte(s[:at], '\n') + 1
	return fmt.Errorf("line %d, column %d: %s", line, utf8.RuneCountInString(s[start:at])+1, rule)
}

func checkArtifactCharacters(s string) error {
	if i := artifactCharacterIssue(s); i >= 0 {
		return artifactError(s, i, "text must be valid UTF-8 without forbidden control or bidi characters")
	}
	return nil
}

func artifactCharacterIssue(s string) int {
	for i, r := range s {
		if r == utf8.RuneError {
			if _, n := utf8.DecodeRuneInString(s[i:]); n == 1 {
				return i
			}
		}
		if (unicode.IsControl(r) && r != '\n' && r != '\t') || isArtifactBidi(r) {
			return i
		}
	}
	return -1
}

func isArtifactBidi(r rune) bool {
	return r == 0x061c || r == 0x200e || r == 0x200f ||
		(r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069)
}

func asciiLetter(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }

// checkDisplayMarkdown implements the display policy even inside inline code.
// Only a column-zero opening fence exempts content from Markdown syntax checks;
// character checks still apply throughout, including the fenced content. As in
// criteria.go, any reference-definition mark is rejected outside fences: nested
// or multiline definitions can otherwise resolve an external reference image.
func checkDisplayMarkdown(text string, maxBytes int) error {
	if len(text) > maxBytes {
		return artifactError(text, 0, "Markdown exceeds the byte limit")
	}
	var fence codeFence
	offset := 0
	linkScope := 0
	for _, line := range strings.Split(text, "\n") {
		if i := artifactCharacterIssue(line); i >= 0 {
			return artifactError(text, offset+i, "text must be valid UTF-8 without forbidden control or bidi characters")
		}
		trimmed := strings.TrimLeft(line, " \t")
		if fence.n == 0 && trimmed != line && (strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~")) {
			return artifactError(text, offset, "opening fences must start at column 0")
		}
		if fence.step(line) {
			offset += len(line) + 1
			linkScope = offset
			continue
		}
		for i := 0; i < len(line); i++ {
			switch line[i] {
			case '<':
				return artifactError(text, offset+i, "raw HTML and autolinks are forbidden outside fences")
			case '&':
				j := i + 1
				if j < len(line) && line[j] == '#' {
					return artifactError(text, offset+i, "character references are forbidden outside fences")
				}
				for j < len(line) && asciiLetter(line[j]) {
					j++
				}
				if j > i+1 && j < len(line) && line[j] == ';' {
					return artifactError(text, offset+i, "character references are forbidden outside fences")
				}
			case ']':
				if i+1 < len(line) && line[i+1] == ':' {
					return artifactError(text, offset+i, "reference-definition marks are forbidden outside fences; use inline links")
				}
				if i+1 >= len(line) || line[i+1] != '(' {
					continue
				}
				end := strings.IndexByte(line[i+2:], ')')
				if end < 0 {
					return artifactError(text, offset+i, "inline targets require a closing parenthesis on the same line")
				}
				target := line[i+2 : i+2+end]
				image := strings.HasPrefix(target, "https://") && displayImage(text[linkScope:offset+len(line)], offset+i-linkScope)
				if !validDisplayTarget(target, image) {
					return artifactError(text, offset+i, "inline targets must be HTTPS links or clean relative memory paths; images must be relative; titles are forbidden")
				}
			}
		}
		offset += len(line) + 1
	}
	return nil
}

// displayImage checks both literal brackets (inline code is not exempt from the
// display policy) and brackets as rendered in labels. Escaped brackets or code
// spans in an image label must not disguise its external destination. Labels may
// wrap lines, but cannot extend across a fence.
func displayImage(text string, close int) bool {
	if displayImageBracket(text, close) {
		return true
	}
	if !strings.ContainsAny(text[:close], "\\`") {
		return false
	}
	var label strings.Builder
	for _, seg := range splitCodeSpans(text) {
		if seg.code {
			label.WriteString(strings.Repeat(" ", len(seg.text)))
			continue
		}
		for i := 0; i < len(seg.text); i++ {
			if seg.text[i] == '\\' && i+1 < len(seg.text) {
				label.WriteString("  ")
				i++
			} else {
				label.WriteByte(seg.text[i])
			}
		}
	}
	return displayImageBracket(label.String(), close)
}

func displayImageBracket(text string, close int) bool {
	if text[close] != ']' {
		return false
	}
	depth := 1
	for i := close - 1; i >= 0; i-- {
		switch text[i] {
		case ']':
			depth++
		case '[':
			depth--
			if depth == 0 {
				return i > 0 && text[i-1] == '!'
			}
		}
	}
	return false
}

var displayAnchor = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func validDisplayTarget(target string, image bool) bool {
	if target == "" || strings.ContainsAny(target, "()<>\\\"'") || strings.ContainsFunc(target, unicode.IsSpace) {
		return false
	}
	if strings.HasPrefix(target, "https://") {
		u, err := url.Parse(target)
		return !image && err == nil && u.Hostname() != "" && u.Opaque == ""
	}
	path, anchor, hasAnchor := strings.Cut(target, "#")
	if hasAnchor && !displayAnchor.MatchString(anchor) {
		return false
	}
	if checkRelPath(path) != nil {
		return false
	}
	// Encoded separators and dot segments would cease to be clean when opened.
	decoded, err := url.PathUnescape(path)
	return err == nil && checkRelPath(decoded) == nil && !strings.ContainsAny(decoded, "#()<>\"'") && !strings.ContainsFunc(decoded, unicode.IsSpace)
}

func checkDisplayLine(s string, maxBytes int) error {
	if i := strings.IndexAny(s, "\n\t"); i >= 0 {
		return artifactError(s, i, "single-line fields must not contain LF or tab")
	}
	if strings.TrimSpace(s) == "" {
		return artifactError(s, 0, "single-line fields must be non-empty after trimming")
	}
	// A single line is shown inside other text, so it cannot open a fence that exempts it.
	if t := strings.TrimLeft(s, " "); strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") {
		return artifactError(s, 0, "single-line fields must not start with a code fence")
	}
	return checkDisplayMarkdown(s, maxBytes)
}

type secretFinding struct {
	Line int
	Kind string
}

// This best-effort list detects PEM and PGP private keys, AWS access keys, GitHub and Slack
// tokens, Google API keys, Stripe live keys, Anthropic and OpenAI-style API keys,
// JWTs, and URL connection strings with a userinfo password. It deliberately has
// no generic password-assignment rule and cannot replace minimizing agent input.
var artifactSecretPatterns = []struct {
	kind string
	re   *regexp.Regexp
}{
	{"private key", regexp.MustCompile(`-----BEGIN (?:(?:RSA |EC |OPENSSH |DSA |ENCRYPTED )?PRIVATE KEY|PGP PRIVATE KEY BLOCK)-----`)},
	{"AWS access key", regexp.MustCompile(`(?:AKIA|ASIA)[A-Z0-9]{16}`)},
	{"GitHub token", regexp.MustCompile(`(?:gh[pousr]_[A-Za-z0-9]{36,}|github_pat_[A-Za-z0-9_]{22,})`)},
	{"Slack token", regexp.MustCompile(`xox[abprs]-[A-Za-z0-9-]+`)},
	{"Google API key", regexp.MustCompile(`AIza[A-Za-z0-9_-]{35}`)},
	{"Stripe live key", regexp.MustCompile(`(?:sk_live_|rk_live_)[A-Za-z0-9]{16,}`)},
	{"Anthropic API key", regexp.MustCompile(`sk-ant-[A-Za-z0-9_-]{20,}`)},
	{"OpenAI-style API key", regexp.MustCompile(`sk-[A-Za-z0-9_-]{20,}`)},
	{"JWT", regexp.MustCompile(`eyJ[A-Za-z0-9_-]{10,}\.eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}`)},
	{"connection string with password", regexp.MustCompile(`[a-z][a-z0-9+.-]*://[^\s:/@]+:[^\s@/]+@`)},
}

func findSecrets(b []byte) []secretFinding {
	type match struct {
		at   int
		kind string
	}
	var matches []match
	for _, p := range artifactSecretPatterns {
		for _, m := range p.re.FindAllIndex(b, -1) {
			if p.kind == "OpenAI-style API key" && bytes.HasPrefix(b[m[0]:], []byte("sk-ant-")) {
				continue
			}
			matches = append(matches, match{m[0], p.kind})
		}
	}
	sort.SliceStable(matches, func(i, j int) bool { return matches[i].at < matches[j].at })
	var findings []secretFinding
	line, start := 1, 0
	for _, m := range matches {
		line += bytes.Count(b[start:m.at], []byte{'\n'})
		start = m.at
		findings = append(findings, secretFinding{Line: line, Kind: m.kind})
	}
	return findings
}

// redactDiagnostic makes a diagnostic safe to show, store and send back to an agent: every
// recognized secret is replaced by its kind, and control, bidi and format characters other
// than LF become "?".
func redactDiagnostic(s string) string {
	s = redactSecrets(s)
	return strings.Map(func(r rune) rune {
		if r == '\n' {
			return r
		}
		if r == utf8.RuneError || unicode.IsControl(r) || isArtifactBidi(r) || unicode.Is(unicode.Cf, r) {
			return '?'
		}
		return r
	}, strings.ToValidUTF8(s, "?"))
}

// redactSecrets replaces every recognized secret and leaves the rest of the text unchanged.
func redactSecrets(s string) string {
	for _, p := range artifactSecretPatterns {
		s = p.re.ReplaceAllString(s, "[redacted "+p.kind+"]")
	}
	return s
}

func checkNoSecrets(name string, b []byte) error {
	findings := findSecrets(b)
	if len(findings) == 0 {
		return nil
	}
	var messages []string
	for _, f := range findings[:min(5, len(findings))] {
		messages = append(messages, fmt.Sprintf("%s:%d: possible %s; remove it (aeh never shows the value)", name, f.Line, f.Kind))
	}
	return fmt.Errorf("%s", strings.Join(messages, "\n"))
}
