package protocol

import (
	"errors"
	"strings"
	"testing"
)

func TestParseDirective_InlinePayload(t *testing.T) {
	t.Parallel()

	input := "{to #Alice} check stock prices"

	got, err := ParseDirective(input)
	if err != nil {
		t.Fatalf("ParseDirective(%q): unexpected error: %v", input, err)
	}

	want := RouteDirective{
		TargetAlias: "Alice",
		Payload:     "check stock prices",
		IsExplicit:  true,
		RawHeader:   "{to #Alice} check stock prices",
	}

	if got != want {
		t.Fatalf("ParseDirective(%q): got %#v, want %#v", input, got, want)
	}
}

func TestParseDirective_MultilineBody(t *testing.T) {
	t.Parallel()

	input := "{to #Alice}\nline one\nline two"

	got, err := ParseDirective(input)
	if err != nil {
		t.Fatalf("ParseDirective(%q): unexpected error: %v", input, err)
	}

	if got.TargetAlias != "Alice" {
		t.Fatalf("TargetAlias: got %q, want %q", got.TargetAlias, "Alice")
	}
	if got.Payload != "line one\nline two" {
		t.Fatalf("Payload: got %q, want %q", got.Payload, "line one\nline two")
	}
}

func TestParseDirective_LeadingBlankLines(t *testing.T) {
	t.Parallel()

	input := "\n \n{to #Alice} hello"

	got, err := ParseDirective(input)
	if err != nil {
		t.Fatalf("ParseDirective(%q): unexpected error: %v", input, err)
	}

	if !got.IsExplicit {
		t.Fatalf("IsExplicit: got %v, want %v", got.IsExplicit, true)
	}
	if got.TargetAlias != "Alice" {
		t.Fatalf("TargetAlias: got %q, want %q", got.TargetAlias, "Alice")
	}
}

func TestParseDirective_MalformedDirective(t *testing.T) {
	t.Parallel()

	cases := []string{
		"{to Alice}",
		"{to #}",
		"{to #Alice",
	}

	for _, input := range cases {
		input := input
		t.Run(input, func(t *testing.T) {
			t.Parallel()

			_, err := ParseDirective(input)
			if !errors.Is(err, ErrInvalidRouteDirective) {
				t.Fatalf("ParseDirective(%q): got err %v, want error Is ErrInvalidRouteDirective", input, err)
			}
		})
	}
}

func TestParseDirective_SecondNonEmptyLineIsPlainText(t *testing.T) {
	t.Parallel()

	input := "hello\n{to #Alice} later"
	got, err := ParseDirective(input)
	if err != nil {
		t.Fatalf("ParseDirective(%q): unexpected error: %v", input, err)
	}

	if got.IsExplicit {
		t.Fatalf("IsExplicit: got %v, want %v", got.IsExplicit, false)
	}
	if got.TargetAlias != "" {
		t.Fatalf("TargetAlias: got %q, want empty", got.TargetAlias)
	}
	if got.Payload != input {
		t.Fatalf("Payload: got %q, want %q", got.Payload, input)
	}
}

func TestParseDirective_UnknownTargetReturnedAsAlias(t *testing.T) {
	t.Parallel()

	input := "{to #Unknown} ping"
	got, err := ParseDirective(input)
	if err != nil {
		t.Fatalf("ParseDirective(%q): unexpected error: %v", input, err)
	}

	if got.TargetAlias != "Unknown" {
		t.Fatalf("TargetAlias: got %q, want %q", got.TargetAlias, "Unknown")
	}
	if got.RawHeader != input {
		t.Fatalf("RawHeader: got %q, want %q", got.RawHeader, input)
	}
}

func TestFormat(t *testing.T) {
	t.Parallel()

	got := Format("Alice")
	if got != "{to #Alice}" {
		t.Fatalf("Format(%q): got %q, want %q", "Alice", got, "{to #Alice}")
	}
	if ExampleHeader("Alice") != got {
		t.Fatalf("ExampleHeader(%q): got %q, want %q", "Alice", ExampleHeader("Alice"), got)
	}
	if !strings.HasPrefix(got, "{to #") || !strings.HasSuffix(got, "}") {
		t.Fatalf("Format(%q): malformed header %q", "Alice", got)
	}
}

