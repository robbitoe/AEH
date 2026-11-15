package aeh

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
)

func mockupTestImage(kind string, magic []byte) string {
	return "data:image/" + kind + ";base64," + base64.StdEncoding.EncodeToString(magic)
}

func rebuiltMockupBody(t *testing.T, document []byte) []byte {
	t.Helper()
	_, body, ok := bytes.Cut(document, []byte(mockupBody))
	if !ok || !bytes.HasSuffix(body, []byte(mockupTail)) {
		t.Fatal("missing runner wrapper")
	}
	return bytes.TrimSuffix(body, []byte(mockupTail))
}

func TestSanitizeMockupRealistic(t *testing.T) {
	png := mockupTestImage("png", []byte("\x89PNG\r\n\x1a\n"))
	src := `<style>.page { color: #123; display: grid; } button { padding: 8px; }</style>
<header id="top" class="page wide"><h1>Project &amp; team</h1><nav><a href="#members">Members</a></nav></header>
<main><section id="members"><h2>Members</h2><img src="` + png + `" alt="Team &quot;icon&quot;"/>
<table><caption>People</caption><thead><tr><th colspan="2">Name</th></tr></thead><tbody><tr><td rowspan="1">Ana</td><td>Ready</td></tr></tbody><tfoot><tr><td colspan="2">1 person</td></tr></tfoot></table>
<label>Email <input placeholder="you@example.com" type="email" disabled></label><input type="password" value="demo">
<select disabled><option value="editor">Editor</option><option disabled>Owner</option></select><button class="primary">Save</button>
<p style="font-weight: bold;">A &lt; B &gt; C &#39;quoted&#39; &nbsp; <strong>Ready</strong></p></section></main><footer><small>Preview</small></footer>`
	document, err := sanitizeMockup([]byte(src), `Team "preview" & planning`, 20000, 8)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`<header class="page wide" id="top">`, `<img alt="Team &quot;icon&quot;" src="` + png + `">`,
		`<input disabled placeholder="you@example.com" type="email">`, `<button class="primary" type="button">`,
		`<title>Team &quot;preview&quot; &amp; planning</title>`, `&#39;quoted&#39;`, "\u00a0",
		`default-src 'none'; style-src 'unsafe-inline'; img-src data:; form-action 'none'; base-uri 'none'; frame-ancestors 'none'`,
		`<meta name="referrer" content="no-referrer">`,
	} {
		if !bytes.Contains(document, []byte(want)) {
			t.Errorf("canonical document missing %q", want)
		}
	}
	body := rebuiltMockupBody(t, document)
	again, err := sanitizeMockup(body, `Team "preview" & planning`, 20000, 8)
	if err != nil || !bytes.Equal(document, again) {
		t.Fatalf("rebuilt fragment must be stable: %v", err)
	}
	if len(document) > 20000+len(mockupHead)+len(mockupEscaper.Replace(`Team "preview" & planning`))+len(mockupBody)+len(mockupTail) {
		t.Fatal("document exceeds wrapper plus fragment limit")
	}
}

func TestSanitizeMockupAllowedVocabulary(t *testing.T) {
	for tag := range mockupElements {
		t.Run(tag, func(t *testing.T) {
			src := "<" + tag + ">"
			if !mockupVoid[tag] {
				src += "</" + tag + ">"
			}
			if _, err := sanitizeMockup([]byte(src), "Preview", 1000, 100); err != nil {
				t.Fatal(err)
			}
		})
	}
	for typ := range mockupInputTypes {
		if _, err := sanitizeMockup([]byte(`<input type="`+typ+`">`), "Preview", 1000, 100); err != nil {
			t.Fatal(err)
		}
	}
	for _, src := range []string{
		"", "plain é\ttext\n日本語 \ufffd", `<div id="A_1-x" class="a B_2 c-d" style="color: red;"></div>`,
		`<p>&amp;&lt;&gt;&quot;&#39;&nbsp; &amp;lt; ' " ></p>`,
		`<input disabled/><button type="button" disabled></button><option value="&lt;&quot;&#39;">Label</option>`,
		`<input disabled=""><button disabled="disabled"></button><select disabled="true"></select>`,
		`<a href="#A_1-2">Anchor</a><td colspan="1" rowspan="100"></td>`,
		"<div\nclass=\"a\"\tid=\"b\" \n></div><br /><hr/>",
		strings.Repeat("<div>", 64) + strings.Repeat("</div>", 64),
		`<style>.x { color: red; } @media (min-width: 1px) { p { color: blue; } }</style>`,
		`<div style="font-family: &quot;Arial&quot;;"></div>`,
	} {
		doc, err := sanitizeMockup([]byte(src), "Preview", 10000, 100)
		if err != nil {
			t.Fatalf("allowed grammar rejected: %v", err)
		}
		again, err := sanitizeMockup(rebuiltMockupBody(t, doc), "Preview", 10000, 100)
		if err != nil || !bytes.Equal(doc, again) {
			t.Fatalf("allowed grammar not stable: %v", err)
		}
	}
}

