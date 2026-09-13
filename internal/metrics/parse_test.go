package metrics

import (
	"fmt"
	"strconv"
	"strings"
)

// A test-only parser for the Prometheus text exposition format.
//
// It exists so the round-trip test can assert that what the writer emitted
// reproduces the registry's TYPED values — rendering into a golden file proves
// the bytes are stable, but only a parse proves they mean what the counters
// say. Sixty lines is also, not incidentally, the measurement ADR 0012 rests
// on: a format whose reader fits in one screen is not a format worth a fifth
// dependency.
//
// It is deliberately STRICTER than Prometheus in the places this writer could
// realistically go wrong — a repeated HELP line, a family whose samples are not
// contiguous, a sample whose name belongs to no declared family — because those
// are the three mistakes that fail an entire scrape rather than one panel.

type parsedFamily struct {
	name    string
	typ     string
	help    string
	samples []parsedSample
}

type parsedSample struct {
	name   string
	labels map[string]string
	value  float64
}

// parseExposition reads a whole document, in order.
func parseExposition(body string) (map[string]*parsedFamily, error) {
	families := make(map[string]*parsedFamily)
	var order []string
	var current *parsedFamily
	closed := make(map[string]bool)

	for n, line := range strings.Split(body, "\n") {
		if line == "" {
			continue
		}
		ln := n + 1

		if strings.HasPrefix(line, "# ") {
			kind, rest, ok := strings.Cut(strings.TrimPrefix(line, "# "), " ")
			if !ok {
				return nil, fmt.Errorf("line %d: malformed comment %q", ln, line)
			}
			name, text, _ := strings.Cut(rest, " ")
			switch kind {
			case "HELP":
				if _, seen := families[name]; seen {
					return nil, fmt.Errorf("line %d: a second HELP for %q; "+
						"Prometheus rejects the whole document for this", ln, name)
				}
				if current != nil {
					closed[current.name] = true
				}
				current = &parsedFamily{name: name, help: text}
				families[name] = current
				order = append(order, name)
			case "TYPE":
				f, ok := families[name]
				if !ok {
					return nil, fmt.Errorf("line %d: TYPE for %q before its HELP", ln, name)
				}
				f.typ = text
			default:
				return nil, fmt.Errorf("line %d: unknown comment kind %q", ln, kind)
			}
			continue
		}

		s, err := parseSample(line)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", ln, err)
		}
		owner := ownerOf(s.name, families)
		if owner == nil {
			return nil, fmt.Errorf("line %d: sample %q belongs to no declared family", ln, s.name)
		}
		if closed[owner.name] {
			return nil, fmt.Errorf("line %d: family %q resumes after another family started; "+
				"a family's samples must be contiguous", ln, owner.name)
		}
		if current != nil && owner.name != current.name {
			return nil, fmt.Errorf("line %d: sample %q appeared inside family %q",
				ln, s.name, current.name)
		}
		owner.samples = append(owner.samples, s)
	}

	// Lexical family order is part of the contract the writer promises.
	for i := 1; i < len(order); i++ {
		if order[i-1] >= order[i] {
			return nil, fmt.Errorf("families are not in lexical order: %q before %q",
				order[i-1], order[i])
		}
	}
	return families, nil
}

// ownerOf maps a sample name back onto its family, accounting for the three
// suffixes a histogram family's samples carry.
func ownerOf(sample string, families map[string]*parsedFamily) *parsedFamily {
	if f, ok := families[sample]; ok {
		return f
	}
	for _, suffix := range []string{"_bucket", "_sum", "_count"} {
		if strings.HasSuffix(sample, suffix) {
			if f, ok := families[strings.TrimSuffix(sample, suffix)]; ok {
				return f
			}
		}
	}
	return nil
}

func parseSample(line string) (parsedSample, error) {
	s := parsedSample{labels: map[string]string{}}

	name, rest, found := strings.Cut(line, "{")
	if !found {
		var value string
		name, value, found = strings.Cut(line, " ")
		if !found {
			return s, fmt.Errorf("no value in %q", line)
		}
		v, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return s, fmt.Errorf("value %q: %w", value, err)
		}
		s.name, s.value = name, v
		return s, nil
	}

	body, value, found := strings.Cut(rest, "} ")
	if !found {
		return s, fmt.Errorf("unterminated label set in %q", line)
	}
	v, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return s, fmt.Errorf("value %q: %w", value, err)
	}
	s.name, s.value = name, v

	for _, pair := range splitLabels(body) {
		k, quoted, found := strings.Cut(pair, "=")
		if !found || len(quoted) < 2 || quoted[0] != '"' || quoted[len(quoted)-1] != '"' {
			return s, fmt.Errorf("malformed label %q", pair)
		}
		if _, dup := s.labels[k]; dup {
			return s, fmt.Errorf("label %q appears twice", k)
		}
		s.labels[k] = unescape(quoted[1 : len(quoted)-1])
	}
	return s, nil
}

// splitLabels splits on commas that are not inside a quoted value, which is
// what makes a label value containing a comma parse correctly.
func splitLabels(body string) []string {
	var out []string
	inQuotes, escaped, start := false, false, 0
	for i := 0; i < len(body); i++ {
		switch {
		case escaped:
			escaped = false
		case body[i] == '\\':
			escaped = true
		case body[i] == '"':
			inQuotes = !inQuotes
		case body[i] == ',' && !inQuotes:
			out = append(out, body[start:i])
			start = i + 1
		}
	}
	if start < len(body) {
		out = append(out, body[start:])
	}
	return out
}

func unescape(s string) string {
	if !strings.Contains(s, "\\") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 >= len(s) {
			b.WriteByte(s[i])
			continue
		}
		i++
		switch s[i] {
		case 'n':
			b.WriteByte('\n')
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// sample finds one sample of a family by its exact label set.
func (f *parsedFamily) sample(name string, labels map[string]string) (parsedSample, bool) {
	for _, s := range f.samples {
		if s.name != name || len(s.labels) != len(labels) {
			continue
		}
		match := true
		for k, v := range labels {
			if s.labels[k] != v {
				match = false
				break
			}
		}
		if match {
			return s, true
		}
	}
	return parsedSample{}, false
}
