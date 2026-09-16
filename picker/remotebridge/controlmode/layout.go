package controlmode

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
)

type PaneCell struct {
	ID         string
	W, H, X, Y int
}

type Layout struct {
	W, H  int
	Panes []PaneCell
	// Floats holds the window's floating panes. Each cell is the INNER box
	// and equals the pane's usable size, so it feeds a renderer's dims
	// unconverted; only tmux's create/resize/move flags take the border
	// inset. Mirrored by the daemon as local floating panes.
	Floats []PaneCell
	// Raw is the v1 layout string for the TILED panes only, incl. checksum
	// prefix, whatever format the input was. select-layout keeps a window's
	// floats in place for a v1 string, while a v2 string must name every
	// local pane, floats included, or is rejected — and the mirror's local
	// floats never match the remote's one for one (a user's own float).
	Raw string
}

// ParseLayout parses a tmux layout string (window_layout / %layout-change
// payload) in either format tmux emits: v2 JSON, which a control client gets
// only after `refresh-client -f new-layouts` (tmux/tmux#5390), or v1. The
// string is self-describing — v2 starts with "{". Panes are returned
// depth-first in cell order — the order local panes must be created in,
// since select-layout assigns panes to cells positionally.
//
// Floats are leaves of the tree in both formats: v2 marks each with a "z"
// field; a next-3.8 remote predating v2 lists their ids again in a trailing
// "<WxH,X,Y,id[,...]>" section. A current tmux's v1 dump leaves floats out
// entirely. Float leaves are pruned out of the tree (collapsing any split
// left with a single child) and the checksum recomputed over the pruned body.
func ParseLayout(s string) (Layout, error) {
	if strings.HasPrefix(s, "{") {
		return parseLayoutV2(s)
	}
	// Strip the leading "<checksum>," prefix.
	_, body, ok := strings.Cut(s, ",")
	if !ok {
		return Layout{}, fmt.Errorf("layout: no checksum separator in %q", s)
	}
	p := &layoutParser{s: body}
	root, err := p.cell()
	if err != nil {
		return Layout{}, err
	}
	var floats []PaneCell
	if p.pos < len(p.s) && p.s[p.pos] == '<' {
		floats, err = p.floatSection()
		if err != nil {
			return Layout{}, err
		}
	}
	if p.pos != len(p.s) {
		return Layout{}, fmt.Errorf("layout: trailing data %q", p.s[p.pos:])
	}
	var out Layout
	out.W, out.H = root.w, root.h
	out.Floats = floats

	if len(floats) == 0 {
		out.Raw = s
	} else {
		floatIDs := make(map[string]bool, len(floats))
		for _, f := range floats {
			floatIDs[f.ID] = true
		}
		tiled := pruneFloats(root, floatIDs)
		if tiled == nil {
			return Layout{}, fmt.Errorf("layout: no tiled panes in %q", s)
		}
		var sb strings.Builder
		writeCell(tiled, &sb)
		out.Raw = fmt.Sprintf("%04x,%s", layoutChecksum(sb.String()), sb.String())
		root = tiled
	}

	collectLeaves(root, &out.Panes)
	if len(out.Panes) == 0 {
		return Layout{}, fmt.Errorf("layout: no panes in %q", s)
	}
	return out, nil
}

// maxLayoutDepth matches tmux's own v1 nesting limit; the v2 walk is
// recursive, and the tree comes from the remote.
const maxLayoutDepth = 1000

// jsonCell is one node of tmux's v2 "L" tree: "t" is "p" (pane) or "h"/"v"
// (split). Decoded in one pass: a per-level json.RawMessage decode re-scans
// every subtree once per ancestor, quadratic on a deep remote-supplied tree.
// Ignore must stay: without a field tagged "i" (the pane index),
// encoding/json's case-insensitive match puts that int into I.
type jsonCell struct {
	T      string     `json:"t"`
	W      *int       `json:"w"`
	H      *int       `json:"h"`
	X      *int       `json:"x"`
	Y      *int       `json:"y"`
	I      string     `json:"I"`
	C      []jsonCell `json:"c"`
	Z      *int       `json:"z"`
	Ignore int        `json:"i"`
}

