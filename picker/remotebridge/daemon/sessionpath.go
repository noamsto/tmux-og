package daemon

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/noamsto/tmux-og/picker/remotebridge/controlmode"
)

// sessionPathRe excludes '|' where dirRe excludes whitespace: the picker reads
// the stamp back inside a '|'-delimited list-panes row, where a pipe shifts every
// later field, while a space in a session path is both legal and harmless there.
var sessionPathRe = regexp.MustCompile(`^/[^|]*$`)

// readSessionPath fetches the remote session's own #{session_path}; the local
// mirror's is og-remote-open's cwd, since the remote directory need not exist
// here to pass as -c. An unusable reply is "".
func readSessionPath(rt roundTrip, sess string) string {
	l, ok := one(rt, fmt.Sprintf("display-message -p -t %s -F %s", tmuxQuote(sess), tmuxQuote(ctlSafe("#{session_path}"))))
	if !ok || l.Kind == controlmode.Error {
		return ""
	}
	p := strings.TrimRight(string(l.Data), "\r\n")
	return matching(cleanLabelValueExact(p, dirMaxRunes), sessionPathRe)
}
