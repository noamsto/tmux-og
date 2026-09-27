#!/usr/bin/env bats
# Regression guard for a command-injection class that has recurred twice in
# this repo's tmux.conf.nix: run-shell/if-shell format-expand their whole
# argument BEFORE handing it to `sh -c`, so a bare #{session_name} (etc.) in
# that argument is shell injection the moment the format's value contains a
# shell metacharacter (branch names, window titles, ... are all attacker/
# user-influenced). tmux's own fix is #{q:NAME}, which backslash-escapes the
# metacharacters sh -c would otherwise see. This file walks the EMITTED
# tmux.conf (a Nix store file, not the Nix source) and fails on every #{...}
# inside a shell-string argument that isn't one of tmux's two SHELL-quoting
# forms -- `#{q:NAME}` or `#{qs:NAME}` -- with a plain, unnested body. The four
# formats that can lose a word or expand a leading `~` must use bare `#{qs:}`:
# putting it inside shell quotes is weaker than `#{q:}`. The style modifiers
# `#{qe:}`/`#{qh:}` and the argument-escaping `#{qa:}` stay flagged: they double
# `#` for the format drawer, they do not quote for a shell.
# It also fails on a `%%`/`%N` command-prompt placeholder in a shell string.
#
# Scope, precisely:
#   - run-shell's command argument (after its flags) is a shell string.
#   - if-shell's FIRST argument is a shell string, UNLESS -F is present (then
#     it's a format, not shell, and is left alone).
#   - Everything else (if-shell -F's format, command-prompt -I seeds,
#     confirm-before -p text, split-window/new-window -c dir args, plain
#     status-format strings, ...) is not shell and must NOT be flagged.
#   - `{ ... }` brace blocks, and any string that is itself a nested tmux
#     command (e.g. set-hook's command argument, or an old-style if-shell
#     command/else-command string), hold further run-shell/if-shell calls
#     and are recursed into. The recursion predicate treats a quote as a
#     command boundary too, not just line-start/whitespace/;/{ -- a
#     string-form `if-shell "..." "set-hook -g -B '@a::1' 'run-shell ...'"`
#     nests run-shell behind a single quote, and without this the nested
#     shell string is never scanned at all.
#
# KNOWN BLIND SPOT: tmux config macros (`name=value` at line start, used as
# `$name`). tmux substitutes those at parse time, so a macro's body can BE a
# shell argument -- but this scanner reads the emitted text, where the use site
# says only `$is_vim`. Quoting inside a macro body is therefore on the author,
# not on this guard; the one such macro in this conf is commented accordingly.

WRAP_REQUIRED_FORMATS=(session_name hook_session_name pane_current_command pane_current_path)

is_wrap_required_format() {
	local format="$1" required
	for required in "${WRAP_REQUIRED_FORMATS[@]}"; do
		[[ $format == "$required" ]] && return 0
	done
	return 1
}

# --- tokenizer ---------------------------------------------------------
# Splits one logical tmux config line into tokens, matching tmux's own
# quoting: '...' is literal (no escapes), "..." processes backslash
# escapes, and adjacent quoted/bare fragments with no separating whitespace
# glue into one token (e.g. -I'#{@x}'). `{` and `}` are always their own
# token, since they delimit nested command lists.
# Result lands in the global TOKENS array.
TOKENS=()
tmux_tokenize() {
	TOKENS=()
	local s="$1"
	local n=${#s}
	local bs=$'\\'
	local i=0 c buf="" have=0
	while ((i < n)); do
		c="${s:i:1}"
		case "$c" in
		' ' | $'\t')
			if ((have)); then
				TOKENS+=("$buf")
				buf=""
				have=0
			fi
			i=$((i + 1))
			;;
		"'")
			have=1
			i=$((i + 1))
			while ((i < n)); do
				c="${s:i:1}"
				i=$((i + 1))
				[[ $c == "'" ]] && break
				buf+="$c"
			done
			;;
		'"')
			have=1
			i=$((i + 1))
			while ((i < n)); do
				c="${s:i:1}"
				if [[ $c == "$bs" ]] && ((i + 1 < n)); then
					buf+="${s:i+1:1}"
					i=$((i + 2))
					continue
				fi
				i=$((i + 1))
				[[ $c == '"' ]] && break
				buf+="$c"
			done
			;;
		'{' | '}')
			if ((have)); then
				TOKENS+=("$buf")
				buf=""
				have=0
			fi
			TOKENS+=("$c")
			i=$((i + 1))
			;;
		*)
			have=1
			buf+="$c"
			i=$((i + 1))
			;;
		esac
	done
	# `if`, not `((have)) &&`: an &&-list's false branch makes it the function's
	# exit status, which aborts the caller under errexit.
	if ((have)); then TOKENS+=("$buf"); fi
}