func parseLayoutV2(s string) (Layout, error) {
	var doc struct {
		V *int     `json:"V"`
		L jsonCell `json:"L"`
	}
	dec := json.NewDecoder(strings.NewReader(s))
	if err := dec.Decode(&doc); err != nil {
		return Layout{}, fmt.Errorf("layout: bad v2 json in %q: %w", s, err)
	}
	// dec.More() reports false for a trailing "}" or "]".
	if _, err := dec.Token(); err != io.EOF {
		return Layout{}, fmt.Errorf("layout: trailing data after v2 json in %q", s)
	}
	if doc.V == nil || *doc.V != 2 {
		return Layout{}, fmt.Errorf("layout: unsupported v2 version %v in %q", doc.V, s)
	}

	var floats []PaneCell
	root, err := buildV2Node(doc.L, 0, &floats)
	if err != nil {
		return Layout{}, err
	}

	var out Layout
	out.W, out.H = root.w, root.h
	out.Floats = floats

	floatIDs := make(map[string]bool, len(floats))
	for _, f := range floats {
		floatIDs[f.ID] = true
	}
	tiled := pruneFloats(root, floatIDs)
	if tiled == nil {
		return Layout{}, fmt.Errorf("layout: no tiled panes in %q", s)
	}
	var sb strings.Builder
	writeCell(tiled, &sb)
	out.Raw = fmt.Sprintf("%04x,%s", layoutChecksum(sb.String()), sb.String())

	collectLeaves(tiled, &out.Panes)
	if len(out.Panes) == 0 {
		return Layout{}, fmt.Errorf("layout: no panes in %q", s)
	}
	return out, nil
}

// buildV2Node converts a decoded v2 cell into a *node, appending each float
// leaf to *floats in tree order.
func buildV2Node(c jsonCell, depth int, floats *[]PaneCell) (*node, error) {
	if depth > maxLayoutDepth {
		return nil, fmt.Errorf("layout: v2 nesting exceeds %d", maxLayoutDepth)
	}
	if c.W == nil || c.H == nil || c.X == nil || c.Y == nil {
		return nil, fmt.Errorf("layout: v2 cell missing w/h/x/y")
	}
	n := &node{w: *c.W, h: *c.H, x: *c.X, y: *c.Y}
	switch c.T {
	case "p":
		if len(c.C) != 0 {
			return nil, fmt.Errorf("layout: v2 pane has children")
		}
		if !validPaneID(c.I) {
			return nil, fmt.Errorf("layout: v2 pane id %q not %%+digits", c.I)
		}
		n.id = c.I
		if c.Z != nil {
			*floats = append(*floats, PaneCell{ID: n.id, W: n.w, H: n.h, X: n.x, Y: n.y})
		}
		return n, nil
	case "h", "v":
		if len(c.C) < 2 {
			return nil, fmt.Errorf("layout: v2 split has %d children, want >= 2", len(c.C))
		}
		if c.T == "h" {
			n.kind = '{'
		} else {
			n.kind = '['
		}
		for _, rawChild := range c.C {
			child, err := buildV2Node(rawChild, depth+1, floats)
			if err != nil {
				return nil, err
			}
			n.children = append(n.children, child)
		}
		return n, nil
	default:
		return nil, fmt.Errorf("layout: v2 unknown cell type %q", c.T)
	}
}

