package main

import "runtime"

// CheckLocal probes localChecklist plus the clipboard and URL-opener special
// cases through lookup, which the production caller wires to exec.LookPath —
// injected so tests never touch the real filesystem/PATH.
func CheckLocal(lookup func(string) (string, error)) []Result {
	results := make([]Result, 0, len(localChecklist)+2)
	for _, item := range localChecklist {
		results = append(results, checkLookup(lookup, item.Name, item.Feature))
	}
	results = append(results, checkClipboard(lookup))
	results = append(results, checkOpener(lookup))
	return results
}

func checkLookup(lookup func(string) (string, error), name, feature string) Result {
	if _, err := lookup(name); err != nil {
		return Result{Name: name, Feature: feature, Detail: "not found"}
	}
	return Result{Name: name, Feature: feature, OK: true}
}

// checkClipboard is an either/or: xclip, wl-paste or (macOS) osascript satisfies image paste.
// Absence isn't a hard failure the way other local checks are — the paste
// path degrades to forwarding the raw byte rather than breaking outright.
func checkClipboard(lookup func(string) (string, error)) Result {
	const name = "xclip/wl-paste/osascript"
	const feature = "image paste (ctrl+v)"
	_, xclipErr := lookup("xclip")
	_, wlPasteErr := lookup("wl-paste")
	_, osascriptErr := lookup("osascript")
	if xclipErr == nil || wlPasteErr == nil || osascriptErr == nil {
		return Result{Name: name, Feature: feature, OK: true}
	}
	return Result{Name: name, Feature: feature, Detail: "paste forwards the raw byte instead (documented degrade)"}
}

func openerName() string {
	if runtime.GOOS == "darwin" {
		return "open"
	}
	return "xdg-open"
}

func checkOpener(lookup func(string) (string, error)) Result {
	return checkLookup(lookup, openerName(), "URL opening (prefix+i o/p)")
}
