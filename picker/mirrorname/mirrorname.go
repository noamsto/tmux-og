// Package mirrorname builds the local tmux session name for a remote-bridge
// mirror. tmux's stock session menu parses session names as tmux commands,
// so a remote-controlled name must be sanitized before it reaches a local
// session name (#783).
package mirrorname

// Part maps every byte outside [A-Za-z0-9_-] to '_'. It operates byte-wise,
// not rune-wise, and must stay byte-identical to mirror_name_part in
// scripts/lib-remote.sh: the shared testdata/vectors.tsv pins both against
// the same expected output.
func Part(s string) string {
	b := []byte(s)
	for i, c := range b {
		switch {
		case c >= 'A' && c <= 'Z':
		case c >= 'a' && c <= 'z':
		case c >= '0' && c <= '9':
		case c == '_' || c == '-':
		default:
			b[i] = '_'
		}
	}
	return string(b)
}

// Local builds the local mirror session name for a remote host+session pair.
func Local(host, sess string) string {
	return Part(host) + "-" + Part(sess)
}
