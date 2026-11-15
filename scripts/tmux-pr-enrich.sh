#!/usr/bin/env bash
# Shim for the PR enrichment poller, which lives in Go (picker/prenrich, binary
# tmux-pr-enrich-go). It bakes the build-time config into flags and keeps the
# `tmux-pr-enrich` name callers invoke.
# Modes:
#   --tick | --tick-run | --tick-run-pending
#   --target T --branch B [--dir D] [--force]
#   --target T --url U
#   --mock-* ...
# A --notify value still starting with '@' means notifications are off;
# OG_NOTIFY_BIN overrides it (tests).
exec @pr_enrich_bin@ \
	--refresh-seconds @pr_refresh_seconds@ \
	--check-refresh-seconds @pr_check_refresh_seconds@ \
	--reflow @reflow@ \
	--notify "${OG_NOTIFY_BIN:-@notify@}" \
	--lib-log @lib_log@ \
	--icon-pending @enrich_icon_pending@ \
	--icon-success @enrich_icon_success@ \
	--icon-failure @enrich_icon_failure@ \
	--icon-merged @enrich_icon_merged@ \
	--icon-closed @enrich_icon_closed@ \
	--icon-conflict @enrich_icon_conflict@ \
	--icon-draft @enrich_icon_draft@ \
	"$@"