# --- violation bookkeeping ----------------------------------------------

CONF_VIOLATIONS=0

record_violation() {
	local lineno="$1" expansion="$2" excerpt="$3"
	CONF_VIOLATIONS=$((CONF_VIOLATIONS + 1))
	echo "${lineno}: ${expansion}: ${excerpt}"
}

# Scans one shell-command string (already stripped of its own outer quotes)
# for #{...} expansions and for %%/%N placeholders. Every expansion must be
# #{q:NAME} or #{qs:NAME}, with no further #{ nested inside NAME -- #{q:#{X}}
# does NOT quote, it's #{q:} of a body that is itself unquoted. The
# WRAP_REQUIRED_FORMATS must use bare #{qs:NAME}: single quotes become syntax
# from the expansion, and double quotes leave $ and backticks live.
# With wrap_only=1, only enforces the WRAP_REQUIRED_FORMATS rules. This lets
# #(...) keep its deliberate presentation-format exemption while sharing the
# shell quote-state tracker.
# Reports every offender, not just the first.
scan_shell_string() {
	local str="$1" lineno="$2" quote_state="${3:-out}" wrap_only="${4:-0}"
	local n=${#str}
	local excerpt="${str:0:80}"
	local i=0
	while ((i < n)); do
		if [[ ${str:i:2} == '#{' ]]; then
			local start=$i depth=1 j=$((i + 2)) nested=0
			while ((j < n && depth > 0)); do
				if [[ ${str:j:2} == '#{' ]]; then
					nested=1
					depth=$((depth + 1))
					j=$((j + 2))
				elif [[ ${str:j:1} == '}' ]]; then
					depth=$((depth - 1))
					j=$((j + 1))
				else
					j=$((j + 1))
				fi
			done
			local group="${str:start:$((j - start))}"
			if [[ $group == '#{?'* ]]; then
				# A conditional emits one of its BRANCHES; the condition itself is
				# only tested and never reaches the shell. So recurse past the first
				# top-level comma and hold the branches to the same rule. This is
				# what lets `#{?client_name,--client #{q:client_name},}` pass: it is
				# the empty-safe way to emit a flag and its value together or
				# neither, which a bare #{q:} cannot do (empty = zero words).
				local body="${group:3:$((${#group} - 4))}"
				local k=0 d=0 bn=${#body}
				while ((k < bn)); do
					if [[ ${body:k:2} == '#{' ]]; then
						d=$((d + 1))
						k=$((k + 2))
					elif [[ ${body:k:1} == '}' ]]; then
						d=$((d - 1))
						k=$((k + 1))
					elif [[ ${body:k:1} == ',' ]] && ((d == 0)); then
						break
					else
						k=$((k + 1))
					fi
				done
				((k < bn)) && scan_shell_string "${body:$((k + 1))}" "$lineno" "$quote_state" "$wrap_only"
			else
				local modifier="" format=""
				if [[ $group == '#{qs:'* ]]; then
					modifier="qs"
					format="${group:5:$((${#group} - 6))}"
				elif [[ $group == '#{q:'* ]]; then
					modifier="q"
					format="${group:4:$((${#group} - 5))}"
				fi
				if ((nested)); then
					if ((wrap_only)); then
						local required
						for required in "${WRAP_REQUIRED_FORMATS[@]}"; do
							if [[ $group == *"#{$required}"* || $group == *"#{q:$required}"* || $group == *"#{qs:$required}"* ]]; then
								record_violation "$lineno" "$group" "$excerpt"
								break
							fi
						done
					else
						record_violation "$lineno" "$group" "$excerpt"
					fi
				elif [[ -z $modifier ]]; then
					local bare_format="${group:2:$((${#group} - 3))}"
					# A style modifier (qe:/qh:/qa:) doesn't quote for a shell, so a
					# wrap-required format wrapped in one must still be flagged in
					# wrap_only mode -- strip it before the comparison below.
					local wrap_check_format="$bare_format"
					case $wrap_check_format in
					qe:* | qh:* | qa:*) wrap_check_format="${wrap_check_format#*:}" ;;
					esac
					if ((wrap_only)) && is_wrap_required_format "$wrap_check_format"; then
						record_violation "$lineno" "$group" "$excerpt"
					elif ((! wrap_only)); then
						record_violation "$lineno" "$group" "$excerpt"
					fi
				elif [[ $modifier == qs && $quote_state != out ]]; then
					record_violation "$lineno" "$group" "$excerpt"
				elif [[ $modifier == q ]] && is_wrap_required_format "$format"; then
					record_violation "$lineno" "$group" "$excerpt"
				fi
			fi
			i=$j
		elif [[ ${str:i:2} == '%%' || ${str:i:2} =~ ^%[1-9]$ ]]; then
			# A command-prompt template placeholder is substituted AFTER format
			# expansion, so no #{q:...}/#{qs:...} can ever reach it -- it is
			# unprotectable in a shell string by construction. And %N inside a
			# run-shell string is NQ (cmd.c:869-871): raw, unescaped insertion,
			# strictly worse than the '%%' it would replace. The safe shape is to
			# pass the prompt result as a run-shell ARGUMENT and reference it
			# #{qs:N}, which puts it back under the rule above.
			#
			# This over-flags rather than under-flags: the scanner cannot see
			# whether a given run-shell sits under a command-prompt, so a literal
			# %% in an unrelated shell string (a printf format, say) would trip it
			# too. Zero such cases exist in the conf today; if you hit one, that is
			# the reason, and the fix is to rewrite the format, not to weaken this.
			((wrap_only)) || record_violation "$lineno" "${str:i:2}" "$excerpt"
			i=$((i + 2))
		else
			local c="${str:i:1}"
			case "$quote_state:$c" in
			out:\\)
				i=$((i + 2))
				continue
				;;
			out:\') quote_state=single ;;
			out:\") quote_state=double ;;
			single:\') quote_state=out ;;
			double:\\)
				# In double quotes, a backslash only protects $, `, ", \\, and a
				# newline. Any other following byte leaves the quote state unchanged.
				local next="${str:i+1:1}"
				if [[ $next == '$' || $next == '`' || $next == '"' || $next == \\ || $next == $'\n' ]]; then
					i=$((i + 2))
					continue
				fi
				;;
			double:\") quote_state=out ;;
			esac
			i=$((i + 1))
		fi
	done
}

