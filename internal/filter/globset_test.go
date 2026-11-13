package filter_test

import (
	"testing"

	"github.com/restic/restic/internal/filter"
)

// globsetMatchTests are the matches! and nmatches! rows of globset 0.4.18
// (crates/globset/src/glob.rs) that use options a case-insensitive Unix glob
// shares: default, CASEI and BSESC.
var globsetMatchTests = []struct {
	pattern string
	path    string
	match   bool
}{
	{"a", "a", true},
	{"a*b", "a_b", true},
	{"a*b*c", "abc", true},
	{"a*b*c", "a_b_c", true},
	{"a*b*c", "a___b___c", true},
	{"abc*abc*abc", "abcabcabcabcabcabcabc", true},
	{"a*a*a*a*a*a*a*a*a", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", true},
	{"a*b[xyz]c*d", "abxcdbxcddd", true},
	{"*.rs", ".rs", true},
	{"☃", "☃", true},

	{"some/**/needle.txt", "some/needle.txt", true},
	{"some/**/needle.txt", "some/one/needle.txt", true},
	{"some/**/needle.txt", "some/one/two/needle.txt", true},
	{"some/**/needle.txt", "some/other/needle.txt", true},
	{"**", "abcde", true},
	{"**", "", true},
	{"**", ".asdf", true},
	{"**", "/x/.asdf", true},
	{"some/**/**/needle.txt", "some/needle.txt", true},
	{"some/**/**/needle.txt", "some/one/needle.txt", true},
	{"some/**/**/needle.txt", "some/one/two/needle.txt", true},
	{"some/**/**/needle.txt", "some/other/needle.txt", true},
	{"**/test", "one/two/test", true},
	{"**/test", "one/test", true},
	{"**/test", "test", true},
	{"/**/test", "/one/two/test", true},
	{"/**/test", "/one/test", true},
	{"/**/test", "/test", true},
	{"**/.*", ".abc", true},
	{"**/.*", "abc/.abc", true},
	{"**/foo/bar", "foo/bar", true},
	{".*/**", ".abc/abc", true},
	{"test/**", "test/", true},
	{"test/**", "test/one", true},
	{"test/**", "test/one/two", true},
	{"some/*/needle.txt", "some/one/needle.txt", true},

	{"a[0-9]b", "a0b", true},
	{"a[0-9]b", "a9b", true},
	{"a[!0-9]b", "a_b", true},
	{"[a-z123]", "1", true},
	{"[1a-z23]", "1", true},
	{"[123a-z]", "1", true},
	{"[abc-]", "-", true},
	{"[-abc]", "-", true},
	{"[-a-c]", "b", true},
	{"[a-c-]", "b", true},
	{"[-]", "-", true},
	{"a[^0-9]b", "a_b", true},

	{"*hello.txt", "hello.txt", true},
	{"*hello.txt", "gareth_says_hello.txt", true},
	{"*hello.txt", "some/path/to/hello.txt", true},
	{"*hello.txt", `some\path\to\hello.txt`, true},
	{"*hello.txt", "/an/absolute/path/to/hello.txt", true},
	{"*some/path/to/hello.txt", "some/path/to/hello.txt", true},
	{"*some/path/to/hello.txt", "a/bigger/some/path/to/hello.txt", true},

	{"_[[]_[]]_[?]_[*]_!_", "_[_]_?_*_!_", true},

	{"aBcDeFg", "aBcDeFg", true},
	{"aBcDeFg", "abcdefg", true},
	{"aBcDeFg", "ABCDEFG", true},
	{"aBcDeFg", "AbCdEfG", true},

	{"a,b", "a,b", true},
	{",", ",", true},
	{"{a,b}", "a", true},
	{"{a,b}", "b", true},
	{"{**/src/**,foo}", "abc/src/bar", true},
	{"{**/src/**,foo}", "foo", true},
	{"{[}],foo}", "}", true},
	{"{foo}", "foo", true},
	{"{}", "", true},
	{"{,}", "", true},
	{"{*.foo,*.bar,*.wat}", "test.foo", true},
	{"{*.foo,*.bar,*.wat}", "test.bar", true},
	{"{*.foo,*.bar,*.wat}", "test.wat", true},
	{"foo{,.txt}", "foo.txt", true},
	{"foo{,.txt}", "foo", false},
	{"{a,b{c,d}}", "bc", true},
	{"{a,b{c,d}}", "bd", true},
	{"{a,b{c,d}}", "a", true},

	{`\[`, "[", true},
	{`\?`, "?", true},
	{`\*`, "*", true},
	{`\a`, "a", true},

	{"a*b*c", "abcd", false},
	{"abc*abc*abc", "abcabcabcabcabcabcabca", false},
	{"some/**/needle.txt", "some/other/notthis.txt", false},
	{"some/**/**/needle.txt", "some/other/notthis.txt", false},
	{"/**/test", "test", false},
	{"/**/test", "/one/notthis", false},
	{"/**/test", "/notthis", false},
	{"**/.*", "ab.c", false},
	{"**/.*", "abc/ab.c", false},
	{".*/**", "a.bc", false},
	{".*/**", "abc/a.bc", false},
	{"a[0-9]b", "a_b", false},
	{"a[!0-9]b", "a0b", false},
	{"a[!0-9]b", "a9b", false},
	{"[!-]", "-", false},
	{"*hello.txt", "hello.txt-and-then-some", false},
	{"*hello.txt", "goodbye.txt", false},
	{"*some/path/to/hello.txt", "some/path/to/hello.txt-and-then-some", false},
	{"*some/path/to/hello.txt", "some/other/path/to/hello.txt", false},
	{"a", "foo/a", false},
	{"./foo", "foo", false},
	{"**/foo", "foofoo", false},
	{"**/foo/bar", "foofoo/bar", false},
	{"/*.c", "mozilla-sha1/sha1.c", false},
	{"a[^0-9]b", "a0b", false},
	{"a[^0-9]b", "a9b", false},
	{"[^-]", "-", false},
	{"some/*/needle.txt", "some/needle.txt", false},
	{".*/**", ".abc", false},
	{"foo/**", "foo", false},
}

func TestGlobsetMatch(t *testing.T) {
	for _, test := range globsetMatchTests {
		glob, err := filter.ParseGlob(test.pattern)
		if err != nil {
			t.Errorf("ParseGlob(%q) returned error %v", test.pattern, err)
			continue
		}

		if glob.Match(test.path) != test.match {
			t.Errorf("glob %q against %q: want %v, got %v", test.pattern, test.path, test.match, !test.match)
		}
	}
}

func TestGlobMatchesAgainstBytesAndASCIICase(t *testing.T) {
	for _, test := range []struct {
		pattern string
		path    string
		match   bool
	}{
		{"*.ZIP", "/photos/sub/dir/archive.zip", true},
		{"**/@eadir/**", "/photos/@eaDir/thumb.jpg", true},
		{"[A-C]x", "bx", true},
		{"[^A]x", "ax", false},
		{"É", "é", false},
		{"é", "é", true},
		{"?", "é", false},
		{"??", "é", true},
		{"[é]", "é", false},
		{"a?b", "a\nb", true},
		{"a*", "a\xff\xfe", true},
		{"[\x01-~]", "\xc3", false},
	} {
		glob, err := filter.ParseGlob(test.pattern)
		if err != nil {
			t.Errorf("ParseGlob(%q) returned error %v", test.pattern, err)
			continue
		}

		if glob.Match(test.path) != test.match {
			t.Errorf("glob %q against %q: want %v, got %v", test.pattern, test.path, test.match, !test.match)
		}
	}
}

func TestParseGlobErrors(t *testing.T) {
	for _, test := range []struct {
		pattern string
		err     string
	}{
		{"[", "error parsing glob '[': unclosed character class; missing ']'"},
		{"[]", "error parsing glob '[]': unclosed character class; missing ']'"},
		{"[!", "error parsing glob '[!': unclosed character class; missing ']'"},
		{"[!]", "error parsing glob '[!]': unclosed character class; missing ']'"},
		{"[z-a]", "error parsing glob '[z-a]': invalid range; 'z' > 'a'"},
		{"[z--]", "error parsing glob '[z--]': invalid range; 'z' > '-'"},
		{"{a,b", "error parsing glob '{a,b': unclosed alternate group; missing '}' (maybe escape '{' with '[{]'?)"},
		{"{a,{b,c}", "error parsing glob '{a,{b,c}': unclosed alternate group; missing '}' (maybe escape '{' with '[{]'?)"},
		{"a,b}", "error parsing glob 'a,b}': unopened alternate group; missing '{' (maybe escape '}' with '[}]'?)"},
		{"{a,b}}", "error parsing glob '{a,b}}': unopened alternate group; missing '{' (maybe escape '}' with '[}]'?)"},
		{`a\`, `error parsing glob 'a\': dangling '\'`},
	} {
		_, err := filter.ParseGlob(test.pattern)
		if err == nil || err.Error() != test.err {
			t.Errorf("ParseGlob(%q): want error %q, got %v", test.pattern, test.err, err)
		}
	}
}
