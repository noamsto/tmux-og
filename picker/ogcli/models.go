package ogcli

// models is the registry of Go-backed verbs, keyed by the table's "model"
// field. Each entry is the flag set kong parses for validation, --help and
// completion; the target is still exec'd with the original arguments.
var models = map[string]modelSpec{
	"remote-open": {new: func() any { return &remoteOpen{} }},
}

type modelSpec struct {
	// new returns a pointer to a fresh kong-tagged struct.
	new func() any
}

// remoteOpen mirrors the argv og-remote-open accepts; remoteopen.ParseArgs
// stays the authority on what combinations are valid.
type remoteOpen struct {
	Host   string `arg:"" help:"Remote host (an ssh alias)."`
	Sess   string `arg:"" optional:"" help:"Remote tmux session name."`
	Win    string `arg:"" optional:"" help:"Window: a numeric index or an @id."`
	Into   string `help:"Place the remote window into this local session"`
	Window string `help:"With --into: place this remote window (@<id> or @<option>=<value>)"`
}
