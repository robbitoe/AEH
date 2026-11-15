package aeh

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

const mockupHead = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta http-equiv="Content-Security-Policy" content="default-src 'none'; style-src 'unsafe-inline'; img-src data:; form-action 'none'; base-uri 'none'; frame-ancestors 'none'">
<meta name="referrer" content="no-referrer">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>`

const mockupBody = `</title>
</head>
<body>
`

const mockupTail = `
</body>
</html>
`

var mockupEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&#39;")

var mockupElements = wordSet("div span main header footer nav section article aside h1 h2 h3 h4 h5 h6 p strong em b i small code pre blockquote label br hr img input ul ol li dl dt dd table thead tbody tfoot tr th td caption button select option a style")
var mockupVoid = wordSet("br hr img input")
var mockupInputTypes = wordSet("text email password number checkbox radio search date tel url")
var mockupClass = regexp.MustCompile(`^[A-Za-z0-9_-]+(?: [A-Za-z0-9_-]+)*$`)
var mockupID = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
var mockupHref = regexp.MustCompile(`^#[A-Za-z][A-Za-z0-9_-]*$`)
var mockupSpan = regexp.MustCompile(`^[0-9]+$`)

func wordSet(words string) map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(words) {
		m[w] = true
	}
	return m
}

// sanitizeMockup accepts a body fragment, not a document. It never publishes the
// source bytes: only validated tokens reconstructed under the runner's wrapper.
// Secret scanning is a separate raw-byte check for callers before reconstruction.
func sanitizeMockup(src []byte, title string, maxBytes, maxImageBytes int) ([]byte, error) {
	s := string(src)
	if len(src) > maxBytes {
		return nil, artifactError(s, 0, "mockup source exceeds the byte limit")
	}
	if err := checkArtifactCharacters(s); err != nil {
		return nil, err
	}
	if err := checkDisplayLine(title, maxBytes); err != nil {
		return nil, fmt.Errorf("mockup title: %w", err)
	}
	p := mockupParser{src: s, maxImageBytes: maxImageBytes}
	for p.pos < len(s) {
		var err error
		if s[p.pos] != '<' {
			err = p.text()
		} else if strings.HasPrefix(s[p.pos:], "</") {
			err = p.endTag()
		} else {
			err = p.startTag()
		}
		if err != nil {
			return nil, err
		}
		if p.out.Len() > maxBytes {
			return nil, p.fail("rebuilt mockup fragment exceeds the byte limit")
		}
	}
	if len(p.stack) != 0 {
		return nil, p.fail("every non-void element must be explicitly closed in nesting order")
	}
	return []byte(mockupHead + mockupEscaper.Replace(title) + mockupBody + p.out.String() + mockupTail), nil
}

type mockupParser struct {
	src           string
	pos           int
	out           strings.Builder
	stack         []string
	maxImageBytes int
}

func (p *mockupParser) fail(rule string) error { return artifactError(p.src, p.pos, rule) }

func mockupSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' }

func (p *mockupParser) spaces() {
	for p.pos < len(p.src) && mockupSpace(p.src[p.pos]) {
		p.pos++
	}
}

func (p *mockupParser) text() error {
	start := p.pos
	for p.pos < len(p.src) && p.src[p.pos] != '<' {
		p.pos++
	}
	text, at, ok := decodeMockupEntities(p.src[start:p.pos])
	if !ok {
		return artifactError(p.src, start+at, "only the six allowed character entities may follow an ampersand")
	}
	p.out.WriteString(mockupEscaper.Replace(text))
	return nil
}

// decodeMockupEntities is deliberately not html.UnescapeString: the accepted
// vocabulary is closed and semicolons are mandatory.
func decodeMockupEntities(s string) (string, int, bool) {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] != '&' {
			b.WriteByte(s[i])
			i++
			continue
		}
		found := false
		for _, e := range []struct{ encoded, decoded string }{
			{"&amp;", "&"}, {"&lt;", "<"}, {"&gt;", ">"}, {"&quot;", `"`}, {"&#39;", "'"}, {"&nbsp;", "\u00a0"},
		} {
			if strings.HasPrefix(s[i:], e.encoded) {
				b.WriteString(e.decoded)
				i += len(e.encoded)
				found = true
				break
			}
		}
		if !found {
			return "", i, false
		}
	}
	return b.String(), 0, true
}