# Walks a token array. run-shell's argument and if-shell's non-F first
# argument get scanned as shell. `{ ... }` blocks recurse. Any other token
# that is ITSELF a nested tmux command line starting with run-shell/if-shell
# (set-hook's command argument; an old-style if-shell command/else-command
# string) recurses too -- everything else is inert and left alone.
walk_tokens() {
	local lineno="$1"
	shift
	local -a _toks=("$@")
	local n=${#_toks[@]}
	local i=0
	while ((i < n)); do
		local tok="${_toks[i]}"
		case "$tok" in
		run-shell)
			i=$((i + 1))
			local has_c=0
			while ((i < n)) && [[ ${_toks[i]} == -* ]]; do
				case "${_toks[i]}" in
				-d | -t | -c) i=$((i + 2)) ;;
				-C)
					has_c=1
					i=$((i + 1))
					;;
				*) i=$((i + 1)) ;;
				esac
			done
			if ((i < n)); then
				if ((has_c)); then
					# -C's argument is a tmux command, not a shell string -- run-shell
					# -C hands it straight to cmd_parse, never through sh -c -- so
					# recurse into it as a command line (which can itself hold a
					# nested run-shell) instead of scanning it for #{...}. This
					# recursion polices shell sinks nested inside that command; a
					# bare format in -C's OWN argument gets re-parsed by tmux
					# itself (e.g. a menu -T title), which is a tmux-side re-parse
					# hazard this scanner does not police -- out of scope here.
					local arg="${_toks[i]}"
					tmux_tokenize "$arg"
					walk_tokens "$lineno" "${TOKENS[@]}"
				else
					scan_shell_string "${_toks[i]}" "$lineno"
				fi
				i=$((i + 1))
			fi
			;;
		if-shell)
			i=$((i + 1))
			local has_f=0
			while ((i < n)) && [[ ${_toks[i]} == -* ]]; do
				case "${_toks[i]}" in
				-F)
					has_f=1
					i=$((i + 1))
					;;
				-t) i=$((i + 2)) ;;
				*) i=$((i + 1)) ;;
				esac
			done
			if ((i < n)); then
				((has_f)) || scan_shell_string "${_toks[i]}" "$lineno"
				i=$((i + 1))
			fi
			;;
		'{')
			local depth=1 j=$((i + 1))
			while ((j < n && depth > 0)); do
				case "${_toks[j]}" in
				'{') depth=$((depth + 1)) ;;
				'}') depth=$((depth - 1)) ;;
				esac
				j=$((j + 1))
			done
			local close=$((j - 1))
			walk_tokens "$lineno" "${_toks[@]:$((i + 1)):$((close - i - 1))}"
			i=$j
			;;
		*)
			# Anywhere in the string, not just at its start: a command/else-command
			# argument may chain with ';' ("set -g x y ; run-shell '...'"), and
			# anchoring to ^ would let that bypass the guard entirely.
			#
			# The boundary set is exactly the bytes that can precede a nested
			# command AND that tmux_tokenize strips on the next pass, which is
			# what lets this recursion terminate. A quote qualifies: set-hook
			# -g -B's command argument is single-quoted, so a nested run-shell
			# sits behind one. '(' does NOT belong here -- tmux has no ( )
			# grouping, so `(run-shell ...)` is parsed as a command named
			# "(run-shell" and rejected outright ("unknown command"), making the
			# construct unreachable; and since tmux_tokenize never strips '(',
			# adding it made a matching token retokenize to itself forever.
			if [[ $tok =~ (^|[[:space:]\;{\'\"])(run-shell|if-shell)([[:space:]]|$) ]]; then
				tmux_tokenize "$tok"
				walk_tokens "$lineno" "${TOKENS[@]}"
			fi
			i=$((i + 1))
			;;
		esac
	done
}

# --- logical lines --------------------------------------------------------
# Both rules below run over the SAME joined stream, so neither can miss a
# statement the other sees. Fills LOGICAL_TEXT/LOGICAL_START in step.
#
# Continuation follows tmux's lexer (cmd-parse.y counts consecutive
# backslashes): only an ODD trailing count continues the line. Treating any
# trailing backslash as a continuation glues the next statement on as one inert
# token, and the command word on it is then never scanned.
LOGICAL_TEXT=()
LOGICAL_START=()
join_logical_lines() {
	LOGICAL_TEXT=()
	LOGICAL_START=()
	local conf="$1"
	local lineno=0 logical="" start=0 line trimmed t nbs
	while IFS= read -r line || [[ -n $line ]]; do
		lineno=$((lineno + 1))
		if [[ -z $logical ]]; then
			trimmed="${line#"${line%%[![:space:]]*}"}"
			[[ -z $trimmed || $trimmed == '#'* ]] && continue
			start=$lineno
		fi
		t="$line"
		nbs=0
		while [[ $t == *\\ ]]; do
			t="${t%\\}"
			nbs=$((nbs + 1))
		done
		if ((nbs % 2 == 1)); then
			logical+="${line%\\}"
			continue
		fi
		logical+="$line"
		LOGICAL_TEXT+=("$logical")
		LOGICAL_START+=("$start")
		logical=""
	done <"$conf"
	# A file ending mid-continuation still holds a statement; dropping it would
	# report a clean zero for text nobody scanned.
	if [[ -n $logical ]]; then
		LOGICAL_TEXT+=("$logical")
		LOGICAL_START+=("$start")
	fi
}

scan_conf() {
	join_logical_lines "$1"
	local i
	for i in "${!LOGICAL_TEXT[@]}"; do
		tmux_tokenize "${LOGICAL_TEXT[i]}"
		walk_tokens "${LOGICAL_START[i]}" "${TOKENS[@]}"
	done
}

# Entry point for tests: prints every violation, returns 1 if any were found.
check_conf_quoting() {
	CONF_VIOLATIONS=0
	scan_conf "$1"
	((CONF_VIOLATIONS == 0))
}

# --- #(...) formats -------------------------------------------------------
# `#(cmd)` in a status/window format also runs through a shell, and is
# format-expanded first, so it is the same injection mechanism as run-shell.
# The blanket rule above is deliberately NOT applied here: these formats also
# carry presentation-only options (@thm_*, @icon_*, start_time) that are
# generated locally and still sit in shell single quotes, and unquoting those
# would change word counts on the 1s status path for no security gain.
#
# What must never appear here unquoted is the identity set below — the values
# an untrusted REMOTE host can steer, which is the trust boundary this whole
# guard exists for (a bridged session is named from the remote's session list).
# scan_shell_string's wrap-only mode holds every wrap-required format to bare
# #{qs:} without widening this check to presentation-only formats.
IDENTITY_FORMATS=(session_name hook_session_name window_name pane_title)

check_hashparen_identity() {
	CONF_VIOLATIONS=0
	join_logical_lines "$1"
	local idx line lineno n i depth j body name
	for idx in "${!LOGICAL_TEXT[@]}"; do
		line="${LOGICAL_TEXT[idx]}"
		lineno="${LOGICAL_START[idx]}"
		n=${#line}
		i=0
		while ((i < n)); do
			if [[ ${line:i:2} != '#(' ]]; then
				i=$((i + 1))
				continue
			fi
			# Balance parens to find the close. Stopping at the FIRST ')' would
			# truncate the body at any literal one -- an rgba()/regex argument --
			# and hide a bare format after it. Over-scanning can only cost a false
			# positive; under-scanning costs exactly the miss this guard exists for.
			depth=1
			j=$((i + 2))
			while ((j < n && depth > 0)); do
				case "${line:j:1}" in
				'(') depth=$((depth + 1)) ;;
				')') depth=$((depth - 1)) ;;
				esac
				j=$((j + 1))
			done
			body="${line:$((i + 2)):$((j - i - 3))}"
			for name in "${IDENTITY_FORMATS[@]}"; do
				if ! is_wrap_required_format "$name" && [[ $body == *"#{$name}"* ]]; then
					record_violation "$lineno" "#{$name}" "${body:0:80}"
				fi
			done
			scan_shell_string "$body" "$lineno" out 1
			i=$j
		done
	done
	((CONF_VIOLATIONS == 0))
}

# --- fixtures --------------------------------------------------------

@test "flags every known-bad bare/unquoted #{} pattern" {
	cat >"$BATS_TEST_TMPDIR/bad.conf" <<'EOF'
set-hook -g after-new-window 'run-shell -b "/bin/x #{session_name}"'
bind S run-shell '/bin/x "#{session_name}"'
bind Z if-shell '/bin/x #{pane_current_command}' foo bar
bind Q if-shell -F '#{@gate}' { run-shell "/bin/x #{session_name}" }
run-shell "/bin/x #{q:#{session_name}}"
bind V if-shell '/bin/gate' 'set -g @x y ; run-shell "/bin/x #{window_name}"' 'display-message no'
bind U run-shell '/bin/x #{?client_name,--client #{client_name},}'
bind E run-shell '/bin/x #{qe:@window_bridge_name}'
bind N run-shell -C "display-menu Foo f {run-shell '/bin/x #{session_name}'}"
EOF
	run check_conf_quoting "$BATS_TEST_TMPDIR/bad.conf"
	[ "$status" -eq 1 ]
	[[ $output == *'#{session_name}'* ]]
	[[ $output == *'#{pane_current_command}'* ]]
	[[ $output == *'#{q:#{session_name}}'* ]]
	local count
	count=$(echo "$output" | grep -c .)
	# the last fixture is a run-shell chained after a ';' inside an old-style
	# if-shell command argument -- it must not slip past the recursion
	[[ $output == *'#{window_name}'* ]]
	# a conditional's BRANCH is emitted into the shell, so it is held to the
	# same rule even though the condition itself never reaches sh
	[[ $output == *'#{client_name}'* ]]
	# #{qe:} is STYLE quoting (format_quote_style doubles '#' only) -- accepting
	# #{qs:} must not widen the predicate to every #{q*:} modifier
	[[ $output == *'#{qe:@window_bridge_name}'* ]]
	# run-shell -C's argument is a tmux command, and a run-shell NESTED inside
	# that command is still reachable from a shell -- the recursion into -C's
	# argument must keep walking it as a command line, not stop at scanning it
	[ "$count" -eq 9 ]
}

@test "flags a bare format in a run-shell nested behind a quote in a string-form branch" {
	# set-hook -g -B's command argument is single-quoted, so the byte before
	# the nested run-shell is ' rather than whitespace/;/{ -- this is the
	# recursion blind spot the predicate widening closes. Before the widening
	# this fixture was NOT flagged at all.
	cat >"$BATS_TEST_TMPDIR/quoted-nest.conf" <<'EOF'
if-shell "true" "set-hook -g -B '@a::1' 'run-shell -b \"/bin/x #{session_name}\"'"
EOF
	run check_conf_quoting "$BATS_TEST_TMPDIR/quoted-nest.conf"
	[ "$status" -eq 1 ]
	[[ $output == *'#{session_name}'* ]]
	[ "$(echo "$output" | grep -c .)" -eq 1 ]
}

@test "flags a %% or %N command-prompt placeholder inside a shell string" {
	cat >"$BATS_TEST_TMPDIR/pct.conf" <<'EOF'
bind , command-prompt { run-shell "/bin/ctl rename #{q:@bridge_pane} '%%'" }
bind . command-prompt { run-shell "/bin/ctl rename #{q:@bridge_pane} #{q:1}%2" }
EOF
	run check_conf_quoting "$BATS_TEST_TMPDIR/pct.conf"
	[ "$status" -eq 1 ]
	[[ $output == *'%%'* ]]
	[[ $output == *'%2'* ]]
	[ "$(echo "$output" | grep -c .)" -eq 2 ]
}

@test "does not flag non-shell contexts" {
	# The `,` line is the real bind's shape: #{qs:1} is an accepted shell-quoting
	# form, while the trailing %1 ARGUMENT token and the else branch's
	# `rename-window -- '%%'` are tmux-command positions, not shell strings, and
	# so is `new-session -s '%%'` on the next line.
	cat >"$BATS_TEST_TMPDIR/good.conf" <<'EOF'
bind Q if-shell -F '#{&&:#{@bridge_win},#{@bridge_pane}}' { run-shell "/bin/x #{q:@bridge_pane}" }
bind , if-shell -F '#{@gate}' { command-prompt -I'#{@window_bridge_name}' { run-shell "/bin/ctl rename #{q:@bridge_pane} #{qs:1}" %1 } } { command-prompt -I'#W' { rename-window -- '%%' } }
bind N command-prompt -p "New session name:" "new-session -s '%%'"
bind x confirm-before -p "kill #{@bridge_pane}? (y/n)" { run-shell "/bin/x #{q:@bridge_pane}" }
bind | if-shell -F '#{@gate}' { run-shell "/bin/x #{q:@bridge_pane}" } { split-window -h -c "#{pane_current_path}" }
set -g status-format[0] "#{session_name}"
bind U run-shell '/bin/x #{?client_name,--client #{q:client_name},}'
bind M run-shell -C "display-menu -T 'Menu' Foo f {new-window}"
EOF
	run check_conf_quoting "$BATS_TEST_TMPDIR/good.conf"
	[ "$status" -eq 0 ]
	[ -z "$output" ]
}

@test "requires bare qs: for wrap-required shell formats" {
	cat >"$BATS_TEST_TMPDIR/wrap-q.conf" <<'EOF'
run-shell "/bin/x #{q:session_name}"
run-shell "/bin/x #{q:hook_session_name}"
run-shell "/bin/x #{q:pane_current_command}"
run-shell "/bin/x #{q:pane_current_path}"
EOF
	run check_conf_quoting "$BATS_TEST_TMPDIR/wrap-q.conf"
	[ "$status" -eq 1 ]
	[ "$(echo "$output" | grep -c .)" -eq 4 ]

	cat >"$BATS_TEST_TMPDIR/wrap-qs.conf" <<'EOF'
run-shell "/bin/x #{qs:session_name}"
run-shell "/bin/x #{qs:hook_session_name}"
run-shell "/bin/x #{qs:pane_current_command}"
run-shell "/bin/x #{qs:pane_current_path}"
EOF
	run check_conf_quoting "$BATS_TEST_TMPDIR/wrap-qs.conf"
	[ "$status" -eq 0 ]
	[ -z "$output" ]
}

@test "flags qs: inside shell single and double quotes" {
	cat >"$BATS_TEST_TMPDIR/quoted-qs.conf" <<'EOF'
run-shell "/bin/x '#{qs:session_name}'"
run-shell '/bin/x "#{qs:pane_current_command}"'
EOF
	run check_conf_quoting "$BATS_TEST_TMPDIR/quoted-qs.conf"
	[ "$status" -eq 1 ]
	[[ $output == *'#{qs:session_name}'* ]]
	[[ $output == *'#{qs:pane_current_command}'* ]]
	[ "$(echo "$output" | grep -c .)" -eq 2 ]
}

@test "the emitted tmux.conf has zero shell-quoting violations" {
	# Hard failure, never skip: a skip reports `ok`, so a broken CONF binding
	# would retire this guard silently — the one outcome it exists to prevent.
	[ -n "${CONF:-}" ] || {
		echo "CONF is unset; the check derivation must bind it to tmuxConfig.tmuxConf" >&2
		false
	}
	[ -f "$CONF" ] || {
		echo "CONF does not exist: $CONF" >&2
		false
	}
	run check_conf_quoting "$CONF"
	if [ "$status" -ne 0 ]; then
		echo "$output" >&2
	fi
	[ "$status" -eq 0 ]
}

@test "flags a bare identity format inside a #(...) shell format" {
	cat >"$BATS_TEST_TMPDIR/hp-bad.conf" <<'EOF'
set -g status-format[0] "#(/bin/statusline --session '#{session_name}' --thm-bg '#{@thm_bg}')"
EOF
	run check_hashparen_identity "$BATS_TEST_TMPDIR/hp-bad.conf"
	[ "$status" -eq 1 ]
	# exactly one violation, and it names session_name. The presentation-only
	# @thm_bg is deliberately not flagged -- assert on the reported expansion
	# (field 2), not on the whole line: the excerpt quotes the body verbatim
	# and so mentions @thm_bg either way.
	[ "$(echo "$output" | grep -c .)" -eq 1 ]
	[ "$(echo "$output" | cut -d' ' -f2)" = '#{session_name}:' ]
}

@test "accepts a wrap-required identity format inside a #(...) shell format" {
	cat >"$BATS_TEST_TMPDIR/hp-good.conf" <<'EOF'
set -g status-format[0] "#(/bin/statusline --session #{qs:session_name} --thm-bg '#{@thm_bg}')"
EOF
	run check_hashparen_identity "$BATS_TEST_TMPDIR/hp-good.conf"
	[ "$status" -eq 0 ]
	[ -z "$output" ]
}

@test "flags q: for a wrap-required identity format inside #(...)" {
	cat >"$BATS_TEST_TMPDIR/hp-wrap-q.conf" <<'EOF'
set -g status-format[0] "#(/bin/statusline --session #{q:session_name})"
EOF
	run check_hashparen_identity "$BATS_TEST_TMPDIR/hp-wrap-q.conf"
	[ "$status" -eq 1 ]
	[ "$(echo "$output" | grep -c .)" -eq 1 ]
	[ "$(echo "$output" | cut -d' ' -f2)" = '#{q:session_name}:' ]
}

@test "flags q: for every wrap-required format inside #(...)" {
	cat >"$BATS_TEST_TMPDIR/hp-wrap-formats.conf" <<'EOF'
set -g status-format[0] "#(/bin/statusline --command #{q:pane_current_command} --path #{q:pane_current_path})"
EOF
	run check_hashparen_identity "$BATS_TEST_TMPDIR/hp-wrap-formats.conf"
	[ "$status" -eq 1 ]
	[[ $output == *'#{q:pane_current_command}'* ]]
	[[ $output == *'#{q:pane_current_path}'* ]]
	[ "$(echo "$output" | grep -c .)" -eq 2 ]
}

@test "flags qe:/qh:/qa: for a wrap-required identity format inside #(...)" {
	# wrap_only counterpart to the header's qe:/qh:/qa: rule (format_quote_style /
	# format_quote_shell -a don't quote for a shell) -- see scan_shell_string above.
	cat >"$BATS_TEST_TMPDIR/hp-wrap-style.conf" <<'EOF'
set -g status-format[0] "#(/bin/statusline --session #{qe:session_name})"
set -g status-format[1] "#(/bin/statusline --session #{qh:session_name})"
set -g status-format[2] "#(/bin/statusline --session #{qa:session_name})"
EOF
	run check_hashparen_identity "$BATS_TEST_TMPDIR/hp-wrap-style.conf"
	[ "$status" -eq 1 ]
	[[ $output == *'#{qe:session_name}'* ]]
	[[ $output == *'#{qh:session_name}'* ]]
	[[ $output == *'#{qa:session_name}'* ]]
	[ "$(echo "$output" | grep -c .)" -eq 3 ]
}

@test "flags bare pane formats inside #(...)" {
	cat >"$BATS_TEST_TMPDIR/hp-bare-pane.conf" <<'EOF'
set -g status-format[0] "#(/bin/statusline --command #{pane_current_command} --path #{pane_current_path})"
EOF
	run check_hashparen_identity "$BATS_TEST_TMPDIR/hp-bare-pane.conf"
	[ "$status" -eq 1 ]
	[[ $output == *'#{pane_current_command}'* ]]
	[[ $output == *'#{pane_current_path}'* ]]
	[ "$(echo "$output" | grep -c .)" -eq 2 ]
}

@test "flags qs: inside single and double quotes in #(...)" {
	cat >"$BATS_TEST_TMPDIR/hp-quoted-qs.conf" <<'EOF'
set -g status-format[0] "#(/bin/statusline --session '#{qs:session_name}')"
set -g status-format[1] '#(/bin/statusline --command "#{qs:pane_current_command}")'
EOF
	run check_hashparen_identity "$BATS_TEST_TMPDIR/hp-quoted-qs.conf"
	[ "$status" -eq 1 ]
	[[ $output == *'#{qs:session_name}'* ]]
	[[ $output == *'#{qs:pane_current_command}'* ]]
	[ "$(echo "$output" | grep -c .)" -eq 2 ]
}

@test "the emitted tmux.conf never puts an identity format bare in #(...)" {
	[ -n "${CONF:-}" ] || {
		echo "CONF is unset; the check derivation must bind it to tmuxConfig.tmuxConf" >&2
		false
	}
	run check_hashparen_identity "$CONF"
	if [ "$status" -ne 0 ]; then
		echo "$output" >&2
	fi
	[ "$status" -eq 0 ]
}

# The three ways this scanner was found to report a clean zero over conf text
# that plainly contains a bare format reaching a shell. Each is a false
# NEGATIVE, so each fixture must come back with a violation, not just a diff.

@test "a literal ) earlier in a #(...) body does not hide a bare format after it" {
	cat >"$BATS_TEST_TMPDIR/paren.conf" <<'EOF'
set -g status-format[0] "#(/bin/statusline --style 'rgba(0,0,0)' --session #{session_name})"
EOF
	run check_hashparen_identity "$BATS_TEST_TMPDIR/paren.conf"
	[ "$status" -eq 1 ]
	[[ $output == *'#{session_name}'* ]]
}

@test "an even trailing backslash run is not a continuation" {
	# tmux continues only on an ODD count; treating this as one would glue the
	# next statement on as an inert token and never scan its command word.
	printf 'run-shell "safe" \\\\\nrun-shell "bad #{session_name}"\n' \
		>"$BATS_TEST_TMPDIR/parity.conf"
	run check_conf_quoting "$BATS_TEST_TMPDIR/parity.conf"
	[ "$status" -eq 1 ]
	[[ $output == *'#{session_name}'* ]]
}

@test "a statement left pending by a continuation at EOF is still scanned" {
	printf 'run-shell "bad #{session_name}" \\\n' >"$BATS_TEST_TMPDIR/eof.conf"
	run check_conf_quoting "$BATS_TEST_TMPDIR/eof.conf"
	[ "$status" -eq 1 ]
	[[ $output == *'#{session_name}'* ]]
}

@test "a real (odd) continuation still joins into one statement" {
	printf 'run-shell "bad #{session_name}" \\\n   --extra\n' \
		>"$BATS_TEST_TMPDIR/join.conf"
	run check_conf_quoting "$BATS_TEST_TMPDIR/join.conf"
	[ "$status" -eq 1 ]
	[[ $output == *'#{session_name}'* ]]
}
