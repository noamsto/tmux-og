package main

import (
	"bufio"
	"os"
	"strings"
	"testing"
)

var tsvUnescape = strings.NewReplacer(`\\`, "\x00", `\r`, "\r", `\n`, "\n", `\e`, "\x1b", `\t`, "\t")

// decodeTSV decodes the escape set \r \n \e \t \\ shared with the bats reader.
func decodeTSV(s string) string {
	return strings.ReplaceAll(tsvUnescape.Replace(s), "\x00", `\`)
}

func TestSanitizeTitle(t *testing.T) {
	f, err := os.Open("testdata/sanitize-title.tsv")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<20)
	rows := 0
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		cols := strings.Split(line, "\t")
		if len(cols) != 3 {
			t.Fatalf("bad row (%d cols): %q", len(cols), line)
		}
		rows++
		name, in, want := cols[0], decodeTSV(cols[1]), decodeTSV(cols[2])
		t.Run(name, func(t *testing.T) {
			if got := sanitizeTitle(in); got != want {
				t.Fatalf("sanitizeTitle = %q, want %q", got, want)
			}
		})
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if rows == 0 {
		t.Fatal("no rows in sanitize-title.tsv")
	}
}