// name reads exactly the lowercase ASCII name grammar. Tag names admit digits;
// attribute names admit hyphens. Delimiters are checked by the caller.
func (p *mockupParser) name(attribute bool) string {
	start := p.pos
	if p.pos >= len(p.src) || p.src[p.pos] < 'a' || p.src[p.pos] > 'z' {
		return ""
	}
	p.pos++
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		if c >= 'a' && c <= 'z' || attribute && c == '-' || !attribute && c >= '0' && c <= '9' {
			p.pos++
		} else {
			break
		}
	}
	return p.src[start:p.pos]
}

func (p *mockupParser) startTag() error {
	p.pos++ // '<'
	tag := p.name(false)
	if !mockupElements[tag] {
		return p.fail("only allowlisted lowercase elements are permitted; declarations, comments and processing instructions are forbidden")
	}
	if tag == "style" && len(p.stack) != 0 {
		return p.fail("style elements must be direct children of the fragment root")
	}
	attrs := map[string]string{}
	for {
		before := p.pos
		p.spaces()
		if p.pos == len(p.src) {
			return p.fail("start tags must end with a greater-than sign")
		}
		if p.src[p.pos] == '>' || p.src[p.pos] == '/' {
			break
		}
		if before == p.pos {
			return p.fail("attributes must be separated by whitespace")
		}
		name := p.name(true)
		if name == "" {
			return p.fail("attribute names must be lowercase ASCII letters and hyphens")
		}
		if _, exists := attrs[name]; exists {
			return p.fail("duplicate attributes are forbidden")
		}
		if !allowedMockupAttribute(tag, name) {
			return p.fail("only allowlisted attributes are permitted; event handlers are forbidden")
		}
		value, assigned := "", false
		if p.pos < len(p.src) && p.src[p.pos] == '=' {
			assigned = true
			p.pos++
			if p.pos == len(p.src) || p.src[p.pos] != '"' {
				return p.fail("attribute values require double quotes immediately after equals")
			}
			p.pos++
			start := p.pos
			for p.pos < len(p.src) && p.src[p.pos] != '"' {
				p.pos++
			}
			if p.pos == len(p.src) {
				return p.fail("attribute values require a closing double quote")
			}
			raw := p.src[start:p.pos]
			if strings.ContainsAny(raw, "<`") || strings.ContainsFunc(raw, unicode.IsControl) {
				return artifactError(p.src, start, "attribute values must not contain less-than signs, backticks or control characters")
			}
			var at int
			var ok bool
			value, at, ok = decodeMockupEntities(raw)
			if !ok {
				return artifactError(p.src, start+at, "attribute values permit only the six allowed character entities")
			}
			p.pos++
		}
		if name != "disabled" && !assigned {
			return p.fail("only disabled may be a boolean attribute without an assigned value")
		}
		if rule := checkMockupAttribute(name, value, p.maxImageBytes); rule != "" {
			return p.fail(rule)
		}
		if name == "type" && (tag == "button" && value != "button" || tag == "input" && !mockupInputTypes[value]) {
			return p.fail("type must belong to the element's inert control allowlist")
		}
		attrs[name] = value
	}
	if p.src[p.pos] == '/' {
		if !mockupVoid[tag] {
			return p.fail("self-closing syntax is allowed only for void elements")
		}
		p.pos++
	}
	if p.pos == len(p.src) || p.src[p.pos] != '>' {
		return p.fail("a start tag must end immediately after its optional slash")
	}
	p.pos++
	if len(p.stack) >= 64 {
		return p.fail("element nesting exceeds depth 64")
	}
	if tag == "button" {
		attrs["type"] = "button"
	}
	p.out.WriteByte('<')
	p.out.WriteString(tag)
	keys := make([]string, 0, len(attrs))
	for name := range attrs {
		keys = append(keys, name)
	}
	sort.Strings(keys)
	for _, name := range keys {
		p.out.WriteByte(' ')
		p.out.WriteString(name)
		if name != "disabled" {
			p.out.WriteString(`="`)
			p.out.WriteString(mockupEscaper.Replace(attrs[name]))
			p.out.WriteByte('"')
		}
	}
	p.out.WriteByte('>')
	if mockupVoid[tag] {
		return nil
	}
	p.stack = append(p.stack, tag)
	if tag == "style" {
		end := strings.Index(p.src[p.pos:], "</style>")
		if end < 0 {
			return p.fail("style elements require an exact closing tag")
		}
		css := p.src[p.pos : p.pos+end]
		if strings.Contains(css, "&") {
			return p.fail("style element text must not contain entities")
		}
		if rule := checkMockupCSS(css); rule != "" {
			return p.fail(rule)
		}
		p.out.WriteString(css)
		p.pos += end
		return p.endTag()
	}
	return nil
}

