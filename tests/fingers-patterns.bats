#!/usr/bin/env bats
# The tmux-og replacements for tmux-fingers' url and path built-ins
# (@fingers-pattern-10/11) must stop before trailing punctuation. The patterns
# are read out of the conf template, so the test pins what ships. Fingers
# compiles patterns with PCRE2; grep -P is the compatible engine here.
# Run from repo root: bats tests/fingers-patterns.bats

# The conf double-quotes the pattern, so tmux turns \\ into \ before fingers
# sees it; undo exactly that.
conf_pattern() {
	sed -n "s/^set -g @fingers-pattern-$1 \"\\(.*\\)\"\$/\\1/p" config/tmux.conf.tmpl | sed 's/\\\\/\\/g'
}

# fingers' own built-ins, verbatim, as the control the new patterns replace
OLD_URL='((https?://|git@|git://|ssh://|ftp://|file:///)[^\s()"'"'"']+)'
OLD_PATH='(([.\w\-~\$@]+)?(/[.\w\-@]+)+/?)'

setup() {
	URL="$(conf_pattern 10)"
	PATH_RE="$(conf_pattern 11)"
	EMAIL="$(conf_pattern 6)"
	[ -n "$URL" ] && [ -n "$PATH_RE" ]
}

# expect_match <pattern> <input> <expected>
expect_match() {
	run bash -c 'printf "%s\n" "$2" | grep -oP -- "$1"' _ "$1" "$2"
	[ "$status" -eq 0 ]
	[ "$output" = "$3" ]
}

@test "builtins are replaced, not duplicated" {
	run grep -E '^set -g @fingers-enabled-builtin-patterns "ip,' config/tmux.conf.tmpl
	[ "$status" -eq 0 ]
	run grep -E '@fingers-enabled-builtin-patterns.*(url|path)' config/tmux.conf.tmpl
	[ "$status" -ne 0 ]
}

@test "url: stops before each trailing punctuation character" {
	local c
	for c in . ',' ';' : '!' '?' ')' ']' '>' '`' "'" '"' '}'; do
		expect_match "$URL" "see https://x.io/a${c}" "https://x.io/a"
	done
}

@test "url: trailing punctuation run is dropped" {
	expect_match "$URL" "go to https://x.io/a.)," "https://x.io/a"
}

@test "url: inside parentheses and backticks" {
	expect_match "$URL" "(https://x.io/a)" "https://x.io/a"
	expect_match "$URL" "see (https://x.io/a)." "https://x.io/a"
	local bt
	bt="$(printf '\140')"
	expect_match "$URL" "${bt}https://x.io/a${bt}" "https://x.io/a"
}

@test "url: query string and fragment survive" {
	expect_match "$URL" "https://x.io/a?q=1&b=2#frag." "https://x.io/a?q=1&b=2#frag"
}

@test "url: balanced parentheses stay inside" {
	expect_match "$URL" "https://en.wikipedia.org/wiki/Foo_(bar)." "https://en.wikipedia.org/wiki/Foo_(bar)"
	expect_match "$URL" "(see https://en.wikipedia.org/wiki/Foo_(bar))" "https://en.wikipedia.org/wiki/Foo_(bar)"
}

@test "url: file:/// and other schemes" {
	expect_match "$URL" "file:///tmp/a/b.txt." "file:///tmp/a/b.txt"
	expect_match "$URL" "ssh://host/repo.git," "ssh://host/repo.git"
}

@test "path: stops before a trailing dot" {
	expect_match "$PATH_RE" "edit foo/bar.go." "foo/bar.go"
	expect_match "$PATH_RE" "see /a/b." "/a/b"
}

@test "path: keeps dots in the middle and a trailing slash" {
	expect_match "$PATH_RE" "in a/b.tar.gz," "a/b.tar.gz"
	expect_match "$PATH_RE" "cd ../x/y/" "../x/y/"
}

@test "path: line and column suffix is not part of the match" {
	expect_match "$PATH_RE" "./a/b.go:12:3" "./a/b.go"
}

@test "path: home and variable prefixes" {
	# shellcheck disable=SC2088 # a literal ~ is the input
	expect_match "$PATH_RE" "~/x/y" "~/x/y"
	local d
	d="$(printf '\044')"
	expect_match "$PATH_RE" "${d}HOME/x/y." "${d}HOME/x/y"
}

@test "joined alternation: a url is not re-matched as a path" {
	expect_match "($EMAIL|$URL|$PATH_RE)" "see https://x.io/a/b.go." "https://x.io/a/b.go"
}

@test "old url built-in keeps trailing punctuation (control)" {
	local c
	for c in . ',' ';' : '!' '?' ']' '>' '`'; do
		run bash -c 'printf "%s\n" "$2" | grep -oP -- "$1"' _ "$OLD_URL" "see https://x.io/a${c}"
		[ "$output" = "https://x.io/a${c}" ]
	done
}

@test "old path built-in keeps a trailing dot (control)" {
	run bash -c 'printf "%s\n" "$2" | grep -oP -- "$1"' _ "$OLD_PATH" "edit foo/bar.go."
	[ "$output" = "foo/bar.go." ]
}
