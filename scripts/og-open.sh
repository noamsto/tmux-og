#!/usr/bin/env bash
# $BROWSER: opens a URL on the controlling host when this pane's session is
# bridged to a mirror whose attached control client is a live og_open
# subscriber (registered in @og_open_clients, or the legacy @og_open_client,
# by the bridge daemon), otherwise on this host via the platform opener. A
# record is tagged with the size owner @og_bridge_owner's client when that
# client is a registered attached control client, so only that controller
# opens it. Only the forwarded path validates: whitespace separates the
# forwarded log's records, a trailing `;` is tmux's argv command separator,
# and only http(s) may cross the bridge. Off a bridge any argument passes
# through untouched. See docs/agents/bridge-shipped-state.md.
set -uo pipefail

case $# in
1) ;;
*)
	echo "usage: og-open <url>" >&2
	exit 2
	;;
esac
url=$1

bridged=0
loglen=0
owner=
if [ -n "${TMUX:-}" ] && [ -n "${TMUX_PANE:-}" ]; then
	clients=$(tmux list-clients -t "$TMUX_PANE" -F \
		'#{?#{&&:#{client_control_mode},#{||:#{m:* #{client_name} *, #{@og_open_clients} },#{==:#{client_name},#{@og_open_client}}}},1,0}|#{?#{&&:#{client_control_mode},#{&&:#{m:* #{client_name} *, #{@og_open_clients} },#{m:*|#{client_name},#{@og_bridge_owner}}}},#{client_name},}|#{n:@og_open_url}' \
		2>/dev/null)
	while IFS='|' read -r ok target n; do
		if [ "$ok" = 1 ]; then
			bridged=1
			loglen=$n
		fi
		if [ -n "$target" ]; then
			owner=$target
		fi
	done <<EOF
$clients
EOF
fi

if [ "$bridged" = 1 ]; then
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

	record=" $(date +%s)-$$${owner:+@$owner}|$url"
	if [ "$loglen" -gt 4096 ]; then
		exec tmux set-option -t "$TMUX_PANE" @og_open_url "$record"
	fi
	exec tmux set-option -a -t "$TMUX_PANE" @og_open_url "$record"
fi

unset BROWSER
if [ "$(uname -s)" = Darwin ]; then
	exec open "$url"
fi
exec xdg-open "$url"
