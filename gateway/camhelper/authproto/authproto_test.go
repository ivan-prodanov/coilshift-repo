package authproto

import (
	"strings"
	"testing"
)

func TestParsePK(t *testing.T) {
	good := "04" + strings.Repeat("ab", 64)
	if b, err := ParsePK(good); err != nil || len(b) != 65 || b[0] != 4 {
		t.Fatalf("good pk: %x %v", b, err)
	}
	for name, s := range map[string]string{
		"short":      "04" + strings.Repeat("ab", 63),
		"long":       good + "00",
		"not hex":    "04" + strings.Repeat("zz", 64),
		"uppercase":  "04" + strings.Repeat("AB", 64),
		"compressed": "02" + strings.Repeat("ab", 64),
		"empty":      "",
	} {
		if _, err := ParsePK(s); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}
