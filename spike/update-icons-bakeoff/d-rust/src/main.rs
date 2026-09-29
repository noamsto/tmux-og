// Variant D: the 1 s window-naming path of scripts/tmux-update-icons.sh in
// Rust, with the same collapsed call pattern as variants B and C (one tmux
// read, one tmux write only when something changed, one guarded git per polled
// window). std only, no dependencies.
//
// Out of scope, as in every variant: the 5 s arming sweep, carousel/remux
// stamping, the cwd-move reconcile, the reflow kick, agent-state file parsing,
// and the branch-transition path. Each exits 3 rather than run a path the
// fixture does not exercise.
use std::collections::{HashMap, HashSet};
use std::fs;
use std::io::Write;
use std::os::unix::fs::MetadataExt;
use std::process::{exit, Command, Stdio};
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::Arc;
use std::time::Duration;

extern "C" {
    fn getuid() -> u32;
    fn kill(pid: i32, sig: i32) -> i32;
}

const ICONS_TSV: &str = include_str!("../../icons.tsv");

const PANE_FORMAT: &str = "P|#{pane_id}|#{session_id}|#{window_index}|#{pane_index}|#{pane_current_path}|#{pane_current_command}|#{@branch}|#{pane_floating_flag}|#{@worktree}|#{@window_cwd_seen}|#{?window_modal_pane,#{pane_last},#{pane_active}}|#{window_active}|#{s/[|]/ /:@window_ai_name}|#{s/[|]/ /:@remux_relaunch}|#{@window_icon_display}|#{@window_icon_padded}|#{@window_claude_ago}|#{automatic-rename}|#{@active_pane_icon}|#{@claude_session_fg}|#{@crew_name}|#{@crew_seen}|#{@bridge_win}|#{@bridge_proc}|#{@claude_img_src}|#{@window_has_agent}|#{@window_manual_name}|#{@window_naming_dirty}|#{@window_task}";

fn out_of_scope(what: &str) -> ! {
    eprintln!("update-icons bake-off: {what} is out of scope");
    exit(3);
}

struct Icons {
    max: usize,
    agents: HashSet<&'static str>,
    glyph: HashMap<&'static str, &'static str>,
}

fn load_icons() -> Icons {
    let mut ic = Icons {
        max: 0,
        agents: HashSet::new(),
        glyph: HashMap::new(),
    };
    for line in ICONS_TSV.split('\n') {
        let Some((key, val)) = line.split_once('\t') else {
            continue;
        };
        match key {
            "#max" => ic.max = val.parse().unwrap_or(0),
            "#agents" => ic.agents.extend(val.split_whitespace()),
            _ => {
                ic.glyph.insert(key, val);
            }
        }
    }
    ic
}

fn normalize_wrapped(cmd: &str) -> &str {
    if let Some(rest) = cmd.strip_prefix('.') {
        if let Some(name) = rest.strip_suffix("-wrapped") {
            return name;
        }
    }
    cmd
}

// Mirrors _icon_cell_width in scripts/lib-icons.sh (first character only).
fn cell_width(s: &str) -> usize {
    let cp = s.chars().next().map_or(0, |c| c as u32);
    match cp {
        0 => 2,
        0xE000..=0xF8FF | 0xF0000.. => 1,
        0x1F000.. => 2,
        0x231A..=0x231B
        | 0x23E9..=0x23EC
        | 0x23F0
        | 0x23F3
        | 0x25FD..=0x25FE
        | 0x2614..=0x2615
        | 0x2648..=0x2653
        | 0x267F
        | 0x2693
        | 0x26A1
        | 0x26AA..=0x26AB
        | 0x26BD..=0x26BE
        | 0x26C4..=0x26C5
        | 0x26CE
        | 0x26D4
        | 0x26EA
        | 0x26F2..=0x26F3
        | 0x26F5
        | 0x26FA
        | 0x26FD
        | 0x2705
        | 0x270A..=0x270B
        | 0x2728
        | 0x274C
        | 0x274E
        | 0x2753..=0x2755
        | 0x2757
        | 0x2795..=0x2797
        | 0x27B0
        | 0x27BF
        | 0x2B1B..=0x2B1C
        | 0x2B50
        | 0x2B55 => 2,
        _ => 1,
    }
}

// Mirrors build_proc_icons: icon string (trailing space per icon) and its width.
fn build_proc_icons(ic: &Icons, procs: &[String]) -> (String, usize) {
    let mut out = String::new();
    let (mut dw, mut count) = (0, 0);
    for p in procs {
        if count >= ic.max {
            break;
        }
        let icon = ic.glyph.get(normalize_wrapped(p)).copied().unwrap_or("");
        if icon.is_empty() {
            continue;
        }
        out.push_str(icon);
        out.push(' ');
        dw += cell_width(icon) + 1;
        count += 1;
    }
    (out, dw)
}

