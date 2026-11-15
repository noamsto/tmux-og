package main

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestPRURLWellformed(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"https://github.com/a/b/pull/12", true},
		{"https://github.com/a/b/pull/12/", false},
		{"https://github.com/a/b/pulls/12", false},
		{"https://github.com/a/b/pull/1x", false},
		{"http://github.com/a/b/pull/12", false},
		{"https://github.com/a/b/c/pull/12", false},
		{"", false},
	}
	for _, c := range cases {
		if got := prURLWellformed(c.in); got != c.want {
			t.Errorf("prURLWellformed(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestSameRepoPR(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"first same-repo, flag removed",
			`[{"number":1,"isCrossRepository":true},{"number":2,"isCrossRepository":false,"title":"t"},{"number":3}]`,
			`[{"number":2,"title":"t"}]`},
		{"absent flag counts as same-repo", `[{"number":3,"title":"x"}]`, `[{"number":3,"title":"x"}]`},
		{"only cross-repo", `[{"number":1,"isCrossRepository":true}]`, `[]`},
		{"empty", `[]`, `[]`},
		{"invalid JSON", `not json`, `[]`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var got, want any
			if err := json.Unmarshal(sameRepoPR([]byte(c.in)), &got); err != nil {
				t.Fatalf("output is not JSON: %v", err)
			}
			if err := json.Unmarshal([]byte(c.want), &want); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("got %v, want %v", got, want)
			}
		})
	}
}
