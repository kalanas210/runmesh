// Package redis is a hand-written RESP2 client: enough of the Redis
// Serialization Protocol to send a command and decode its reply, and nothing
// else. It exists for the same reason internal/metrics hand-writes the
// Prometheus exposition format instead of taking client_golang, and the case
// is even stronger here — RESP is a five-line grammar, not a page, and
// ADR 0015 works the two-part dependency test from ADR 0007 out in full.
//
// This file is the wire format ONLY: encoding a command as a RESP array of
// bulk strings, and decoding a reply as one of RESP's five reply types. It has
// no notion of a connection, a pool or Redis's command vocabulary — those live
// in client.go — because a protocol that does not know how it is transported
// is a protocol a test can drive over a bytes.Buffer instead of a socket.
package redis

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strconv"
)

// ServerError is a Redis "-ERR ..." (or "-WRONGTYPE ...", "-NOSCRIPT ...")
// reply, wrapped in a named type so a caller can errors.As it for the exact
// server text instead of string-matching Value.Str or a formatted error.
type ServerError string

func (e ServerError) Error() string { return string(e) }

func serverError(msg string) error { return ServerError(msg) }

// Kind is which of RESP2's five reply types a Value holds. The five map
// directly onto the five leading bytes the protocol defines; there is no
// sixth kind to add without a protocol version this client does not speak.
type Kind byte

const (
	KindStatus Kind = '+' // a short, human-readable "it worked" - OK, PONG
	KindError  Kind = '-' // the server refused the command
	KindInt    Kind = ':' // a signed 64-bit integer, exact, never a float
	KindBulk   Kind = '$' // a byte string of known length, or null
	KindArray  Kind = '*' // zero or more further Values, or null
)

// Value is one fully-decoded RESP reply. Exactly one of the fields below is
// meaningful for a given Kind, chosen the way a sum type would be if Go had
// one: Str for KindStatus, Err for KindError, Int for KindInt, Bulk for
// KindBulk, Array for KindArray.
//
// Bulk and Array both distinguish nil from empty, because RESP does: `$-1`
// (no such key) and `$0` (a key holding an empty string) are different
// replies to GET, and collapsing them the way a plain []byte comparison would
// is exactly the kind of bug a hand-rolled client earns by not being careful
// here.
type Value struct {
	Kind  Kind
	Str   string
	Err   error
	Int   int64
	Bulk  []byte  // nil for a null bulk string ($-1); len==0, non-nil for ""
	Array []Value // nil for a null array (*-1); len==0, non-nil for an empty one
}

// IsNil reports whether this Value is RESP's null - $-1 or *-1 - which are the
// same absence at two different types and both must be checked before a
// caller reads Bulk or Array.
func (v Value) IsNil() bool {
	return (v.Kind == KindBulk && v.Bulk == nil) || (v.Kind == KindArray && v.Array == nil)
}

// errShortLine is returned by readLine when the server closes the connection
// or sends a line with no terminator; it is never returned to a caller of Do,
// which wraps it with which command was in flight.
var errShortLine = errors.New("redis: connection closed mid-reply")

// encodeCommand renders args as RESP's request format: an array of bulk
// strings. This is the ONLY request shape RESP defines past its earliest
// inline-command version, which this client does not speak - every command
// this package issues, PING included, goes out this way.
func encodeCommand(args ...string) []byte {
	// Sized exactly, not guessed: the header line, each argument's own header
	// line, the argument bytes, and the trailing \r\n on every line.
	n := 1 + len(itoa(len(args))) + 2
	for _, a := range args {
		n += 1 + len(itoa(len(a))) + 2 + len(a) + 2
	}
	buf := make([]byte, 0, n)
	buf = append(buf, '*')
	buf = append(buf, itoa(len(args))...)
	buf = append(buf, '\r', '\n')
	for _, a := range args {
		buf = append(buf, '$')
		buf = append(buf, itoa(len(a))...)
		buf = append(buf, '\r', '\n')
		buf = append(buf, a...)
		buf = append(buf, '\r', '\n')
	}
	return buf
}