// `git branch --show-current`, killed after 2 s: the same fork count and guard
// as variants B and C, with no `timeout` exec.
fn current_branch(dir: &str) -> String {
    let Ok(child) = Command::new("git")
        .args(["-C", dir, "branch", "--show-current"])
        .stdout(Stdio::piped())
        .stderr(Stdio::null())
        .spawn()
    else {
        return String::new();
    };
    let done = Arc::new(AtomicBool::new(false));
    let watchdog_done = Arc::clone(&done);
    let pid = child.id() as i32;
    std::thread::spawn(move || {
        std::thread::sleep(Duration::from_secs(2));
        if !watchdog_done.load(Ordering::SeqCst) {
            // SAFETY: plain kill(2) on the pid of the child spawned above.
            unsafe { kill(pid, 9) };
        }
    });
    let out = child.wait_with_output();
    done.store(true, Ordering::SeqCst);
    match out {
        Ok(o) if o.status.success() => String::from_utf8_lossy(&o.stdout)
            .trim_end_matches('\n')
            .to_string(),
        _ => String::new(),
    }
}

#[derive(Default)]
struct Window {
    sess: String,
    idx: String,
    path: String,
    cwd: String,
    branch: String,
    task: String,
    ai_name: String,
    display: String,
    padded: String,
    ago: String,
    rename: String,
    crew: String,
    crew_seen: String,
    bridge: String,
    has_agent: String,
    manual: String,
    naming_dirty: String,
    procs: Vec<String>,
    poison: bool,
}

#[derive(Default)]
struct Session {
    active_icon: String,
    session_fg: String,
    active_win: String,
    active_proc: String,
}

fn owned(md: &fs::Metadata) -> bool {
    md.uid() == unsafe { getuid() }
}

// The steady state of claude_status_dir_trusted, the marker-gated
// claude_prune_stale_state and an empty claude_pane_ids.
fn check_state_dir(server_start: &str, server_pid: &str) {
    let dir = std::env::var("CLAUDE_STATUS_DIR")
        .unwrap_or_else(|_| format!("/tmp/claude-status-{}", unsafe { getuid() }));
    match fs::symlink_metadata(&dir) {
        Ok(md) if md.is_dir() && owned(&md) => {}
        _ => out_of_scope("untrusted state dir"),
    }
    match fs::symlink_metadata(format!("{dir}/.owner-only")) {
        Ok(md) if md.is_file() && owned(&md) && !md.file_type().is_symlink() => {}
        _ => out_of_scope("owner-only marker missing"),
    }
    match fs::read_to_string(format!("{dir}/.server_start.{server_pid}")) {
        Ok(gate) if gate.trim_end_matches('\n') == server_start => {}
        _ => out_of_scope("prune sweep"),
    }
    for sub in ["panes", "screen"] {
        if let Ok(mut entries) = fs::read_dir(format!("{dir}/{sub}")) {
            if entries.next().is_some() {
                out_of_scope("agent state files");
            }
        }
    }
}

