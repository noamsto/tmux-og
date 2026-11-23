#!/usr/bin/env bash
# Grow or shrink a tiled side pane back into its intended width band after a
# layout change moved it out of it.
#
# tmux keeps a pane at whatever width a resize left it: a carousel squeezed to
# 1 column stays 1 column, a dash that was 125 columns wide stays that wide
# after the window grows back. A pane declares its intent in the pane option
# @pane_pref_width "<pct>,<min>,<max>,<keep>": restore it to <pct>% of its row
# span clamped to [min, max] (max 0 = unbounded) whenever its width leaves
# [min, max], while leaving its left partner at least <keep> columns. An
# unstamped crew-dash-side pane (@pane_label) or image carousel
# (@claude_img_src) gets its default stamped here, so a pane launched before
# the option existed is covered too.
#   args: <target-window>   (the layout-change/resize hook passes #{window_id})
# The hook gate writes @pane_pref_sig before forking this run, so events at an
# unchanged geometry skip. The marker means "tried and could not fix": it is
# cleared on a successful restore, in the same command list as the resize,
# because the same action can squeeze a restored pane back to the identical
# geometry and a lingering marker would then suppress it. A resize that
# crushed another pane is rolled back in that list and the marker re-stamped
# with the restored geometry. It is also cleared when the window is or became
# zoomed; a mirror or grid window is never written. The guard's own resize
# unsets @pane_pref_hold again, because the after-resize-pane hook stamps a
# hold for every resize-pane and would otherwise freeze the layout it just set.
set -uo pipefail

# Width a pane without a preference must not be crushed under. The hook gate
# has no copy of the defaults below: it trips on the label or @claude_img_src.
MIN_COLS=20
SIDE_DASH_DEFAULT=32,50,80,80
CAROUSEL_DEFAULT=40,20,0,40
# Byte-identical to the hook gate's signature, so a rolled-back window matches
# the marker and the gate does not fork again (tests/pane-pref-width.bats pins it).
SIG_FMT='#{window_width}x#{window_height}:#{P:#{?pane_floating_flag,,#{pane_id}.#{pane_left}.#{pane_top}.#{pane_width}.#{pane_height} }}'
# A pane's own min, read from its option inside #{P:}.
MIN_FMT='#{s/^[^,]*,([^,]*),.*/\1/:@pane_pref_width}'

target=${1:-}
[[ -z $target ]] && exit 0

declare -A left top width height pref img label
order=()
wwidth=0
snapshot=
read_sig=
while IFS='|' read -r ww zoomed bridge grid pane floating pl pt pw ph opt src side layout sig; do
	# The window can change between the gate and this backgrounded run. Only a
	# zoom needs the marker cleared, so an unzoom back to this same geometry is
	# restored instead of suppressed; a mirror window is daemon-owned and never
	# written, and the gate writes no marker in a mirror or grid window.
	if [[ $zoomed == 1 || $bridge == 1 || $grid == 1 ]]; then
		[[ $zoomed == 1 && $bridge != 1 ]] && tmux set -wu -t "$target" @pane_pref_sig 2>/dev/null
		exit 0
	fi
	wwidth=$ww
	snapshot=$layout
	read_sig=$sig
	[[ $floating == 1 ]] && continue
	order+=("$pane")
	left[$pane]=$pl
	top[$pane]=$pt
	width[$pane]=$pw
	height[$pane]=$ph
	pref[$pane]=$opt
	img[$pane]=$src
	label[$pane]=$side
done < <(tmux list-panes -t "$target" -F "#{window_width}|#{window_zoomed_flag}|#{?@bridge_win,1,}|#{@crew_grid}|#{pane_id}|#{pane_floating_flag}|#{pane_left}|#{pane_top}|#{pane_width}|#{pane_height}|#{@pane_pref_width}|#{?@claude_img_src,1,}|#{?#{==:#{@pane_label},crew-dash-side},1,}|#{window_layout}|$SIG_FMT" 2>/dev/null)

# Resolve each pane's preference. A malformed value leaves the pane out of
# every set: it is neither restored nor protected.
declare -A pmin pmax ppct pkeep
valid=()
protected=
for p in "${order[@]}"; do
	value=${pref[$p]}
	if [[ -z $value ]]; then
		if [[ ${label[$p]} == 1 ]]; then
			value=$SIDE_DASH_DEFAULT
		elif [[ ${img[$p]} == 1 ]]; then
			value=$CAROUSEL_DEFAULT
		else
			((width[$p] >= MIN_COLS)) && protected+="<$p>"
			continue
		fi
		tmux set -p -t "$p" @pane_pref_width "$value" 2>/dev/null
	fi
	IFS=, read -r pct min max keep <<<"$value"
	[[ $pct =~ ^[0-9]+$ && $min =~ ^[0-9]+$ && $max =~ ^[0-9]+$ && $keep =~ ^[0-9]+$ ]] || continue
	((min >= 1 && pct >= 1 && pct <= 100)) || continue
	ppct[$p]=$pct
	pmin[$p]=$min
	pmax[$p]=$max
	pkeep[$p]=$keep
	valid+=("$p")
