package emailnorm

import "testing"

func TestNormalizeExistingRule(t *testing.T) {
	for input, want := range map[string]string{
		" User@Example.COM ": "user@example.com",
		"A\tB@Example.COM":   "ab@example.com",
		"Δ.Test@Example.COM": "δ.test@example.com",
	} {
		if got := Normalize(input); got != want {
			t.Fatalf("Normalize(%q) = %q, want %q", input, got, want)
		}
	}
}
