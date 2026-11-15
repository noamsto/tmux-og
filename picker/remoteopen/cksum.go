package remoteopen

import (
	"fmt"
	"strings"
)

// cksum is POSIX cksum (CRC-32/CKSUM). The socket key must match what the
// bash launcher computed with coreutils `cksum`, so the byte-length suffix
// and final complement are part of the contract, not a detail to simplify.
func cksum(data []byte) uint32 {
	var crc uint32
	for _, b := range data {
		crc = cksumUpdate(crc, b)
	}
	for n := uint64(len(data)); n > 0; n >>= 8 {
		crc = cksumUpdate(crc, byte(n))
	}
	return ^crc
}

func cksumUpdate(crc uint32, b byte) uint32 {
	crc ^= uint32(b) << 24
	for range 8 {
		if crc&0x80000000 != 0 {
			crc = crc<<1 ^ 0x04C11DB7
		} else {
			crc <<= 1
		}
	}
	return crc
}

// intoBaseDir mirrors the tmux socket directory fallback order so the
// --into socket lands beside the server's own socket when one is configured.
func intoBaseDir(getenv func(string) string) string {
	for _, k := range []string{"TMUX_TMPDIR", "XDG_RUNTIME_DIR"} {
		if v := getenv(k); v != "" {
			return v
		}
	}
	return "/tmp"
}

// intoSockPath keys the socket on the NUL-joined identity fields; the
// selector is appended only when set, so a bare --into keeps its key.
func intoSockPath(base, host, sess, serverPid, intoID, selector string) string {
	key := strings.Join([]string{host, sess, serverPid, intoID}, "\x00")
	if selector != "" {
		key += "\x00" + selector
	}
	return fmt.Sprintf("%s/og-into-%d.sock", base, cksum([]byte(key)))
}
