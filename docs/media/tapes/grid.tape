# The window grid: every window in a multi-column, multi-row list whose labels
# give way as the client narrows, then spring back. og-demo-reflow (from the
# runner) attaches through an outer tmux and walks OG_DEMO_WIDTHS over the
# inner client. Stay above the grid's ~97-cell starved floor: below it the
# fixed per-slot badge/icon/ago columns overflow the client and the row clips.
Set Shell bash
Set FontFamily "JetBrainsMono Nerd Font Mono"
Set FontSize 16
Set Width 1400
Set Height 620
Set Padding 8
Set Framerate 15
Set Theme "Catppuccin Mocha"

Hide
Type "clear; OG_DEMO_WIDTHS='128 100 128' og-demo-reflow"
Enter
Sleep 4s
Show

Sleep 1.5s
Sleep 8s
