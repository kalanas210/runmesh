package runmesh

import (
	"crypto/rand"
	"encoding/base32"
	"strings"
	"time"
)

// idEnc is Crockford's base32 alphabet, lowercased. Unlike base32.StdEncoding
// it is order-preserving, which is what makes an encoded id sort like the
// bytes it encodes.
var idEnc = base32.NewEncoding("0123456789abcdefghjkmnpqrstvwxyz").
	WithPadding(base32.NoPadding)

// NewID returns a prefixed, k-sortable 128-bit identifier: six bytes of
// big-endian millisecond timestamp followed by ten crypto/rand bytes, e.g.
// "job_0d7k2mq4x9...".
//
// Sortability earns its keep twice. GET /jobs?cursor= becomes keyset
// pagination on the id alone, with no tie-breaker column; and the Week-2
// PostgreSQL primary-key b-tree appends rather than fragmenting the way a
// random UUIDv4 does.
//
// now is a parameter rather than a time.Now() call so this package stays pure
// and the clock purity test needs no exception for it. Fourteen lines of
// crypto/rand is also why go.mod has no require block at all.
func NewID(prefix string, now time.Time) string {
	var b [16]byte
	ms := uint64(now.UnixMilli())
	b[0] = byte(ms >> 40)
	b[1] = byte(ms >> 32)
	b[2] = byte(ms >> 24)
	b[3] = byte(ms >> 16)
	b[4] = byte(ms >> 8)
	b[5] = byte(ms)
	if _, err := rand.Read(b[6:]); err != nil {
		// crypto/rand.Read is documented never to fail on any platform Go
		// supports. If it does, every identifier and every idempotency key in
		// the process is unsafe, so refusing to continue is the only correct
		// response.
		panic("runmesh: crypto/rand unavailable: " + err.Error())
	}
	return prefix + strings.ToLower(idEnc.EncodeToString(b[:]))
}
