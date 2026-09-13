package metrics

import "strconv"

// This file is the whole of the Prometheus text exposition format, version
// 0.0.4. It is about a hundred lines, which is the measurement ADR 0012 rests
// on: the thing client_golang uniquely provides is a serialisation spec that
// fits on one page, not a wire protocol.
//
// The grammar, in full:
//
//	# HELP <name> <help text>
//	# TYPE <name> counter|gauge|histogram
//	<name>{<label>="<value>",...} <value>
//
// with `\` and newline escaped in a help string, and `\`, `"` and newline
// escaped in a label value.

// ContentType is what GET /api/v1/metrics must send. The version parameter is
// not decorative: a scraper uses it to choose a parser, and omitting it makes
// Prometheus guess.
const ContentType = "text/plain; version=0.0.4; charset=utf-8"

// posInf is the literal spelling of the open-ended bucket. strconv would render
// math.Inf(1) as "+Inf" too, but spelling it as a constant means the one value
// whose formatting the format itself pins can never be changed by a formatting
// decision taken for some other reason.
const posInf = "+Inf"

// labelPair is one rendered dimension. Values are escaped at render, never at
// observation, so nothing on the hot path pays for a string scan.
type labelPair struct {
	name  string
	value string
}

func appendHelp(dst []byte, name, help string) []byte {
	dst = append(dst, "# HELP "...)
	dst = append(dst, name...)
	dst = append(dst, ' ')
	dst = appendEscapedHelp(dst, help)
	return append(dst, '\n')
}

func appendType(dst []byte, name, typ string) []byte {
	dst = append(dst, "# TYPE "...)
	dst = append(dst, name...)
	dst = append(dst, ' ')
	dst = append(dst, typ...)
	return append(dst, '\n')
}

func appendUintSample(dst []byte, name string, labels []labelPair, v uint64) []byte {
	dst = appendSampleName(dst, name, labels)
	dst = strconv.AppendUint(dst, v, 10)
	return append(dst, '\n')
}

func appendIntSample(dst []byte, name string, labels []labelPair, v int64) []byte {
	dst = appendSampleName(dst, name, labels)
	dst = strconv.AppendInt(dst, v, 10)
	return append(dst, '\n')
}

func appendFloatSample(dst []byte, name string, labels []labelPair, v float64) []byte {
	dst = appendSampleName(dst, name, labels)
	dst = appendFloat(dst, v)
	return append(dst, '\n')
}

func appendSampleName(dst []byte, name string, labels []labelPair) []byte {
	dst = append(dst, name...)
	if len(labels) > 0 {
		dst = append(dst, '{')
		for i, l := range labels {
			if i > 0 {
				dst = append(dst, ',')
			}
			dst = append(dst, l.name...)
			dst = append(dst, '=', '"')
			dst = appendEscapedValue(dst, l.value)
			dst = append(dst, '"')
		}
		dst = append(dst, '}')
	}
	return append(dst, ' ')
}

// appendFloat is the ONLY float formatting in this package, and 'g' with a
// precision of -1 is the only defensible choice.
//
// -1 means "the fewest digits that round-trip exactly", which is what makes the
// formatting a pure function of the value. That matters far more for a bucket
// boundary than for a sample value: `le` is part of a series' IDENTITY, so a
// boundary that formats as 0.005 in one scrape and 5e-03 in the next does not
// change a number — it retires one time series and starts another, and every
// rate() and histogram_quantile() over the boundary silently resets. The
// boundary slices are compile-time constants in set.go so the values cannot
// drift either, which makes the pair of decisions airtight rather than merely
// careful.
func appendFloat(dst []byte, v float64) []byte {
	switch {
	case v != v: // NaN. Histogram.Observe refuses these, so this is belt and braces.
		return append(dst, "NaN"...)
	case v > maxFloat:
		return append(dst, posInf...)
	case v < -maxFloat:
		return append(dst, "-Inf"...)
	}
	return strconv.AppendFloat(dst, v, 'g', -1, 64)
}

// maxFloat is math.MaxFloat64, spelled here so expo.go needs no math import for
// one comparison.
const maxFloat = 1.7976931348623157e308

func formatBound(v float64) string {
	return string(appendFloat(make([]byte, 0, 24), v))
}

// appendEscapedValue escapes a label value: backslash, double quote and
// newline, and nothing else — the format names exactly three sequences.
//
// EVERY label value in this process today comes from a closed vocabulary
// already stringified by house code: Stop.String(), State.String(), the
// runmesh.Code* constants, the route pattern routeOf() returns. None of them
// can contain any of the three. The escaping exists anyway, because the
// guarantee worth having is "this writer cannot emit an unparseable line",
// not "our current labels happen to be safe" — and the difference between the
// two is one future tool that puts a Windows path in an error code, one
// unparseable line, and a scrape that fails WHOLESALE, taking every dashboard
// down at once rather than one panel.
func appendEscapedValue(dst []byte, s string) []byte {
	if !needsEscape(s) {
		return append(dst, s...)
	}
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '\\':
			dst = append(dst, '\\', '\\')
		case '"':
			dst = append(dst, '\\', '"')
		case '\n':
			dst = append(dst, '\\', 'n')
		default:
			dst = append(dst, c)
		}
	}
	return dst
}

// appendEscapedHelp escapes a help string. A help string is NOT quoted, so a
// double quote in it is legal and is left alone; only backslash and newline are
// special.
func appendEscapedHelp(dst []byte, s string) []byte {
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '\\':
			dst = append(dst, '\\', '\\')
		case '\n':
			dst = append(dst, '\\', 'n')
		default:
			dst = append(dst, c)
		}
	}
	return dst
}

func needsEscape(s string) bool {
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\', '"', '\n':
			return true
		}
	}
	return false
}