func TestParseDirective_NormalizesLineEndings(t *testing.T) {
	t.Parallel()

	t.Run("CRLF", func(t *testing.T) {
		t.Parallel()

		input := "{to #Alice}\r\nline one\r\nline two"
		got, err := ParseDirective(input)
		if err != nil {
			t.Fatalf("ParseDirective(%q): unexpected error: %v", input, err)
		}
		if got.TargetAlias != "Alice" {
			t.Fatalf("TargetAlias: got %q, want %q", got.TargetAlias, "Alice")
		}
		if got.Payload != "line one\nline two" {
			t.Fatalf("Payload: got %q, want %q", got.Payload, "line one\nline two")
		}
	})

	t.Run("CR", func(t *testing.T) {
		t.Parallel()

		input := "{to #Alice}\rbody"
		got, err := ParseDirective(input)
		if err != nil {
			t.Fatalf("ParseDirective(%q): unexpected error: %v", input, err)
		}
		if got.TargetAlias != "Alice" {
			t.Fatalf("TargetAlias: got %q, want %q", got.TargetAlias, "Alice")
		}
		if got.Payload != "body" {
			t.Fatalf("Payload: got %q, want %q", got.Payload, "body")
		}
	})
}

func TestParseDirective_MalformedAlias(t *testing.T) {
	t.Parallel()

	t.Run("AliasContainsSpace", func(t *testing.T) {
		t.Parallel()

		input := "{to #al ice}"
		_, err := ParseDirective(input)
		if !errors.Is(err, ErrInvalidRouteDirective) {
			t.Fatalf("ParseDirective(%q): got err %v, want error Is ErrInvalidRouteDirective", input, err)
		}
	})

	t.Run("AliasTooLong", func(t *testing.T) {
		t.Parallel()

		input := "{to #" + strings.Repeat("a", 65) + "}"
		_, err := ParseDirective(input)
		if !errors.Is(err, ErrInvalidRouteDirective) {
			t.Fatalf("ParseDirective(%q): got err %v, want error Is ErrInvalidRouteDirective", input, err)
		}
	})
}

func TestParseDirective_PlainTextWithToPrefixWord(t *testing.T) {
	t.Parallel()

	cases := []string{
		"{tooling update}",
		"{today is friday}",
		"{tomato}",
		"{to123}",
		"{TOOLING}",
	}

	for _, input := range cases {
		input := input
		t.Run(input, func(t *testing.T) {
			t.Parallel()

			got, err := ParseDirective(input)
			if err != nil {
				t.Fatalf("ParseDirective(%q): unexpected error: %v", input, err)
			}
			if got.IsExplicit {
				t.Fatalf("ParseDirective(%q): IsExplicit = true, want false", input)
			}
			if got.TargetAlias != "" {
				t.Fatalf("ParseDirective(%q): TargetAlias = %q, want empty", input, got.TargetAlias)
			}
			if got.Payload != input {
				t.Fatalf("ParseDirective(%q): Payload = %q, want %q", input, got.Payload, input)
			}
		})
	}
}

func TestParseDirective_BareToAndAdjacentDirective(t *testing.T) {
	t.Parallel()

	cases := []string{
		"{to",
		"{to}",
		"{to#Alice}",
	}

	for _, input := range cases {
		input := input
		t.Run(input, func(t *testing.T) {
			t.Parallel()

			_, err := ParseDirective(input)
			if !errors.Is(err, ErrInvalidRouteDirective) {
				t.Fatalf("ParseDirective(%q): got err %v, want error Is ErrInvalidRouteDirective", input, err)
			}
		})
	}
}

func TestParseDirective_PlainTextFallback(t *testing.T) {
	t.Parallel()

	input := "plain message"
	got, err := ParseDirective(input)
	if err != nil {
		t.Fatalf("ParseDirective(%q): unexpected error: %v", input, err)
	}
	if got.IsExplicit {
		t.Fatalf("IsExplicit: got %v, want %v", got.IsExplicit, false)
	}
	if got.TargetAlias != "" {
		t.Fatalf("TargetAlias: got %q, want empty", got.TargetAlias)
	}
	if got.Payload != input {
		t.Fatalf("Payload: got %q, want %q", got.Payload, input)
	}
}
