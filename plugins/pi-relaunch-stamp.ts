// pi session relaunch glue: on session start and every turn, hand this pane's
// @remux_relaunch stamping to the pi-relaunch-stamp binary (on PATH via the
// tmux-og home-manager module) with the current session file and pi's real
// argv. The bash side owns the transform + guards + quoting, so there is
// exactly one copy of that logic and it is bats-testable without pi.
//
// Events:
//   - session_start fires for a new session (file already created) and for a
//     resumed one (reason "resume") — re-stamping after a restore keeps the
//     stamp fresh across repeated restore cycles as long as pi runs at least
//     one turn per cycle (the cursor caveat; pi, unlike cursor, re-fires
//     session_start on every resume).
//   - turn_end fires per turn (one LLM response + tool calls), so a session
//     that survives multiple saves/restores re-stamps the file it is on.
//
// process.argv inside pi is ["bun", "/$bunfs/root/pi", <all args...>] on
// 0.85.1 — the slice(2) is everything the invoking shell/launcher actually
// passed to the real pi binary, which on a machine with the nix-config pi
// wrapper (home/ai/pi/default.nix) includes its injected -e/--skill/
// --prompt-template flags ahead of the caller's own args. Those are
// /nix/store paths: replaying them verbatim would run the wrapper's
// injections a second time on restore (double hook-bridge load) AND persist
// a store path that goes stale once garbage-collected. The bash side reads
// PI_USER_ARGC — exported by that wrapper as $# right before its `exec`,
// i.e. the count of trailing args that are the caller's own — off the
// environment (inherited by this execFile call from the wrapper through pi)
// and keeps only that trailing slice, so the replay carries the caller's own
// flags minus the positional launch prompt and any session-selection flags,
// then appends --session <file> — restore resumes the exact session through
// the CURRENT wrapper, which re-injects its own current store paths, with
// the same model and thinking level. Without PI_USER_ARGC (no wrapper, or an
// older one predating it) the bash side falls back to replaying everything.
//
// A launch carrying --no-session (ephemeral) yields getSessionFile() ===
// undefined, and --no-extensions disables auto-discovery of this file in the
// first place — both degrade to a bare-shell restore, the documented
// failure mode.
import type {ExtensionAPI} from "@earendil-works/pi-coding-agent";
import {execFile} from "node:child_process";

const STAMP = "pi-relaunch-stamp";

type StampCtx = {sessionManager: {getSessionFile(): string | null | undefined}};

export default function (pi: ExtensionAPI) {
	const stamp = (ctx: StampCtx): void => {
		const file = ctx.sessionManager.getSessionFile();
		if (!file) return; // ephemeral (--no-session) or pre-session bootstrap
		// Best effort: the pane can vanish between event and spawn, and tmux
		// may be absent — the bash side exits quietly on both.
		execFile(STAMP, [file, ...process.argv.slice(2)], () => {});
	};

	pi.on("session_start", async (_event, ctx) => stamp(ctx));
	pi.on("turn_end", async (_event, ctx) => stamp(ctx));
}