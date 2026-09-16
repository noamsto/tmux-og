package daemon

import (
	"testing"

	"github.com/noamsto/tmux-og/picker/remotebridge/controlmode"
)

func TestParseLayoutNotice(t *testing.T) {
	const (
		layout  = "c195,80x24,0,0[80x12,0,0,0,80x11,0,13,1]"
		visible = "b25e,80x24,0,0,1"

		// Captured on the pinned binary (next-3.9) after
		// `refresh-client -f new-layouts`: layoutV2 is the unzoomed tree,
		// visibleV2 the single-pane view a zoom reports in field 2.
		layoutV2  = `{"V":2,"L":{"t":"v","w":100,"h":30,"x":0,"y":0,"c":[{"t":"p","w":100,"h":15,"x":0,"y":0,"a":true,"i":0,"I":"%0"},{"t":"p","w":100,"h":14,"x":0,"y":16,"i":1,"I":"%1"}]}}`
		visibleV2 = `{"V":2,"L":{"t":"p","w":100,"h":30,"x":0,"y":0,"a":true,"i":0,"I":"%0"}}`
	)

	tests := []struct {
		name       string
		raw        string
		wantOK     bool
		wantLayout string
		wantZoomed bool
	}{
		{
			name:       "4 fields, zoomed",
			raw:        "%layout-change @0 " + layout + " " + visible + " *Z",
			wantOK:     true,
			wantLayout: layout,
			wantZoomed: true,
		},
		{
			name:       "4 fields, not zoomed",
			raw:        "%layout-change @0 " + layout + " " + visible + " *",
			wantOK:     true,
			wantLayout: layout,
			wantZoomed: false,
		},
		{
			// The doubled '#' is window_flags' escaped spelling of the
			// activity flag (window_printable_flags' escape=1 form); the
			// alphabet admits either spelling, so this pins that it parses
			// alongside Z.
			name:       "4 fields, doubled activity flag plus zoomed",
			raw:        "%layout-change @0 " + layout + " " + visible + " ##Z",
			wantOK:     true,
			wantLayout: layout,
			wantZoomed: true,
		},
		{
			// A trailing space with nothing after it: window_printable_flags
			// reported no flags, so tmux never emits an empty 4th field and
			// bytes.Fields drops it, leaving the 3-field form.
			name:       "3 fields, flags empty",
			raw:        "%layout-change @0 " + layout + " " + visible + " ",
			wantOK:     true,
			wantLayout: layout,
			wantZoomed: false,
		},
		{
			name:   "2 fields",
			raw:    "%layout-change @0 " + layout,
			wantOK: false,
		},
		{
			// Real %layout-change captured with the new-layouts flag taken:
			// field 1 == field 2 when the window isn't zoomed, both the full
			// unzoomed tree.
			name:       "4 fields, v2 JSON, not zoomed",
			raw:        "%layout-change @0 " + layoutV2 + " " + layoutV2 + " *",
			wantOK:     true,
			wantLayout: layoutV2,
			wantZoomed: false,
		},
		{
			// Real %layout-change captured with the new-layouts flag taken,
			// for a zoomed window: field 1 stays the unzoomed tree, field 2
			// is the zoomed single-pane view, flags carry Z.
			name:       "4 fields, v2 JSON, zoomed",
			raw:        "%layout-change @0 " + layoutV2 + " " + visibleV2 + " *Z",
			wantOK:     true,
			wantLayout: layoutV2,
			wantZoomed: true,
		},
		{
			// JSON-shaped but missing the "V": tag layoutShaped keys on;
			// must not be mistaken for a v2 dump.
			name:   "4 fields, JSON without the V tag",
			raw:    `%layout-change @0 {"foo":1} {"foo":1} *`,
			wantOK: false,
		},
		{
			// A shifted 3-field line: field 2 arrived empty and everything
			// moved left, so what would have been the flags field lands in
			// Args[2] where a layout is expected.
			name:   "shifted 3 fields, third field is flags",
			raw:    "%layout-change @0 " + visible + " *Z",
			wantOK: false,
		},
		{
			name:   "4 fields, flags outside the alphabet",
			raw:    "%layout-change @0 " + layout + " " + visible + " bogus",
			wantOK: false,
		},
		{
			// Pins that a remote-controlled layout string can never reach
			// the local select-layout argv starting with '-': a layout field
			// beginning with a non-hex byte is rejected here, before it ever
			// becomes a command-line argument.
			name:   "4 fields, layout begins with a non-hex byte",
			raw:    "%layout-change @0 -000,80x24,0,0,1 -000,80x24,0,0,1 *",
			wantOK: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			l := controlmode.ParseLine(tc.raw)
			got, ok := parseLayoutNotice(l)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (notice %+v)", ok, tc.wantOK, got)
			}
			if !ok {
				return
			}
			if got.layout != tc.wantLayout {
				t.Errorf("layout = %q, want %q", got.layout, tc.wantLayout)
			}
			if got.zoomed != tc.wantZoomed {
				t.Errorf("zoomed = %v, want %v", got.zoomed, tc.wantZoomed)
			}
		})
	}
}
