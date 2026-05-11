// Package protocol parses and formats the explicit `{to #alias}` route directive
// used by orchestrator messages. It is a layer-0 pure package: it does not
// resolve aliases against the registry, only validates directive syntax.
package protocol

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// RouteDirective represents an optional explicit route directive in message text.
type RouteDirective struct {
	TargetAlias string
	Payload     string
	IsExplicit  bool
	RawHeader   string
}

// ErrInvalidRouteDirective is returned when parsing appears to have an attempted
// salutation but it does not match the strict directive grammar.
var ErrInvalidRouteDirective = errors.New("invalid route directive")

var routeDirectiveRE = regexp.MustCompile(`^\s*\{to\s+#([A-Za-z0-9_-]{1,64})\}\s*(.*)$`)

// ParseDirective parses an input body for an explicit route directive.
func ParseDirective(body string) (RouteDirective, error) {
	normalized := normalizeLineEndings(body)
	lines := strings.Split(normalized, "\n")

	idx := firstNonEmptyLineIndex(lines)
	if idx == -1 {
		return RouteDirective{Payload: body}, nil
	}

	header := lines[idx]
	if match := routeDirectiveRE.FindStringSubmatch(header); len(match) == 3 {
		payloadLines := lines[idx+1:]
		payload := match[2]
		if len(payloadLines) > 0 {
			if payload == "" {
				payload = strings.Join(payloadLines, "\n")
			} else {
				payload = payload + "\n" + strings.Join(payloadLines, "\n")
			}
		}

		return RouteDirective{
			TargetAlias: match[1],
			Payload:     payload,
			IsExplicit:  true,
			RawHeader:   header,
		}, nil
	}

	if appearsLikeDirective(header) {
		return RouteDirective{}, fmt.Errorf("parse directive: %w", ErrInvalidRouteDirective)
	}

	return RouteDirective{Payload: body}, nil
}

// Format renders a Salutation header for the given alias.
func Format(alias string) string {
	return "{to #" + alias + "}"
}

// ExampleHeader returns an example salutation header.
func ExampleHeader(alias string) string {
	return Format(alias)
}

func normalizeLineEndings(body string) string {
	body = strings.ReplaceAll(body, "\r\n", "\n")
	body = strings.ReplaceAll(body, "\r", "\n")
	return body
}

func firstNonEmptyLineIndex(lines []string) int {
	for i, line := range lines {
		if strings.TrimSpace(line) != "" {
			return i
		}
	}
	return -1
}

func appearsLikeDirective(line string) bool {
	// Match the leading whitespace class used by routeDirectiveRE (`\s` in Go
	// regexp covers `[\t\n\f\r ]`). Lines are already split on `\n` and CR is
	// normalized away, but trimming the rest keeps both code paths consistent.
	trimmed := strings.TrimLeft(line, " \t\f")
	lower := strings.ToLower(trimmed)
	if !strings.HasPrefix(lower, "{to") {
		return false
	}
	// Distinguish a real directive attempt (e.g. `{to #X}`, `{to}`, `{to#X}`)
	// from plain text that incidentally starts with the letters "to"
	// (e.g. `{tooling update}`, `{today}`). Only treat the line as an attempt
	// when `{to` is followed by whitespace, `#`, `}`, or end of input.
	rest := lower[len("{to"):]
	if rest == "" {
		return true
	}
	switch rest[0] {
	case ' ', '\t', '\f', '#', '}':
		return true
	default:
		return false
	}
}
