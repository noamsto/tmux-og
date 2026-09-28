#!/usr/bin/env bash
# $BROWSER: opens a URL on the controlling host when this pane's session is
# bridged to a mirror whose control client is a live og_open subscriber
# (registered in @og_open_client by the bridge daemon), otherwise on this
# host via the platform opener. Body is POSIX sh so it lints unchanged under
# the bash header every scripts/*.sh ships with. See
# docs/superpowers/specs/2026-09-28-og-open-controller-url-design.md §1.
set -uo pipefail

case $# in
1) ;;
*)
	echo "usage: og-open <url>" >&2
	exit 2
	;;
esac
url=$1

case $url in
http://?* | https://?*) ;;
*)
	echo "og-open: not an http(s) URL: $url" >&2
	exit 2
	;;
esac

case $url in
*[[:space:][:cntrl:]]* | *\;)
	echo "og-open: URL has a disallowed character: $url" >&2
	exit 2
	;;
esac

len=$(printf %s "$url" | wc -c)
if [ "$len" -gt 4096 ]; then
	echo "og-open: URL exceeds 4096 bytes" >&2
	exit 2
fi

if [ -n "${TMUX:-}" ] && [ -n "${TMUX_PANE:-}" ]; then
	clients=$(tmux list-clients -t "$TMUX_PANE" -F \
		'#{?#{&&:#{client_control_mode},#{==:#{client_name},#{@og_open_client}}},1,0}|#{n:@og_open_url}' \
		2>/dev/null)
	bridged=0
	loglen=0
	while IFS='|' read -r ok n; do
		if [ "$ok" = 1 ]; then
			bridged=1
			loglen=$n
		fi
	done <<EOF
$clients
EOF

	if [ "$bridged" = 1 ]; then
		record=" $(date +%s)-$$|$url"
		if [ "$loglen" -gt 4096 ]; then
			exec tmux set-option -t "$TMUX_PANE" @og_open_url "$record"
		fi
		exec tmux set-option -a -t "$TMUX_PANE" @og_open_url "$record"
	fi
fi

unset BROWSER
if [ "$(uname -s)" = Darwin ]; then
	exec open "$url"
fi
exec xdg-open "$url"