fn main() {
    let args: Vec<String> = std::env::args().collect();
    if args.len() < 7 {
        eprintln!("usage: update-icons SESSION RESUME_CLAUDE START_TIME RESUME_CAROUSEL FLAVOR PID");
        exit(2);
    }
    let (invoke_name, server_start, server_pid) = (&args[1], &args[3], &args[6]);
    let ic = load_icons();
    check_state_dir(server_start, server_pid);

    let raw = match Command::new("tmux")
        .args([
            "list-sessions",
            "-F",
            "S|#{session_id}|#{session_name}",
            ";",
            "list-panes",
            "-a",
            "-f",
            "#{!:#{pane_modal_flag}}",
            "-F",
            PANE_FORMAT,
        ])
        .output()
    {
        Ok(o) if o.status.success() => String::from_utf8_lossy(&o.stdout).into_owned(),
        Ok(o) => {
            eprintln!("tmux read: {}", o.status);
            exit(1);
        }
        Err(e) => {
            eprintln!("tmux read: {e}");
            exit(1);
        }
    };

    let mut sess_id_of: HashMap<&str, &str> = HashMap::new();
    let mut sess: HashMap<&str, Session> = HashMap::new();
    let mut wins: HashMap<String, Window> = HashMap::new();
    let mut order: Vec<String> = Vec::new();
    for line in raw.split('\n') {
        if let Some(rest) = line.strip_prefix("S|") {
            if let Some((sid, sname)) = rest.split_once('|') {
                sess_id_of.insert(sname, sid);
            }
            continue;
        }
        let Some(rest) = line.strip_prefix("P|") else {
            continue;
        };
        let f: Vec<&str> = rest.splitn(29, '|').collect();
        if f.len() < 29 {
            continue;
        }
        let proc = if f[23].is_empty() { f[5] } else { f[23] };
        let wkey = format!("{}:{}", f[1], f[2]);
        let (pane_active, window_active) = (f[10], f[11]);
        let s = sess.entry(f[1]).or_default();
        s.active_icon = f[18].to_string();
        s.session_fg = f[19].to_string();
        if window_active == "1" {
            s.active_win = f[2].to_string();
        }
        if pane_active == "1" && window_active == "1" {
            s.active_proc = proc.to_string();
        }
        if !wins.contains_key(&wkey) {
            order.push(wkey.clone());
            wins.insert(
                wkey.clone(),
                Window {
                    sess: f[1].to_string(),
                    idx: f[2].to_string(),
                    path: f[4].to_string(),
                    branch: f[6].to_string(),
                    task: f[28].to_string(),
                    ai_name: f[12].to_string(),
                    display: f[14].to_string(),
                    padded: f[15].to_string(),
                    ago: f[16].to_string(),
                    rename: f[17].to_string(),
                    crew: f[20].to_string(),
                    crew_seen: f[21].to_string(),
                    bridge: f[22].to_string(),
                    has_agent: f[25].to_string(),
                    manual: f[26].to_string(),
                    naming_dirty: f[27].to_string(),
                    ..Default::default()
                },
            );
        }
        let w = wins.get_mut(&wkey).unwrap();
        if pane_active != "0" && pane_active != "1" {
            w.poison = true;
        }
        if w.cwd.is_empty() && f[7] != "1" {
            w.cwd = f[4].to_string();
        }
        if proc.is_empty() {
            continue;
        }
        if !w.procs.iter().any(|p| p == proc) {
            w.procs.push(proc.to_string());
        }
    }
    let invoke_sid = sess_id_of.get(invoke_name.as_str()).copied().unwrap_or("");

    let mut cmds = String::new();
    let mut set = |scope: &str, target: &str, name: &str, val: &str| {
        cmds.push_str(&format!("{scope} -t '{target}' {name} '{val}'\n"));
    };
    let target_dw = ic.max * 3 + 2;
    for key in &order {
        let w = &wins[key];
        let mut has_agent = "";
        if w.bridge != "1" && w.procs.iter().any(|p| ic.agents.contains(normalize_wrapped(p))) {
            has_agent = "1";
        }
        if !w.task.is_empty() {
            set("set -qw", key, "@window_task", "");
        }
        if !w.ai_name.is_empty() {
            set("set -qw", key, "@window_ai_name", "");
        }
        if w.crew != w.crew_seen {
            set("set -qw", key, "@crew_seen", &w.crew);
        }
        let clear_needed = has_agent.is_empty() && !w.naming_dirty.is_empty();
        if !w.poison && w.bridge != "1" && (has_agent != w.has_agent || clear_needed) {
            if has_agent.is_empty() {
                out_of_scope("agent-left naming clear");
            }
            set("set -qw", key, "@window_has_agent", "1");
        }

        let invoking = w.sess == invoke_sid
            && sess.get(invoke_sid).is_some_and(|s| s.active_win == w.idx);
        if invoking || w.branch.is_empty() {
            let path = if w.cwd.is_empty() { &w.path } else { &w.cwd };
            if current_branch(path) != w.branch {
                out_of_scope("branch transition");
            }
        }

        let (icon, dw) = build_proc_icons(&ic, &w.procs);
        let display = icon.strip_suffix(' ').unwrap_or(&icon);
        if !w.ago.is_empty() {
            set("set -qw", key, "@window_claude_ago", "");
        }
        if display != w.display {
            set("set -qw", key, "@window_icon_display", display);
        }
        if w.bridge == "1" {
            if w.rename == "1" {
                set("set -qw", key, "automatic-rename", "off");
            }
        } else if w.rename != "1" && w.manual != "1" {
            set("set -qw", key, "automatic-rename", "on");
        }
        let padded = format!("{icon}{}", " ".repeat(target_dw.saturating_sub(dw)));
        if padded != w.padded {
            set("set -qw", key, "@window_icon_padded", &padded);
        }
    }
    for (id, s) in &sess {
        let active_icon = if s.active_proc.is_empty() {
            ""
        } else {
            ic.glyph
                .get(normalize_wrapped(&s.active_proc))
                .copied()
                .unwrap_or("")
        };
        if active_icon != s.active_icon {
            set("set -q", id, "@active_pane_icon", active_icon);
        }
        if !s.session_fg.is_empty() {
            set("set -q", id, "@claude_session_fg", "");
        }
    }

    if cmds.is_empty() {
        return;
    }
    let mut child = Command::new("tmux")
        .args(["source", "-"])
        .stdin(Stdio::piped())
        .spawn()
        .unwrap_or_else(|e| {
            eprintln!("tmux write: {e}");
            exit(1);
        });
    child
        .stdin
        .take()
        .unwrap()
        .write_all(cmds.as_bytes())
        .unwrap_or_else(|e| {
            eprintln!("tmux write: {e}");
            exit(1);
        });
    if !child.wait().is_ok_and(|s| s.success()) {
        exit(1);
    }
}