done

# Re-checked inside the apply list: resize-pane unzooms a zoomed window, and a
# zoom, mirror or grid flag can land between the read above and the resize.
# Only a zoom clears the marker (see above); a mirror window is never written.
# A changed signature means the snapshot is stale (a pane split since the read
# would make its select-layout fail), so the window is left for the next gate.
still_tiled="#{&&:#{&&:#{&&:#{!:#{window_zoomed_flag}},#{!:#{@bridge_win}}},#{!=:#{@crew_grid},1}},#{==:$SIG_FMT,$read_sig}}"
clear_marker="set -wu -t $target @pane_pref_sig"
clear_if_zoomed="if -F -t $target '#{&&:#{window_zoomed_flag},#{!:#{@bridge_win}}}' '$clear_marker'"

# resize-pane -x adjusts the pane's cell in its nearest left-right row and
# takes the columns from the next cell, or from the previous one when the cell
# is last in that row. That sibling lives in the layout tree, which geometry
# cannot see, so the donor rules below are a first guess that the restored
# check backs, rolling back a resize that crushed another pane:
#   - a pane at the window's right edge is last at every level, so resizing it
#     moves its left border and the columns come from its left;
#   - a partner directly left of the pane with the same rows ends a cell whose
#     next cell holds the pane, so resizing the partner moves their shared
#     border.
# Any other pane is declined, because resizing it would take the columns from
# the pane on its right; the gate's marker stops further forks here.
for c in "${valid[@]}"; do
	cw=${width[$c]} cmin=${pmin[$c]} cmax=${pmax[$c]}
	((cw < cmin || (cmax > 0 && cw > cmax))) || continue
	cl=${left[$c]} ct=${top[$c]} ch=${height[$c]}
	partner=
	for p in "${order[@]}"; do
		[[ $p == "$c" ]] && continue
		if ((left[$p] + width[$p] + 1 == cl && top[$p] == ct && height[$p] == ch)); then
			partner=$p
			break
		fi
	done
	if ((cl + cw == wwidth)); then
		resized=$c
		span=$wwidth
		[[ -n $partner ]] && span=$((width[$partner] + 1 + cw))
	elif [[ -n $partner ]]; then
		resized=$partner
		span=$((width[$partner] + 1 + cw))
	else
		continue
	fi
	tw=$((span * ppct[$c] / 100))
	((tw < cmin)) && tw=$cmin
	((cmax > 0 && tw > cmax)) && tw=$cmax
	cap=$((span - 1 - pkeep[$c]))
	((cap < cmin)) && continue
	((tw > cap)) && tw=$cap
	cols=$tw
	[[ $resized == "$partner" ]] && cols=$((span - 1 - tw))

	# Other pref panes that were at least their own min must stay there.
	pref_protected=
	for p in "${valid[@]}"; do
		[[ $p == "$c" ]] && continue
		((width[$p] >= pmin[$p])) && pref_protected+="<$p>"
	done

	# A restore is kept only when the pane is back in its band, no plain tiled
	# pane that was at least MIN_COLS in the snapshot is now under it, and no
	# other pref pane that was at least its own min is now under that min (a pane
	# already narrower is not the guard's doing); otherwise the window is rolled
	# back to the snapshot read above, which select-layout restores exactly.
	# Protected panes match by id through a glob over '<%1><%2>'.
	in_band="#{e|>=|:#{pane_width},$cmin}"
	((cmax > 0)) && in_band="#{&&:$in_band,#{e|<=|:#{pane_width},$cmax}}"
	crushed="#{P:#{?#{&&:#{m:*<#{pane_id}>*,$protected},#{&&:#{!:#{pane_floating_flag}},#{e|<|:#{pane_width},$MIN_COLS}}},x,}}"
	pref_crushed="#{P:#{?#{&&:#{m:*<#{pane_id}>*,$pref_protected},#{&&:#{!:#{pane_floating_flag}},#{e|<|:#{pane_width},$MIN_FMT}}},x,}}"
	restored="#{&&:#{&&:$in_band,#{!:$crushed}},#{!:$pref_crushed}}"

	# One pane per run: the resize changes the geometry, so the gate fires
	# again and the next run handles any other pane against fresh widths rather
	# than the ones read above. The rollback re-stamps the marker with the
	# restored geometry, so its own layout change forks nothing.
	tmux if -F -t "$c" "$still_tiled" \
		"resize-pane -t $resized -x $cols ; set -wu -t $target @pane_pref_hold ; if -F -t $c '$restored' { $clear_marker } { select-layout -t $target '$snapshot' ; set -wF -t $target @pane_pref_sig '$SIG_FMT' }" \
		"$clear_if_zoomed"
	exit 0
done
exit 0