// validPaneID reports whether id is "%" followed by one or more digits, the
// same shape v1 leaf ids always have.
func validPaneID(id string) bool {
	if len(id) < 2 || id[0] != '%' {
		return false
	}
	for _, r := range id[1:] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

type node struct {
	w, h, x, y int
	id         string  // set on leaves
	kind       byte    // '{' (horizontal) or '[' (vertical); set on splits
	children   []*node // set on splits
}

type layoutParser struct {
	s   string
	pos int
}

// cell := WxH,X,Y [ , id | { children } | [ children ] ]
func (p *layoutParser) cell() (*node, error) {
	n := &node{}
	var err error
	if n.w, err = p.intUntil('x'); err != nil {
		return nil, err
	}
	if n.h, err = p.intUntil(','); err != nil {
		return nil, err
	}
	if n.x, err = p.intUntil(','); err != nil {
		return nil, err
	}
	// Y runs until one of , { [  } ]  or end.
	n.y, err = p.intUntilAny(",{[}]")
	if err != nil {
		return nil, err
	}
	if p.pos >= len(p.s) {
		return nil, fmt.Errorf("layout: unexpected end after cell")
	}
	switch p.s[p.pos] {
	case ',':
		p.pos++ // consume ','
		n.id = "%" + p.numRun()
	case '{':
		n.kind = '{'
		return p.split(n, '}')
	case '[':
		n.kind = '['
		return p.split(n, ']')
	}
	return n, nil
}

func (p *layoutParser) split(n *node, end byte) (*node, error) {
	p.pos++ // consume the already-matched opening delimiter
	for {
		c, err := p.cell()
		if err != nil {
			return nil, err
		}
		n.children = append(n.children, c)
		if p.pos >= len(p.s) {
			return nil, fmt.Errorf("layout: unterminated split")
		}
		switch p.s[p.pos] {
		case ',':
			p.pos++
		case end:
			p.pos++
			return n, nil
		default:
			return nil, fmt.Errorf("layout: bad split delimiter %q", p.s[p.pos])
		}
	}
}

func (p *layoutParser) numRun() string {
	start := p.pos
	for p.pos < len(p.s) && p.s[p.pos] >= '0' && p.s[p.pos] <= '9' {
		p.pos++
	}
	return p.s[start:p.pos]
}

func (p *layoutParser) intUntil(sep byte) (int, error) {
	start := p.pos
	for p.pos < len(p.s) && p.s[p.pos] != sep {
		p.pos++
	}
	if p.pos >= len(p.s) {
		return 0, fmt.Errorf("layout: expected %q", sep)
	}
	v, err := strconv.Atoi(p.s[start:p.pos])
	p.pos++ // consume sep
	return v, err
}

func (p *layoutParser) intUntilAny(seps string) (int, error) {
	start := p.pos
	for p.pos < len(p.s) && !strings.ContainsRune(seps, rune(p.s[p.pos])) {
		p.pos++
	}
	return strconv.Atoi(p.s[start:p.pos])
}

func collectLeaves(n *node, out *[]PaneCell) {
	if len(n.children) == 0 {
		*out = append(*out, PaneCell{ID: n.id, W: n.w, H: n.h, X: n.x, Y: n.y})
		return
	}
	for _, c := range n.children {
		collectLeaves(c, out)
	}
}

// floatSection parses the trailing "<cell[,cell...]>" section tmux next-3.8
// appends after the tiled split tree, one cell per floating pane. p.pos must
// be positioned at the opening '<'.
func (p *layoutParser) floatSection() ([]PaneCell, error) {
	p.pos++ // consume '<'
	var floats []PaneCell
	for {
		c, err := p.floatCell()
		if err != nil {
			return nil, err
		}
		floats = append(floats, c)
		if p.pos >= len(p.s) {
			return nil, fmt.Errorf("layout: unterminated float section")
		}
		switch p.s[p.pos] {
		case ',':
			p.pos++
		case '>':
			p.pos++
			return floats, nil
		default:
			return nil, fmt.Errorf("layout: bad float delimiter %q", p.s[p.pos])
		}
	}
}

// floatCell := WxH,X,Y,id — floats are always leaves, never splits.
func (p *layoutParser) floatCell() (PaneCell, error) {
	var c PaneCell
	var err error
	if c.W, err = p.intUntil('x'); err != nil {
		return c, err
	}
	if c.H, err = p.intUntil(','); err != nil {
		return c, err
	}
	if c.X, err = p.intUntil(','); err != nil {
		return c, err
	}
	if c.Y, err = p.intUntil(','); err != nil {
		return c, err
	}
	c.ID = "%" + p.numRun()
	return c, nil
}

// pruneFloats removes leaves whose id is in floatIDs from the tree rooted at
// n, collapsing any split left with a single surviving child (tmux layout
// splits always need >= 2 children). Returns nil if n has no surviving
// leaves.
func pruneFloats(n *node, floatIDs map[string]bool) *node {
	if len(n.children) == 0 {
		if floatIDs[n.id] {
			return nil
		}
		return n
	}
	var kept []*node
	for _, c := range n.children {
		if pc := pruneFloats(c, floatIDs); pc != nil {
			kept = append(kept, pc)
		}
	}
	switch len(kept) {
	case 0:
		return nil
	case 1:
		return kept[0]
	default:
		n.children = kept
		return n
	}
}

// writeCell serializes n (the pruned, tiled-only tree) back into tmux layout
// syntax, without the checksum prefix.
func writeCell(n *node, sb *strings.Builder) {
	fmt.Fprintf(sb, "%dx%d,%d,%d", n.w, n.h, n.x, n.y)
	if len(n.children) == 0 {
		fmt.Fprintf(sb, ",%s", strings.TrimPrefix(n.id, "%"))
		return
	}
	sb.WriteByte(n.kind)
	for i, c := range n.children {
		if i > 0 {
			sb.WriteByte(',')
		}
		writeCell(c, sb)
	}
	switch n.kind {
	case '{':
		sb.WriteByte('}')
	case '[':
		sb.WriteByte(']')
	}
}

// layoutChecksum reproduces tmux's layout_checksum (layout.c): a rotate-right
// running sum over the layout body (everything after the checksum prefix).
// select-layout validates this checksum against the body it's given and
// rejects the command on mismatch, so a reconstructed Raw string must carry
// a checksum recomputed the same way tmux computes it, not a copy of the
// original (which covered the un-pruned, float-bearing body).
func layoutChecksum(body string) uint16 {
	var csum uint16
	for i := 0; i < len(body); i++ {
		csum = (csum >> 1) + ((csum & 1) << 15)
		csum += uint16(body[i])
	}
	return csum
}
