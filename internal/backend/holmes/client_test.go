package holmes

import (
	"testing"
	"unicode/utf8"
)

func TestTruncateKeepsRunes(t *testing.T) {
	s := "재전송 급증" // 3-byte syllables
	for n := 1; n < len(s); n++ {
		got := truncate(s, n)
		if !utf8.ValidString(got) {
			t.Fatalf("truncate(%q, %d) = %q, split a rune", s, n, got)
		}
	}
	if got := truncate("abc", 5); got != "abc" {
		t.Errorf("short string = %q, want unchanged", got)
	}
}
