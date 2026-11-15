// Package ogcli is the og dispatcher: it resolves `og <noun> [<verb>]` to an
// absolute target path and execs it with the remaining arguments untouched.
package ogcli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Verb is one entry of the verb table. The table file (share/og/table.json) is
// a JSON array of these in display order; the order is the order og prints:
//
//		[{"verb": "remote open", "target": "/nix/store/.../og-remote-open",
//		  "summary": "Open a remote session", "model": "remote-open"}]
//
//	  - verb: one or two space-separated bare words, "<noun>" or "<noun> <verb>".
//	  - target: absolute path of the executable.
//	  - summary: one line shown in help and completions.
//	  - model: optional name of a Go-backed verb (see models.go) whose flags and
//	    --help text og shows; the target still gets its argv untouched.
type Verb struct {
	Verb    string `json:"verb"`
	Target  string `json:"target"`
	Summary string `json:"summary"`
	Model   string `json:"model,omitempty"`
}

// Tokens is the verb's words: one for a bare verb, two for noun and subverb.
func (v Verb) Tokens() []string { return strings.Split(v.Verb, " ") }

// Table is the validated, ordered verb table.
type Table struct {
	verbs  []Verb
	byName map[string]*Verb
}

// completionCommand is the hidden command that prints shell completions; the
// resolver routes it before the table, so no verb may take the name.
const completionCommand = "completion"

// Load reads and validates the table file at path.
func Load(path string) (*Table, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: the table path is the binary's own sibling file
	if err != nil {
		return nil, fmt.Errorf("read verb table: %w", err)
	}
	return ParseTable(data)
}

// ParseTable validates the JSON table.
func ParseTable(data []byte) (*Table, error) {
	var verbs []Verb
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&verbs); err != nil {
		return nil, fmt.Errorf("parse verb table: %w", err)
	}
	t := &Table{verbs: verbs, byName: make(map[string]*Verb, len(verbs))}
	for i := range t.verbs {
		v := &t.verbs[i]
		if err := validateVerb(v); err != nil {
			return nil, err
		}
		if _, dup := t.byName[v.Verb]; dup {
			return nil, fmt.Errorf("verb table: duplicate verb %q", v.Verb)
		}
		t.byName[v.Verb] = v
	}
	for _, v := range t.verbs {
		if v.Model != "" && t.hasSubverbs(v.Verb) {
			return nil, fmt.Errorf("verb table: %q has a model but also subverbs", v.Verb)
		}
	}
	return t, nil
}

func validateVerb(v *Verb) error {
	tokens := v.Tokens()
	if len(tokens) > 2 {
		return fmt.Errorf("verb table: %q has more than two words", v.Verb)
	}
	for _, tok := range tokens {
		if tok == "" || strings.ContainsAny(tok, " \t\n") {
			return fmt.Errorf("verb table: %q is not one or two single-spaced words", v.Verb)
		}
	}
	if tokens[0] == completionCommand {
		return fmt.Errorf("verb table: %q: %q is reserved for the hidden completion command", v.Verb, completionCommand)
	}
	if !filepath.IsAbs(v.Target) {
		return fmt.Errorf("verb table: %q: target %q is not an absolute path", v.Verb, v.Target)
	}
	if v.Model != "" {
		if _, ok := models[v.Model]; !ok {
			return fmt.Errorf("verb table: %q: unknown model %q", v.Verb, v.Model)
		}
	}
	return nil
}

// hasSubverbs reports whether any verb is spelled "<noun> <something>".
func (t *Table) hasSubverbs(noun string) bool {
	for _, v := range t.verbs {
		if strings.HasPrefix(v.Verb, noun+" ") {
			return true
		}
	}
	return false
}

// Verbs is the table in display order.
func (t *Table) Verbs() []Verb { return t.verbs }
