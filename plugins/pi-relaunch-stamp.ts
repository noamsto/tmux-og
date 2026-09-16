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
// 0.85.1 — the slice(2) is everything the invoking shell/launcher passed to
// the real pi binary, including any wrapper-injected flags ahead of the
// caller's own (e.g. this machine's nix-config pi wrapper injects
// -e/--skill/--prompt-template /nix/store paths). The bash side reads
// PI_USER_ARGC and CREW_WORKER_ID off the environment it inherits through
// this execFile call to strip such injected prefixes before replaying — see
// scripts/pi-relaunch-stamp.sh for the actual contract.
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