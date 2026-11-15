package aeh

import (
	"bytes"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

const artifactMarker = "SECRET123"

func assertArtifactRejection(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected rejection")
	}
	if strings.Contains(err.Error(), artifactMarker) {
		t.Fatalf("diagnostic leaked rejected content: %s", err)
	}
	if !strings.Contains(err.Error(), "line ") || !strings.Contains(err.Error(), "column ") {
		t.Fatalf("diagnostic has no location: %s", err)
	}
}

func TestDisplayMarkdown(t *testing.T) {
	positives := []string{
		"", "# A spec\n\nPassword must have 8 characters.\nA & B\n\tIndented text",
		"[Website](https://example.com/a?q=1&sort=asc#part)\n[Spec](wiki/features/cart/spec.md#AC-1)\n![Mockup](wiki/assets/cart.png)",
		"[Numbers](docs/v1.2/requirements.md#1_2) and [Nested [label]](https://example.com)",
		"![Local](wiki/a.png) [A `code` label](https://example.com/a)",
		"[Wrapped\nlabel](https://example.com/a) and ![wrapped\nimage](wiki/a.png)",
		"```html\n<div>&#123; [ref]: ![image](http://example.com)\n```\nSafe",
		"~~~~\n<SECRET123>\n   ~~~~~ \t\n[OK](wiki/a.md)",
		"````lang\n```\n<text>\n````\nDone",
		"~~~\n<unclosed fence is still fenced",
		"```\n```\u00a0\n<still inside>\n```",
		"`code without markup` &notAnEntity &123; [ordinary] text after prose",
		"日本語 and é and \ufffd",
	}
	for i, text := range positives {
		t.Run(fmt.Sprintf("positive-%d", i), func(t *testing.T) {
			if err := checkDisplayMarkdown(text, len(text)); err != nil {
				t.Fatalf("valid Markdown rejected: %v", err)
			}
		})
	}
	negatives := []struct{ name, text string }{
		{"invalid UTF-8", "SECRET123\xff"},
		{"NUL", "SECRET123\x00"},
		{"CRLF", "SECRET123\r\n"},
		{"CR", "SECRET123\r"},
		{"C0 control", "SECRET123\x01"},
		{"DEL", "SECRET123\x7f"},
		{"C1 control", "SECRET123\u0085"},
		{"raw HTML", "<SECRET123>"},
		{"autolink", "<https://example.com/SECRET123>"},
		{"inline code HTML", "`<SECRET123>`"},
		{"inline code entity", "`&SECRET123; &amp;`"},
		{"numeric reference", "SECRET123 &#"},
		{"hex reference", "SECRET123 &#x41;"},
		{"named reference", "SECRET123 &amp;"},
		{"reference definition", "[SECRET123]: https://example.com"},
		{"indented reference", "   [SECRET123]: docs/a.md"},
		{"nested quote reference", "![SECRET123][image]\n> [image]: https://example.com/a.png"},
		{"nested list reference", "![SECRET123][image]\n- [image]: https://example.com/a.png"},
		{"multiline reference", "![SECRET123][image]\n[image\n]: https://example.com/a.png"},
		{"inline code reference mark", "`[SECRET123]: docs/a.md`"},
		{"indented backtick fence", " ```\n<SECRET123>\n```"},
		{"indented tilde fence", "\t~~~\n<SECRET123>\n~~~"},
		{"not a fence", "```inline` <SECRET123>"},
		{"closed fence", "```\nallowed\n```\n<SECRET123>"},
		{"fence controls", "```\nSECRET123\x00\n```"},
		{"http", "[x](http://example.com/SECRET123)"},
		{"other scheme", "[x](mailto:SECRET123@example.com)"},
		{"empty HTTPS host", "[SECRET123](https:///a)"},
		{"bad port", "[x](https://example.com:SECRET123/a)"},
		{"external image", "![SECRET123](https://example.com/a.png)"},
		{"nested label image", "![x [SECRET123]](https://example.com/a.png)"},
		{"escaped bracket image", "![SECRET123 \\[](https://example.com/a.png)"},
		{"escaped closing bracket image", "![SECRET123 \\]](https://example.com/a.png)"},
		{"code bracket image", "![SECRET123 `[`](https://example.com/a.png)"},
		{"code closing bracket image", "![SECRET123 `]`](https://example.com/a.png)"},
		{"multiline image", "![SECRET123\nimage](https://example.com/a.png)"},
		{"link title", "[x](wiki/SECRET123.md \"title\")"},
		{"single quote", "[x](wiki/SECRET123'.md)"},
		{"angle target", "[x](<wiki/SECRET123.md>)"},
		{"backslash", "[x](wiki\\SECRET123.md)"},
		{"absolute path", "[x](/wiki/SECRET123.md)"},
		{"network path", "[x](//example.com/SECRET123)"},
		{"dot segment", "[x](wiki/./SECRET123.md)"},
		{"parent segment", "[x](wiki/../SECRET123.md)"},
		{"empty segment", "[x](wiki//SECRET123.md)"},
		{"trailing slash", "[x](wiki/SECRET123/)"},
		{"scheme in path", "[x](wiki/SECRET123:file)"},
		{"ASCII space", "[x](wiki/SECRET123 file.md)"},
		{"Unicode space", "[x](wiki/SECRET123\u00a0file.md)"},
		{"invalid anchor", "[x](wiki/a.md#SECRET123.bad)"},
		{"multiple anchors", "[x](wiki/a.md#SECRET123#bad)"},
		{"empty anchor", "[SECRET123](wiki/a.md#)"},
		{"anchor alone", "[SECRET123](#anchor)"},
		{"empty target", "[SECRET123]()"},
		{"unclosed target", "[x](wiki/SECRET123.md"},
		{"multiline target", "[x](wiki/SECRET123\na.md)"},
		{"nested target", "[x](wiki/SECRET123(a).md)"},
		{"unlabelled target", "SECRET123 ](javascript:bad)"},
		{"encoded traversal", "[x](wiki/%2e%2e/SECRET123.md)"},
		{"encoded backslash", "[x](wiki/%5cSECRET123.md)"},
		{"encoded space", "[x](wiki/%20SECRET123.md)"},
		{"invalid escape", "[x](wiki/%SECRET123.md)"},
	}
	for _, tc := range negatives {
		t.Run(tc.name, func(t *testing.T) {
			assertArtifactRejection(t, checkDisplayMarkdown(tc.text, 4096))
		})
	}
	assertArtifactRejection(t, checkDisplayMarkdown(artifactMarker, len(artifactMarker)-1))
	assertArtifactRejection(t, checkDisplayMarkdown("", -1))
	for _, r := range []rune{0x061c, 0x200e, 0x200f, 0x202a, 0x202b, 0x202c, 0x202d, 0x202e, 0x2066, 0x2067, 0x2068, 0x2069} {
		t.Run(fmt.Sprintf("bidi-%04x", r), func(t *testing.T) {
			assertArtifactRejection(t, checkDisplayMarkdown("```\nSECRET123"+string(r)+"\n```", 4096))
		})
	}
}

