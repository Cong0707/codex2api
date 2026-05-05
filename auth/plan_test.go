package auth

import "testing"

func TestNormalizePlanType(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{input: "ChatGPT Plus", want: "plus"},
		{input: "PLUS", want: "plus"},
		{input: "pro", want: "pro"},
		{input: "prolite", want: "pro"},
		{input: "Pro 5x", want: "pro"},
		{input: "Pro 20x", want: "pro"},
		{input: "business", want: "team"},
		{input: "go", want: "team"},
		{input: "free", want: "free"},
		{input: "", want: ""},
	}

	for _, tt := range tests {
		got := NormalizePlanType(tt.input)
		if got != tt.want {
			t.Fatalf("NormalizePlanType(%q)=%q, want=%q", tt.input, got, tt.want)
		}
	}
}

func TestPreferPlanType(t *testing.T) {
	if got := PreferPlanType("free", "plus"); got != "plus" {
		t.Fatalf("PreferPlanType should choose plus over free, got=%q", got)
	}
	if got := PreferPlanType("team", "pro"); got != "team" {
		t.Fatalf("PreferPlanType should choose team over pro, got=%q", got)
	}
}

func TestPlanVariant(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{input: "prolite", want: "pro_5x"},
		{input: "Pro 5x", want: "pro_5x"},
		{input: "pro", want: "pro_20x"},
		{input: "Pro 20x", want: "pro_20x"},
		{input: "plus", want: "plus"},
		{input: "team", want: "team"},
	}

	for _, tt := range tests {
		got := PlanVariant(tt.input)
		if got != tt.want {
			t.Fatalf("PlanVariant(%q)=%q, want=%q", tt.input, got, tt.want)
		}
	}
}
