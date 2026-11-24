#!/usr/bin/env bash
# The subcommands live in picker/project (built as og-project-bin). This shim
# exists so mkProjectScript can substitute store paths (the project_bin,
# projects_config and remote_open placeholders), which a Go binary can't carry.
# It must not share the binary's name: wrapperBinDir holds both. With the
# placeholders unsubstituted, the binary comes from OG_PROJECT_BIN or PATH and
# the registry is whatever TMUX_OG_PROJECTS already names. exec keeps the pid so
# signals reach the binary.
bin="@project_bin@"
[[ $bin == @* ]] && bin="${OG_PROJECT_BIN:-$(command -v og-project-bin || true)}"
if [[ -z $bin ]]; then
	echo "og-project: og-project-bin not found (build picker/ or set OG_PROJECT_BIN)" >&2
	exit 1
fi
registry="@projects_config@"
[[ $registry == @* ]] || export TMUX_OG_PROJECTS="$registry"
remote_open="@remote_open@"
[[ $remote_open == @* ]] && remote_open=""
exec "$bin" --remote-open "$remote_open" -- "$@"