func TestDisplayLine(t *testing.T) {
	for _, s := range []string{"A title", " é ", "[Spec](wiki/spec.md)", "A & B"} {
		if err := checkDisplayLine(s, len(s)); err != nil {
			t.Fatalf("valid line rejected: %v", err)
		}
	}
	for _, s := range []string{"", "   ", "\u00a0", "SECRET123\n", "SECRET123\t", "SECRET123\r", "<SECRET123>", "&amp;SECRET123", "![SECRET123](https://example.com/a)"} {
		assertArtifactRejection(t, checkDisplayLine(s, 4096))
	}
	assertArtifactRejection(t, checkDisplayLine(artifactMarker, 2))
}

func TestArtifactLocationsAndFirstError(t *testing.T) {
	for _, tc := range []struct{ text, want string }{
		{"safe\né <SECRET123>", "line 2, column 3"},
		{"safe\né\x00SECRET123", "line 2, column 2"},
		{"safe\n<SECRET123>\n[bad](http://example.com)", "line 2, column 1"},
		{"safe\n[x](http://example.com/SECRET123)\n<bad>", "line 2, column 3"},
	} {
		err := checkDisplayMarkdown(tc.text, 4096)
		assertArtifactRejection(t, err)
		if !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("location = %v, want %s", err, tc.want)
		}
	}
}

