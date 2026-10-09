//go:build !windows

package cli

import "testing"

// TestReadLineSimpleCRLF covers the piped-input line parser: CRLF must count
// as a single terminator. Consuming only the '\r' left the '\n' to be parsed
// as an empty line, so every second prompt of a CRLF answer file silently
// took its default value.
func TestReadLineSimpleCRLF(t *testing.T) {
	cases := []struct {
		name  string
		lines string
		want  []string
	}{
		{"lf", "one\ntwo\n", []string{"one", "two"}},
		{"crlf", "1\r\n2\r\n", []string{"1", "2"}},
		{"crlf_blank_first", "\r\nvalue\r\n", []string{"def", "value"}},
		{"lone_cr", "mac\rstyle\r", []string{"mac", "style"}},
		{"cr_split_from_lf_is_not_merged_across_words", "a\rb\n", []string{"a", "b"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pipedBuf = []byte(tc.lines)
			for _, want := range tc.want {
				if got := readLineSimple("def"); got != want {
					t.Fatalf("readLineSimple() = %q, want %q", got, want)
				}
			}
		})
	}
	pipedBuf = nil
}
