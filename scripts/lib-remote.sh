#!/usr/bin/env bash
# Pure helpers for og-remote-auth and og-remote-theme.
# Hot-path helpers set REPLY; no side effects on source.

# remote_auth_identity <ssh -G output>: set REPLY to the public half of the
# first identityfile whose .pub actually exists, absolute. A host matching
# only `Host *` resolves to five-plus built-in defaults with id_rsa first, and
# id_rsa.pub usually doesn't exist — offering that path would silently never
# match `-f` downstream, so this walks the list instead of trusting the first
# entry. Empty REPLY when none of them exist — the caller must then skip the
# ssh-copy-id offer rather than guess, since this machine carries more than
# one key and a work key must not land on a personal host.
remote_auth_identity() {
	local key value candidate
	REPLY=""
	while read -r key value; do
		[[ $key == identityfile ]] || continue
		candidate="${value/#\~/$HOME}.pub"
		if [[ -f $candidate ]]; then
			REPLY="$candidate"
			return 0
		fi
	done <<<"$1"
	return 0
}
