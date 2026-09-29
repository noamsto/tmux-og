// Package tmuxformat rejects control-byte field delimiters in tmux -F/-p format
// strings embedded in picker Go sources. Mirrors tests/check-tmux-format-delimiters.sh.
package tmuxformat

import (
	"bufio"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Kind names a delimiter rule that fired.
type Kind int

const (
	RawControl Kind = iota
	EscapedControl
)

func (k Kind) String() string {
	switch k {
	case RawControl:
		return "raw control byte"
	case EscapedControl:
		return "escaped control byte"
	default:
		return "violation"
	}
}

// Violation is one rejected line in a scanned file.
type Violation struct {
	File string
	Line int
	Kind Kind
}

func (v Violation) Error() string {
	return fmt.Sprintf("%s:%d: %s in a tmux format — use '|'", v.File, v.Line, v.Kind)
}

// CheckLine applies the delimiter rules to one source line. Lines without "#{"
// are ignored so unrelated "\t" (e.g. strings.SplitN) never false-positive.
func CheckLine(line string) []Kind {
	if !strings.Contains(line, "#{") {
		return nil
	}

	var kinds []Kind

	body := strings.TrimLeft(line, " \t")
	for _, r := range body {
		if r >= 0x01 && r <= 0x1f || r == 0x7f {
			kinds = append(kinds, RawControl)
			break
		}
	}

	if strings.Contains(line, `\t`) || strings.Contains(line, `\n`) ||
		strings.Contains(line, `\x09`) || strings.Contains(line, `\x1f`) {
		kinds = append(kinds, EscapedControl)
	}

	return kinds
}

// ScanReader scans r line-by-line, reporting violations at 1-based line numbers.
func ScanReader(file string, r io.Reader) []Violation {
	var out []Violation
	sc := bufio.NewScanner(r)
	line := 0
	for sc.Scan() {
		line++
		for _, kind := range CheckLine(sc.Text()) {
			out = append(out, Violation{File: file, Line: line, Kind: kind})
		}
	}
	return out
}

// ScanGoTree walks root and scans every non-test .go file (*_test.go holds
// deliberate counterexamples and SplitN("\t") fixtures that must not pollute the gate).
func ScanGoTree(root string) ([]Violation, error) {
	var out []Violation
	scanned := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		scanned++
		f, err := os.Open(path) //nolint:gosec // path is built from trusted internal roots, not user input
		if err != nil {
			return err
		}
		defer f.Close() //nolint:errcheck // read-only file
		out = append(out, ScanReader(path, f)...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if scanned == 0 {
		return nil, fmt.Errorf("scanned no .go files under %s", root)
	}
	return out, nil
}

// FormatReport renders violations like the shell scanner's stderr.
func FormatReport(vs []Violation) string {
	var b strings.Builder
	for _, v := range vs {
		fmt.Fprintf(&b, "%s\n", v.Error())
	}
	if len(vs) > 0 {
		b.WriteString("\nA control byte is not a usable tmux field delimiter: tmux rewrites it to\n")
		b.WriteString(`"_" for any client without a UTF-8 locale, collapsing the row to one field.` + "\n")
	}
	return b.String()
}
