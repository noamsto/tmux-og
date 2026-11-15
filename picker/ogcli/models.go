package ogcli

// models is the registry of Go-backed verbs, keyed by the table's "model"
// field. Each entry gives completion the verb's flags and --help its text;
// kong never parses the verb's argv at exec time: the target gets it raw and
// stays its own sole validator.
var models = map[string]modelSpec{
	"remote-open": {new: func() any { return &remoteOpen{} }, help: remoteOpenHelp},
}

type modelSpec struct {
	// new returns a pointer to a fresh kong-tagged struct.
	new func() any
	// help is printed for `og <verb> --help` in place of kong's generated
	// usage, which cannot express a positional grammar.
	help string
}

// remoteOpenHelp states remoteopen.ParseArgs's grammar, not kong's: the flags
// are positional tokens, not flags that may sit anywhere.
const remoteOpenHelp = `Usage:
  og remote open <host> [<sess> [<win>]]
  og remote open <host> <sess> --into <local-session> [--window <selector>]

Arguments are positional: --into is the third argument and takes the next one;
--window <selector> may follow as the fifth and sixth. Neither accepts the
--flag=value form, and neither may come first.

Arguments:
  <host>                   Remote host (an ssh alias).
  <sess>                   Remote tmux session name.
  <win>                    Window: a numeric index or an @id.
  --into <local-session>   Place the remote window into this local session.
  --window <selector>      With --into: place this remote window (@<id> or
                           @<option>=<value>).

Environment:
  OG_REMOTE_NEW_DIR, OG_REMOTE_RESTORE   Not with --into.
  OG_REMOTE_TMPDIR                       Remote tmpdir.
`

// remoteOpen declares the tokens og-remote-open accepts, for completion;
// remoteopen.ParseArgs is the authority on what combinations are valid.
type remoteOpen struct {
	Host   string `arg:"" help:"Remote host (an ssh alias)."`
	Sess   string `arg:"" optional:"" help:"Remote tmux session name."`
	Win    string `arg:"" optional:"" help:"Window: a numeric index or an @id."`
	Into   string `help:"Place the remote window into this local session"`
	Window string `help:"With --into: place this remote window (@<id> or @<option>=<value>)"`
}