func TestSanitizeMockupRejectsGrammar(t *testing.T) {
	cases := []struct{ name, src string }{
		{"UTF-8", "SECRET123\xff"}, {"NUL", "SECRET123\x00"}, {"CR", "SECRET123\r"},
		{"control", "SECRET123\x01"}, {"DEL", "SECRET123\x7f"}, {"C1", "SECRET123\u0085"},
		{"bidi", "SECRET123\u202e"}, {"comment", "<!-- SECRET123 -->"},
		{"declaration", "<!SECRET123>"}, {"doctype", "<!doctype SECRET123>"},
		{"CDATA", "<![CDATA[SECRET123]]>"}, {"processing instruction", "<?SECRET123?>"},
		{"unknown tag", "<secret123>SECRET123</secret123>"}, {"script", "<script>SECRET123</script>"},
		{"SVG", "<svg>SECRET123</svg>"}, {"form", "<form>SECRET123</form>"},
		{"meta", `<meta content="SECRET123">`}, {"base", `<base href="SECRET123">`}, {"link", `<link href="SECRET123">`},
		{"uppercase", "<DIV>SECRET123</DIV>"}, {"hyphen tag", "<my-tag>SECRET123</my-tag>"},
		{"Unicode tag", "<é>SECRET123</é>"}, {"space before name", "< div>SECRET123</div>"},
		{"bare less than", "SECRET123 <"}, {"open tag", "<div SECRET123"},
		{"no closing angle", `<div class="SECRET123"`},
		{"missing end", "<div>SECRET123"}, {"stray end", "SECRET123</div>"},
		{"wrong nesting", "<div><span>SECRET123</div></span>"}, {"implicit close", "<p>SECRET123<p>next</p>"},
		{"void end", "SECRET123<br></br>"}, {"nonvoid self close", `<div id="SECRET123"/>`},
		{"spaced slash", `SECRET123<br/ >`}, {"end tag whitespace", "<div>SECRET123</div >"},
		{"end tag attribute", `<div>SECRET123</div id="x">`}, {"end tag case", "<div>SECRET123</DIV>"},
		{"unknown attribute", `<div secret="SECRET123"></div>`},
		{"event handler", `<div onclick="SECRET123"></div>`}, {"event prefix", `<div onanything="SECRET123"></div>`},
		{"uppercase attribute", `<div CLASS="SECRET123"></div>`},
		{"attribute digit", `<div class1="SECRET123"></div>`},
		{"attribute underscore", `<div data_x="SECRET123"></div>`},
		{"duplicate", `<div id="x" id="SECRET123"></div>`},
		{"no separator", `<div id="x"class="SECRET123"></div>`},
		{"single quotes", `<div id='SECRET123'></div>`}, {"unquoted", `<div id=SECRET123></div>`},
		{"space before equals", `<div id ="SECRET123"></div>`}, {"space after equals", `<div id= "SECRET123"></div>`},
		{"unclosed value", `<div id="SECRET123`}, {"boolean class", `<div class>SECRET123</div>`},
		{"raw less than in value", `<input value="<SECRET123>">`},
		{"backtick in value", "<input value=\"`SECRET123\">"},
		{"quote terminates value", `<input value="SECRET123"oops">`},
		{"tab in value", "<input value=\"SECRET123\t\">"}, {"newline in value", "<input value=\"SECRET123\n\">"},
		{"ampersand text", "SECRET123 & text"}, {"unknown entity", "SECRET123 &copy;"},
		{"entity case", "SECRET123 &AMP;"}, {"no semicolon", "SECRET123 &amp"},
		{"hex entity", "SECRET123 &#x27;"}, {"numeric entity", "SECRET123 &#60;"},
		{"other decimal apostrophe", "SECRET123 &#039;"},
		{"attribute entity", `<input value="SECRET123 &copy;">`},
		{"class whitespace", `<div class="SECRET123  x"></div>`}, {"class leading space", `<div class=" SECRET123"></div>`},
		{"class trailing space", `<div class="SECRET123 "></div>`}, {"class punctuation", `<div class="SECRET123.x"></div>`},
		{"empty class", `<div class="">SECRET123</div>`}, {"empty ID", `<div id="">SECRET123</div>`},
		{"ID list", `<div id="SECRET123 x"></div>`}, {"ID punctuation", `<div id="SECRET123.x"></div>`},
		{"external href", `<a href="https://example.com/SECRET123">Text</a>`},
		{"relative href", `<a href="wiki/SECRET123.md">Text</a>`},
		{"digit anchor", `<a href="#1SECRET123">Text</a>`}, {"empty anchor", `<a href="#">SECRET123</a>`},
		{"bad anchor char", `<a href="#SECRET123.x">Text</a>`},
		{"href outside a", `<div href="#SECRET123"></div>`}, {"src outside img", `<input src="SECRET123">`},
		{"image external", `<img src="https://example.com/SECRET123">`},
		{"span zero", `<td colspan="0">SECRET123</td>`}, {"span negative", `<td rowspan="-1">SECRET123</td>`},
		{"span overflow", `<td colspan="101">SECRET123</td>`}, {"span noninteger", `<td rowspan="SECRET123"></td>`},
		{"span decimal", `<th colspan="1.0">SECRET123</th>`}, {"span plus", `<th colspan="+1">SECRET123</th>`},
		{"span outside table cell", `<div colspan="1">SECRET123</div>`},
		{"input type", `<input type="SECRET123">`}, {"input submit", `<input type="submit" value="SECRET123">`},
		{"input image", `<input type="image" value="SECRET123">`},
		{"button submit", `<button type="submit">SECRET123</button>`}, {"button reset", `<button type="reset">SECRET123</button>`},
		{"type case", `<input type="TEXT" value="SECRET123">`},
		{"disabled outside control", `<div disabled>SECRET123</div>`},
		{"value outside control", `<p value="SECRET123"></p>`}, {"alt outside img", `<div alt="SECRET123"></div>`},
		{"placeholder outside input", `<button placeholder="SECRET123"></button>`},
		{"style nested", `<div><style>SECRET123</style></div>`},
		{"style entity", `<style>.SECRET123 { content: '&amp;'; }</style>`},
		{"style children", `<style><span>SECRET123</span></style>`},
		{"style unclosed", `<style>.SECRET123 { color: red; }`},
		{"style end case", `<style>.SECRET123{}</STYLE>`},
		{"depth", strings.Repeat("<div>", 65) + "SECRET123" + strings.Repeat("</div>", 65)},
		{"void depth", strings.Repeat("<div>", 64) + `<img alt="SECRET123">` + strings.Repeat("</div>", 64)},
	}
	for _, attr := range []string{"name", "form", "formaction", "action", "target", "srcset"} {
		cases = append(cases, struct{ name, src string }{"forbidden " + attr, `<input ` + attr + `="SECRET123">`})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := sanitizeMockup([]byte(tc.src), "Preview", 10000, 100)
			assertArtifactRejection(t, err)
			if out != nil {
				t.Fatal("rejected input returned output bytes")
			}
		})
	}
	for _, r := range []rune{0x061c, 0x200e, 0x200f, 0x202a, 0x202b, 0x202c, 0x202d, 0x202e, 0x2066, 0x2067, 0x2068, 0x2069} {
		_, err := sanitizeMockup([]byte("<p>SECRET123"+string(r)+"</p>"), "Preview", 1000, 100)
		assertArtifactRejection(t, err)
	}
}