func TestFindSecrets(t *testing.T) {
	var cases []struct{ text, kind string }
	for _, prefix := range []string{"", "RSA ", "EC ", "OPENSSH ", "DSA ", "ENCRYPTED "} {
		cases = append(cases, struct{ text, kind string }{"-----BEGIN " + prefix + "PRIVATE KEY-----", "private key"})
	}
	for _, prefix := range []string{"AKIA", "ASIA"} {
		cases = append(cases, struct{ text, kind string }{prefix + strings.Repeat("A1", 8), "AWS access key"})
	}
	for _, prefix := range []string{"ghp_", "gho_", "ghu_", "ghs_", "ghr_", "github_pat_"} {
		cases = append(cases, struct{ text, kind string }{prefix + strings.Repeat("a1B", 14), "GitHub token"})
	}
	for _, prefix := range []string{"xoxa-", "xoxb-", "xoxp-", "xoxr-", "xoxs-"} {
		cases = append(cases, struct{ text, kind string }{prefix + "123456789-abcdefgh", "Slack token"})
	}
	cases = append(cases, []struct{ text, kind string }{
		{"-----BEGIN PGP PRIVATE KEY BLOCK-----", "private key"},
		{"AIza" + strings.Repeat("a", 35), "Google API key"},
		{"sk_live_" + strings.Repeat("a", 16), "Stripe live key"},
		{"rk_live_" + strings.Repeat("a", 16), "Stripe live key"},
		{"sk-ant-" + strings.Repeat("a_1-", 5), "Anthropic API key"},
		{"sk-" + strings.Repeat("a_1-", 5), "OpenAI-style API key"},
		{"eyJ" + strings.Repeat("a", 10) + ".eyJ" + strings.Repeat("b", 10) + "." + strings.Repeat("c", 10), "JWT"},
		{"postgresql://user:SECRET123@db.example/database", "connection string with password"},
		{"custom+v1.2-scheme://user:SECRET123@server", "connection string with password"},
	}...)
	for i, tc := range cases {
		t.Run(fmt.Sprintf("%s-%d", tc.kind, i), func(t *testing.T) {
			// Invalid UTF-8 and NUL demonstrate that the scanner works on raw files.
			b := []byte("\xff\x00ordinary\nsecond line\n" + tc.text + "\nend")
			want := []secretFinding{{Line: 3, Kind: tc.kind}}
			if got := findSecrets(b); !reflect.DeepEqual(got, want) {
				t.Fatalf("findings = %v, want %v", got, want)
			}
			err := checkNoSecrets("artifact.html", b)
			if err == nil || !strings.Contains(err.Error(), "artifact.html:3: possible "+tc.kind) {
				t.Fatalf("missing redacted secret diagnostic: %v", err)
			}
			if strings.Contains(err.Error(), tc.text) || strings.Contains(err.Error(), artifactMarker) {
				t.Fatal("secret diagnostic leaked the matched value")
			}
		})
	}
	ordinary := []byte("Password must have 8 characters\nsk-\nuser@example.com\nhttps://example.com/a\npassword = example\n-----BEGIN PUBLIC KEY-----")
	if got := findSecrets(ordinary); len(got) != 0 {
		t.Fatalf("ordinary specification triggered findings: %v", got)
	}
	if err := checkNoSecrets("spec.md", ordinary); err != nil {
		t.Fatal(err)
	}
}

func TestSecretsThresholdsAndOrdering(t *testing.T) {
	for _, s := range []string{
		"AKIA" + strings.Repeat("A", 15), "ghp_" + strings.Repeat("a", 35),
		"github_pat_" + strings.Repeat("a", 21), "AIza" + strings.Repeat("a", 34),
		"sk_live_" + strings.Repeat("a", 15), "rk_live_" + strings.Repeat("a", 15),
		"sk-ant-" + strings.Repeat("a", 19), "sk-" + strings.Repeat("a", 19),
		"https://user@example.com/a", "https://user:@example.com/a", "HTTPS://user:pass@host", "akia" + strings.Repeat("A", 16),
	} {
		if got := findSecrets([]byte(s)); len(got) > 0 {
			t.Fatalf("short or nonmatching format triggered: %v", got)
		}
	}
	b := bytes.Repeat([]byte("sk-"+strings.Repeat("a", 20)+"\n"), 7)
	findings := findSecrets(b)
	if len(findings) != 7 || findings[6].Line != 7 {
		t.Fatalf("wrong number or order of findings: %v", findings)
	}
	err := checkNoSecrets("spec.md", b)
	if err == nil || strings.Count(err.Error(), "possible ") != 5 || strings.Contains(err.Error(), "spec.md:6:") {
		t.Fatalf("diagnostic must list only first five findings: %v", err)
	}
	b = []byte("https://user:pass@host sk-" + strings.Repeat("a", 20) + "\n-----BEGIN PRIVATE KEY-----")
	if got := findSecrets(b); !reflect.DeepEqual(got, []secretFinding{{1, "connection string with password"}, {1, "OpenAI-style API key"}, {2, "private key"}}) {
		t.Fatalf("findings must be in source order: %v", got)
	}
}