func (p *mockupParser) endTag() error {
	p.pos += 2 // '</'
	tag := p.name(false)
	if p.pos >= len(p.src) || p.src[p.pos] != '>' {
		return p.fail("end tags must have exactly a lowercase name and a greater-than sign")
	}
	if len(p.stack) == 0 || tag != p.stack[len(p.stack)-1] {
		return p.fail("end tags must close a non-void element in nesting order")
	}
	p.pos++
	p.stack = p.stack[:len(p.stack)-1]
	p.out.WriteString("</" + tag + ">")
	return nil
}

func allowedMockupAttribute(tag, name string) bool {
	if name == "class" || name == "id" || name == "style" {
		return true
	}
	switch tag {
	case "a":
		return name == "href"
	case "img":
		return name == "src" || name == "alt"
	case "th", "td":
		return name == "colspan" || name == "rowspan"
	case "input":
		return name == "type" || name == "placeholder" || name == "value" || name == "disabled"
	case "button":
		return name == "type" || name == "disabled"
	case "select":
		return name == "disabled"
	case "option":
		return name == "value" || name == "disabled"
	}
	return false
}

func checkMockupAttribute(name, value string, maxImageBytes int) string {
	switch name {
	case "class":
		if !mockupClass.MatchString(value) {
			return "class requires tokens separated by single spaces"
		}
	case "id":
		if !mockupID.MatchString(value) {
			return "id requires one ASCII letter, digit, underscore or hyphen token"
		}
	case "href":
		if !mockupHref.MatchString(value) {
			return "href must be a local anchor beginning with an ASCII letter"
		}
	case "src":
		if !validMockupImage(value, maxImageBytes) {
			return "src requires a size-limited PNG, JPEG or GIF data image with valid standard base64 and matching magic bytes"
		}
	case "colspan", "rowspan":
		n, err := strconv.Atoi(value)
		if err != nil || !mockupSpan.MatchString(value) || n < 1 || n > 100 {
			return "colspan and rowspan must be integers from 1 through 100"
		}
	case "style":
		return checkMockupCSS(value)
	}
	return ""
}

func checkMockupCSS(css string) string {
	lower := strings.ToLower(css)
	for _, forbidden := range []string{"\\", "/*", "<", "url(", "image-set(", "@import", "@font-face", "@namespace", "expression", "behavior", "-moz-binding", "javascript:"} {
		if strings.Contains(lower, forbidden) {
			return "CSS must not contain escapes, comments, markup, external resources or executable constructs"
		}
	}
	return ""
}

func validMockupImage(value string, maxBytes int) bool {
	for _, image := range []struct {
		prefix string
		magic  []byte
	}{
		{"data:image/png;base64,", []byte("\x89PNG\r\n\x1a\n")},
		{"data:image/jpeg;base64,", []byte{0xff, 0xd8, 0xff}},
		{"data:image/gif;base64,", []byte("GIF87a")},
		{"data:image/gif;base64,", []byte("GIF89a")},
	} {
		if !strings.HasPrefix(value, image.prefix) {
			continue
		}
		encoded := strings.TrimPrefix(value, image.prefix)
		// DecodeString tolerates CR/LF; the grammar requires standard base64 only.
		if strings.ContainsAny(encoded, "\r\n") || maxBytes < 0 {
			return false
		}
		decoded, err := base64.StdEncoding.Strict().DecodeString(encoded)
		if err == nil && len(decoded) <= maxBytes && bytes.HasPrefix(decoded, image.magic) {
			return true
		}
	}
	return false
}
