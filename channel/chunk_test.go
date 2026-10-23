package channel

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSplitTextChunksPreservesUTF8AndExactText(t *testing.T) {
	cases := []struct {
		name string
		text string
		max  int
	}{
		{name: "ascii boundary", text: strings.Repeat("a", 21), max: 10},
		{name: "wide characters", text: strings.Repeat("汉🙂界", 9), max: 10},
		{name: "paragraph separators", text: strings.Repeat("first line\n\nsecond line\n", 3), max: 22},
		{name: "newline separators", text: strings.Repeat("line one\nline two\n", 3), max: 16},
		{name: "single rune wider than limit", text: "🙂a", max: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chunks := splitTextChunks(tc.text, tc.max)
			if got := strings.Join(chunks, ""); got != tc.text {
				t.Fatalf("reassembled text differs\n got: %q\nwant: %q", got, tc.text)
			}
			for i, chunk := range chunks {
				if !utf8.ValidString(chunk) {
					t.Errorf("chunk %d is invalid UTF-8: %q", i, chunk)
				}
				if len(chunk) > tc.max && !(tc.max < utf8.RuneLen([]rune(chunk)[0]) && len([]rune(chunk)) == 1) {
					t.Errorf("chunk %d is %d bytes, limit %d", i, len(chunk), tc.max)
				}
			}
		})
	}
	if got := splitTextChunks("unchanged", 0); len(got) != 1 || got[0] != "unchanged" {
		t.Fatalf("non-positive max chunks = %#v", got)
	}
}
