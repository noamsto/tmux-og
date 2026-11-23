#!/usr/bin/env bash
# Parent/child pane lifetime (#1222). A child carries @pane_parent <pane_id>.
#   tmux-pane-chain.sh adopt <child> <parent>   mark <child> as owned by <parent>
#   tmux-pane-chain.sh close-children <parent>  kill every descendant of <parent>
# Children are found by one list-panes -a scan, so a float stashed in another
# session counts. A @bridge_pane (daemon-owned mirror pane) is never closed or
# traversed. Fails closed: anything that is not a %N pane id does nothing.
set -euo pipefail

is_pane_id() { [[ ${1:-} =~ ^%[0-9]+$ ]]; }

adopt() {
	local child=${1:-} parent=${2:-}
	is_pane_id "$child" && is_pane_id "$parent" && [[ $child != "$parent" ]] || exit 1
	tmux set-option -p -t "$child" @pane_parent "$parent"
}

close_children() {
	local parent=${1:-} line id par bridge p
	is_pane_id "$parent" || exit 1
	local -A parent_of=()
	while IFS= read -r line; do
		IFS='|' read -r id par bridge <<<"$line"
		[[ -n $par && -z $bridge ]] && parent_of[$id]=$par
	done < <(tmux list-panes -a -F '#{pane_id}|#{@pane_parent}|#{@bridge_pane}')

	# Breadth-first from the parent. seen is seeded with it, so a cycle neither
	# loops nor kills it. order runs shallow to deep and is killed in reverse.
	local -A seen=([$parent]=1)
	local -a order=() frontier=("$parent") next=()
	while ((${#frontier[@]})); do
		next=()
		for id in "${!parent_of[@]}"; do
			[[ -z ${seen[$id]:-} ]] || continue
			for p in "${frontier[@]}"; do
				[[ ${parent_of[$id]} == "$p" ]] || continue
				seen[$id]=1
				next+=("$id")
				order+=("$id")
				break
			done
		done
		frontier=("${next[@]}")
	done

	local i
	for ((i = ${#order[@]} - 1; i >= 0; i--)); do
		tmux kill-pane -t "${order[i]}" 2>/dev/null || true
	done
}

case "${1:-}" in
adopt) adopt "${2:-}" "${3:-}" ;;
close-children) close_children "${2:-}" ;;
*)
	echo "usage: tmux-pane-chain adopt <child> <parent> | close-children <parent>" >&2
	exit 2
	;;
esac
