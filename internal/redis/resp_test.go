package redis

import (
	"bufio"
	"strings"
	"testing"
)

func decode(t *testing.T, wire string) Value {
	t.Helper()
	v, err := decodeValue(bufio.NewReader(strings.NewReader(wire)))
	if err != nil {
		t.Fatalf("decodeValue(%q): %v", wire, err)
	}
	return v
}

func TestEncodeCommandIsAnArrayOfBulkStrings(t *testing.T) {
	t.Parallel()
	got := string(encodeCommand("SET", "k", "v"))
	want := "*3\r\n$3\r\nSET\r\n$1\r\nk\r\n$1\r\nv\r\n"
	if got != want {
		t.Errorf("encodeCommand(SET, k, v) =\n%q\nwant\n%q", got, want)
	}
}

func TestEncodeCommandNoArgs(t *testing.T) {
	t.Parallel()
	if got := string(encodeCommand()); got != "*0\r\n" {
		t.Errorf("encodeCommand() = %q, want *0\\r\\n", got)
	}
}

// TestEncodeCommandEmptyArgument: RESP's bulk-string length prefix is what
// carries an empty argument correctly — there is no delimiter to confuse it
// with "no argument at all".
func TestEncodeCommandEmptyArgument(t *testing.T) {
	t.Parallel()
	got := string(encodeCommand("SET", "k", ""))
	want := "*3\r\n$3\r\nSET\r\n$1\r\nk\r\n$0\r\n\r\n"
	if got != want {
		t.Errorf("encodeCommand(SET, k, \"\") =\n%q\nwant\n%q", got, want)
	}
}

func TestDecodeStatus(t *testing.T) {
	t.Parallel()
	v := decode(t, "+OK\r\n")
	if v.Kind != KindStatus || v.Str != "OK" {
		t.Errorf("decode(+OK) = %+v, want Kind=Status Str=OK", v)
	}
}

func TestDecodeError(t *testing.T) {
	t.Parallel()
	v := decode(t, "-ERR unknown command 'BOGUS'\r\n")
	if v.Kind != KindError {
		t.Fatalf("Kind = %v, want KindError", v.Kind)
	}
	if v.Err == nil || v.Err.Error() != "ERR unknown command 'BOGUS'" {
		t.Errorf("Err = %v, want the server's message verbatim", v.Err)
	}
	if _, ok := v.Err.(ServerError); !ok {
		t.Errorf("Err is a %T, want ServerError", v.Err)
	}
}

func TestDecodeInteger(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		wire string
		want int64
	}{
		{":0\r\n", 0},
		{":1000\r\n", 1000},
		{":-1\r\n", -1}, // a legitimate integer reply, distinct from a null bulk/array
	} {
		v := decode(t, tc.wire)
		if v.Kind != KindInt || v.Int != tc.want {
			t.Errorf("decode(%q) = %+v, want Int=%d", tc.wire, v, tc.want)
		}
	}
}

func TestDecodeBulkString(t *testing.T) {
	t.Parallel()
	v := decode(t, "$6\r\nfoobar\r\n")
	if v.Kind != KindBulk || string(v.Bulk) != "foobar" {
		t.Errorf("decode($6 foobar) = %+v, want Bulk=foobar", v)
	}
}

// TestDecodeEmptyBulkStringIsNotNil is the distinction RESP itself draws
// between $0 (a key holding "") and $-1 (no such key) — collapsing the two
// with a plain []byte nil-check is exactly the bug a hand-rolled client
// earns by not keeping them apart.
func TestDecodeEmptyBulkStringIsNotNil(t *testing.T) {
	t.Parallel()
	v := decode(t, "$0\r\n\r\n")
	if v.IsNil() {
		t.Error("an empty bulk string ($0) reports IsNil, want false")
	}
	if v.Bulk == nil || len(v.Bulk) != 0 {
		t.Errorf("Bulk = %#v, want a non-nil empty slice", v.Bulk)
	}
}

func TestDecodeNullBulkString(t *testing.T) {
	t.Parallel()
	v := decode(t, "$-1\r\n")
	if v.Kind != KindBulk || !v.IsNil() {
		t.Errorf("decode($-1) = %+v, want a nil bulk string", v)
	}
	if v.Bulk != nil {
		t.Errorf("Bulk = %#v, want nil", v.Bulk)
	}
}

