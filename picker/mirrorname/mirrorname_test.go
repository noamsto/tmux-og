package mirrorname

import (
	"bufio"
	"os"
	"strings"
	"testing"
)

func TestPartVectors(t *testing.T) {
	f, err := os.Open("testdata/vectors.tsv")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		in, want, ok := strings.Cut(line, "\t")
		if !ok {
			t.Fatalf("bad vector line %q", line)
		}
		if got := Part(in); got != want {
			t.Errorf("Part(%q) = %q, want %q", in, got, want)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
}

func TestLocal(t *testing.T) {
	if got := Local("user@h.lan", "a b"); got != "user_h_lan-a_b" {
		t.Fatalf("Local() = %q, want %q", got, "user_h_lan-a_b")
	}
}