func itoa(n int) []byte { return strconv.AppendInt(nil, int64(n), 10) }

// decodeValue reads exactly one RESP value from r, recursing once per level
// of array nesting - which for every command this client sends is at most
// one deep (EVAL's reply is a flat array of integers), so there is no
// practical depth limit to enforce.
func decodeValue(r *bufio.Reader) (Value, error) {
	line, err := readLine(r)
	if err != nil {
		return Value{}, err
	}
	if len(line) == 0 {
		return Value{}, fmt.Errorf("redis: empty reply line")
	}
	kind, rest := Kind(line[0]), line[1:]

	switch kind {
	case KindStatus:
		return Value{Kind: kind, Str: rest}, nil

	case KindError:
		return Value{Kind: kind, Str: rest, Err: serverError(rest)}, nil

	case KindInt:
		n, err := strconv.ParseInt(rest, 10, 64)
		if err != nil {
			return Value{}, fmt.Errorf("redis: %q is not an integer reply: %w", rest, err)
		}
		return Value{Kind: kind, Int: n}, nil

	case KindBulk:
		n, err := strconv.Atoi(rest)
		if err != nil {
			return Value{}, fmt.Errorf("redis: %q is not a bulk length: %w", rest, err)
		}
		if n < 0 {
			return Value{Kind: kind, Bulk: nil}, nil // $-1: the null bulk string
		}
		// make([]byte, n) is never nil, even at n==0 — unlike a nil slice
		// literal, which is the whole reason Bulk can tell "" (n==0) apart
		// from $-1 (the branch above, which returns nil explicitly) without
		// a separate boolean field.
		body := make([]byte, n)
		if _, err := readFull(r, body); err != nil {
			return Value{}, err
		}
		// The trailing \r\n after the payload is part of the frame, not part
		// of the value, and is discarded here rather than left for the next
		// read to trip over.
		if _, err := readLine(r); err != nil {
			return Value{}, err
		}
		return Value{Kind: kind, Bulk: body}, nil

	case KindArray:
		n, err := strconv.Atoi(rest)
		if err != nil {
			return Value{}, fmt.Errorf("redis: %q is not an array length: %w", rest, err)
		}
		if n < 0 {
			return Value{Kind: kind, Array: nil}, nil // *-1: the null array
		}
		// make([]Value, n) is never nil, even at n==0 — the same reason the
		// bulk-string case above needs no separate empty-vs-null handling.
		arr := make([]Value, n)
		for i := range arr {
			v, err := decodeValue(r)
			if err != nil {
				return Value{}, err
			}
			arr[i] = v
		}
		return Value{Kind: kind, Array: arr}, nil

	default:
		return Value{}, fmt.Errorf("redis: unknown reply type %q (line: %q)", string(kind), line)
	}
}

// readLine reads one CRLF-terminated line and returns it WITHOUT the
// terminator. RESP header lines never contain an embedded \n, so ReadString
// is exact here rather than an approximation.
func readLine(r *bufio.Reader) (string, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return "", errShortLine
	}
	// Every RESP line ends \r\n; a bare \n would mean the server does not
	// speak this protocol at all; anything shorter cannot be well-formed.
	if len(line) < 2 || line[len(line)-2] != '\r' {
		return "", fmt.Errorf("redis: reply line %q does not end \\r\\n", line)
	}
	return line[:len(line)-2], nil
}

// readFull reads exactly len(buf) bytes, translating an early io.EOF the way
// readLine does: a connection that closes mid-body is the same "gone" event
// whether it happens between lines or inside one.
func readFull(r *bufio.Reader, buf []byte) (int, error) {
	n, err := io.ReadFull(r, buf)
	if err != nil {
		return n, errShortLine
	}
	return n, nil
}