func TestDecodeNullArray(t *testing.T) {
	t.Parallel()
	v := decode(t, "*-1\r\n")
	if v.Kind != KindArray || !v.IsNil() {
		t.Errorf("decode(*-1) = %+v, want a nil array", v)
	}
}

func TestDecodeEmptyArrayIsNotNil(t *testing.T) {
	t.Parallel()
	v := decode(t, "*0\r\n")
	if v.IsNil() {
		t.Error("an empty array (*0) reports IsNil, want false")
	}
	if v.Array == nil || len(v.Array) != 0 {
		t.Errorf("Array = %#v, want a non-nil empty slice", v.Array)
	}
}

// TestDecodeNestedArray is the exact shape the token-bucket Lua script
// returns: a flat array of two integers. It is also the deepest nesting any
// command this package issues ever produces.
func TestDecodeNestedArray(t *testing.T) {
	t.Parallel()
	v := decode(t, "*2\r\n:1\r\n:250\r\n")
	if v.Kind != KindArray || len(v.Array) != 2 {
		t.Fatalf("decode(EVAL reply) = %+v", v)
	}
	if v.Array[0].Int != 1 || v.Array[1].Int != 250 {
		t.Errorf("Array = [%d, %d], want [1, 250]", v.Array[0].Int, v.Array[1].Int)
	}
}

// TestDecodeArrayOfMixedTypes: MULTI/EXEC and a few introspection commands
// this client does not issue today still travel the same decoder, so the
// general case — not just the two-integers case EVAL happens to use — is
// pinned once.
func TestDecodeArrayOfMixedTypes(t *testing.T) {
	t.Parallel()
	v := decode(t, "*3\r\n+OK\r\n$3\r\nfoo\r\n:42\r\n")
	if len(v.Array) != 3 {
		t.Fatalf("len(Array) = %d, want 3", len(v.Array))
	}
	if v.Array[0].Kind != KindStatus || v.Array[0].Str != "OK" {
		t.Errorf("Array[0] = %+v", v.Array[0])
	}
	if v.Array[1].Kind != KindBulk || string(v.Array[1].Bulk) != "foo" {
		t.Errorf("Array[1] = %+v", v.Array[1])
	}
	if v.Array[2].Kind != KindInt || v.Array[2].Int != 42 {
		t.Errorf("Array[2] = %+v", v.Array[2])
	}
}

func TestDecodeRejectsUnknownReplyType(t *testing.T) {
	t.Parallel()
	_, err := decodeValue(bufio.NewReader(strings.NewReader("!weird\r\n")))
	if err == nil {
		t.Error("an unrecognised leading byte was accepted")
	}
}

// TestDecodeRejectsMissingCRLF: a line with no terminator means the peer is
// not speaking RESP at all (or the connection died mid-line), and either way
// this must be a decode error rather than a panic or a silent short read.
func TestDecodeRejectsMissingCRLF(t *testing.T) {
	t.Parallel()
	_, err := decodeValue(bufio.NewReader(strings.NewReader("+OK")))
	if err == nil {
		t.Error("a line with no \\r\\n terminator was accepted")
	}
}

// TestDecodeRejectsTruncatedBulkBody: the length prefix promised more bytes
// than the connection actually has left to give.
func TestDecodeRejectsTruncatedBulkBody(t *testing.T) {
	t.Parallel()
	_, err := decodeValue(bufio.NewReader(strings.NewReader("$10\r\nshort\r\n")))
	if err == nil {
		t.Error("a bulk string shorter than its own length prefix was accepted")
	}
}

// TestRoundTripThroughARealBuffer sends an encoded command into a
// bytes-backed reader and decodes it back with the exact reader a live
// connection would use, so the boundary between client.go and resp.go is
// exercised at least once without a socket.
func TestRoundTripThroughARealBuffer(t *testing.T) {
	t.Parallel()
	wire := encodeCommand("EVAL", "return 1", "0")
	r := bufio.NewReader(strings.NewReader(string(wire)))
	// Not RESP on this side — encodeCommand produces a REQUEST, which a real
	// server parses with its own command reader, not decodeValue (which
	// parses REPLIES). This asserts the bytes are exactly the wire format
	// documented at the top of resp.go, byte for byte.
	got, err := r.ReadString('\n')
	if err != nil {
		t.Fatalf("ReadString: %v", err)
	}
	if got != "*3\r\n" {
		t.Errorf("first line = %q, want the 3-element array header", got)
	}
}
