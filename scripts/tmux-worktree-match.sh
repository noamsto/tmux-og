#!/usr/bin/env bash
# Resolve which window shows a worktree, for the worktrunk post-switch hook.
# The @worktree tag is a hint, not the answer: it is minted from caller intent
# (see tmux-reconcile-window explicit mode) and can outlive the cd that earned
# it, at which point every switch to that worktree resolves to a window sitting
# somewhere else (#199). A pane's cwd is ground truth, so a tag only counts when
# a pane corroborates it — and a tag this matcher proves false is unset, which
# keeps "at most one window tagged W" true as of the last switch to W (post-remove
# still reads the tag, and kills the first window carrying it).
#
# Usage: tmux-worktree-match <worktree> <cur_session> <cur_window>
# Prints "<session>\t<window_index>\t<window_id>" for a match and nothing for no
# match; always exits 0, so the caller falls through to its own take-over /
# new-window chain rather than aborting the switch.
set -uo pipefail

w="${1:-}"
# An empty target degenerates the "$w/" boundary test to "/", which every
# absolute pane path satisfies — bail before touching tmux.
[[ -z $w ]] && exit 0
cs="${2:-}"
cw="${3:-}"

# Canonicalize the target once (bash builtins only — no realpath dependency); a
# pane may spell the worktree either way. Empty when $w does not resolve, which
# also suppresses tag clearing below: no ground truth, no mutation.
w_phys=$(cd -P -- "$w" 2>/dev/null && pwd -P)

# One query for the whole decision. list-windows would report only the active
# pane's path and so could not see a worktree living in a background pane.
#
# "|", not a tab: tmux sanitizes non-printable bytes out of command output when
# the locale is not UTF-8, so under LC_ALL=C (or POSIX) every tab comes back as
# "_" and nothing would ever parse — the matcher would silently match nothing
# and every `wt switch` would spawn a fresh window. "|" is printable and
# survives every locale (tmux-pr-enrich uses it for the same reason).
rows=$(tmux list-panes -a -f '#{!:#{pane_modal_flag}}' -F '#{session_name}|#{window_index}|#{window_id}|#{@worktree}|#{@bridge_win}|#{?window_modal_pane,#{pane_last},#{pane_active}}|#{pane_current_path}' 2>/dev/null) || exit 0
[[ -z $rows ]] && exit 0

# Values come in through the environment, not -v: POSIX has awk escape-process a
# -v value like a string literal, so a path holding a literal \t (or \\, \n …)
# would arrive mangled while $4/$7 — awk's own field split — stay raw, silently
# breaking the comparison. Git bans backslash in ref names, but an ancestor
# directory can still hold one. ENVIRON is POSIX and unprocessed.
plan=$(printf '%s\n' "$rows" | w="$w" wp="$w_phys" cs="$cs" cw="$cw" awk -F'|' '
	BEGIN {
		w = ENVIRON["w"]; wp = ENVIRON["wp"]
		cs = ENVIRON["cs"]; cw = ENVIRON["cw"]
	}
	function under(p, base) {
		if (base == "" || p == "") return 0
		if (p == base) return 1
		# A nested .worktrees/ checkout belongs to a different branch.
		return (index(p, base "/") == 1 && index(p, base "/.worktrees/") != 1)
	}
	function validates(p) { return under(p, w) || under(p, wp) }
	{
		# A "|" inside a path (vanishingly rare) would split a row into the wrong
		# fields. Fail closed: skip it, so at worst we decline to match and the
		# caller creates a window — never rank wrongly, never clear a wrong tag.
		if (NF != 7) next
		# Bridge mirror windows sit in the launcher repo, not remote content:
		# never a candidate, never cleared.
		if ($5 == "1") next
		key = $1 SUBSEP $2 SUBSEP $3
		if (!(key in rank)) {
			ord[++n] = key
			rank[key] = 99
			sess[key] = $1; widx[key] = $2; wid[key] = $3
			# Raw spelling only, deliberately (R2/R15 define the tag test as
			# @worktree == W). A window tagged with the physical spelling from git by
			# reconcile cwd mode just scores rank 2 via validates() instead of
			# rank 0, and is never spuriously cleared — degrades, never lies.
			tagged[key] = ($4 == w)
			readable[key] = 0
		}
		if ($7 != "") readable[key] = 1
		if (!validates($7)) next
		r = ($4 == w ? 0 : 2) + ($6 == "1" ? 0 : 1)
		# Rank 3 (untagged window, background pane) is not a candidate class: the
		# tag is what licenses trusting a pane the user cannot see.
		if (r == 3) next
		if (r < rank[key]) rank[key] = r
	}
	END {
		best = 99
		bestcur = 0
		for (i = 1; i <= n; i++) {
			key = ord[i]
			r = rank[key]
			if (r == 99) {
				if (!tagged[key]) continue
				# Tag with no corroborating pane: unreadable cwd defers to the
				# hint (ranked below everything), readable cwd falsifies it.
				if (readable[key]) {
					if (wp != "") print "CLEAR\t" wid[key]
					continue
				}
				r = 4
			}
			cur = (sess[key] == cs && widx[key] == cw)
			if (r < best || (r == best && cur && !bestcur)) {
				best = r
				bestcur = cur
				pick = key
			}
		}
		if (pick != "") print "MATCH\t" sess[pick] "\t" widx[pick] "\t" wid[pick]
	}
')

[[ -z $plan ]] && exit 0
while IFS=$'\t' read -r verb f1 f2 f3; do
	case $verb in
	CLEAR) tmux set-option -t "$f1" -w -u @worktree 2>/dev/null ;;
	MATCH) printf '%s\t%s\t%s\n' "$f1" "$f2" "$f3" ;;
	esac
done <<<"$plan"

exit 0