func TestSanitizeMockupCSS(t *testing.T) {
	for i, css := range []string{
		`color: red; \SECRET123`, `/* SECRET123 */ color: red`, `content: '<SECRET123>'`,
		`background: url(SECRET123)`, `background: UrL(SECRET123)`, `background: image-set(SECRET123)`,
		`@import 'SECRET123'`, `@IMPORT 'SECRET123'`, `@font-face { font-family: SECRET123; }`,
		`@namespace SECRET123`, `width: expression(SECRET123)`, `behavior: SECRET123`,
		`-moz-binding: SECRET123`, `color: javascript:SECRET123`,
	} {
		for _, src := range []string{`<style>` + css + `</style>`, `<div style="` + css + `"></div>`} {
			t.Run(fmt.Sprintf("css-%d-%t", i, strings.HasPrefix(src, "<style>")), func(t *testing.T) {
				_, err := sanitizeMockup([]byte(src), "Preview", 10000, 100)
				assertArtifactRejection(t, err)
			})
		}
	}
	// CSS is checked after decoding attribute entities, so encoded markup cannot bypass it.
	_, err := sanitizeMockup([]byte(`<div style="content: '&lt;SECRET123>'"></div>`), "Preview", 10000, 100)
	assertArtifactRejection(t, err)
}

