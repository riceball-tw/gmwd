package topic

import "testing"

func TestSlug(t *testing.T) {
	cases := map[string]string{
		"Fix flaky auth tests!":      "fix-flaky-auth-tests",
		"  ---  ":                    "topic",
		"Research: Go 1.27 & SQLite": "research-go-1-27-sqlite",
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-bbbbbb": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}
	for in, want := range cases {
		if got := Slug(in); got != want {
			t.Errorf("Slug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestValidate(t *testing.T) {
	ok := Topic{ID: "fix-auth", Kind: Solve, Prompt: "fix it", Repo: "/r"}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	bad := []Topic{
		{ID: "Bad ID", Kind: Research, Prompt: "x"},
		{ID: "x", Kind: Research, Prompt: " "},
		{ID: "x", Kind: "other", Prompt: "x"},
		{ID: "x", Kind: Solve, Prompt: "x"},
	}
	for _, b := range bad {
		if b.Validate() == nil {
			t.Errorf("Validate(%+v) = nil, want error", b)
		}
	}
}

func TestDefaults(t *testing.T) {
	tp := Topic{ID: "x"}
	if tp.ModelOrDefault() != "sonnet" || tp.Branch() != "gmwd/x" {
		t.Errorf("model %q branch %q", tp.ModelOrDefault(), tp.Branch())
	}
}