func TestSanitizeMockupImages(t *testing.T) {
	for _, image := range []struct {
		kind  string
		magic []byte
	}{
		{"png", []byte("\x89PNG\r\n\x1a\n")}, {"jpeg", []byte{0xff, 0xd8, 0xff}},
		{"gif", []byte("GIF87a")}, {"gif", []byte("GIF89a")},
	} {
		src := []byte(`<img src="` + mockupTestImage(image.kind, image.magic) + `">`)
		if _, err := sanitizeMockup(src, "Preview", 1000, len(image.magic)); err != nil {
			t.Fatal(err)
		}
		_, err := sanitizeMockup(src, "Preview", 1000, len(image.magic)-1)
		assertArtifactRejection(t, err)
	}
	for _, image := range []string{
		"data:image/png;base64,SECRET123", "data:image/png;base64,", "data:image/svg+xml;base64,SECRET123",
		mockupTestImage("png", []byte("SECRET123")), mockupTestImage("jpeg", []byte("GIF89aSECRET123")),
		mockupTestImage("gif", []byte("\x89PNG\r\n\x1a\nSECRET123")),
		"data:image/PNG;base64,iVBORw0KGgo=SECRET123", "data:image/png;charset=utf8;base64,SECRET123",
		"data:image/png;base64,iVBORw0KGgo",  // Missing standard padding.
		"data:image/png;base64,iVBORw0KGgp=", // Nonzero padding bits.
		"data:image/png;base64,iVBORw0KGgo_", // URL-safe alphabet.
		"data:image/png;base64,iVBORw0K Ggo=", "data:image/png;base64,iVBORw0K\nGgo=",
	} {
		_, err := sanitizeMockup([]byte(`<img alt="SECRET123" src="`+image+`">`), "Preview", 10000, 100)
		assertArtifactRejection(t, err)
	}
}

func TestSanitizeMockupLimitsTitleAndLocations(t *testing.T) {
	_, err := sanitizeMockup([]byte(artifactMarker), "Title", len(artifactMarker)-1, 0)
	assertArtifactRejection(t, err)
	// Re-escaping and forced button types may grow a fragment beyond the byte limit.
	for _, src := range []string{strings.Repeat("'", 30), "<button></button>"} {
		_, err := sanitizeMockup([]byte(src), "T", len(src), 0)
		assertArtifactRejection(t, err)
	}
	_, err = sanitizeMockup(nil, "T", -1, 0)
	assertArtifactRejection(t, err)
	for _, title := range []string{"", " ", "SECRET123\n", "SECRET123\t", "SECRET123\x00", "SECRET123\u202e", "<SECRET123>", "SECRET123 &amp;", "[x](http://SECRET123)"} {
		_, err := sanitizeMockup(nil, title, 10000, 100)
		assertArtifactRejection(t, err)
		if !strings.Contains(err.Error(), "mockup title") {
			t.Fatalf("title error needs context: %v", err)
		}
	}
	_, err = sanitizeMockup(nil, artifactMarker, 3, 0)
	assertArtifactRejection(t, err)
	_, err = sanitizeMockup([]byte("<p>safe</p>\né &SECRET123;"), "Preview", 10000, 100)
	assertArtifactRejection(t, err)
	if !strings.Contains(err.Error(), "line 2, column 3") {
		t.Fatalf("wrong error location: %v", err)
	}
	// The wrapper itself is not agent grammar and cannot be fed back as a fragment.
	doc, err := sanitizeMockup(nil, "Preview", 1000, 100)
	if err != nil {
		t.Fatal(err)
	}
	_, err = sanitizeMockup(doc, "Preview", 10000, 100)
	assertArtifactRejection(t, err)
}
