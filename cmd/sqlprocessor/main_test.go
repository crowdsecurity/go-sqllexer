package main

import (
	"bufio"
	"strings"
	"testing"
)

// The encodings this binary emits are a contract: a model is fitted to one of
// them, so a change here silently changes what that model sees. These pin the
// two that "encode" and "encode-marked" produce.

func TestTokenizeLineTypesOnly(t *testing.T) {
	// Input reaches this function already stripped of quotes by readLine.
	for _, tc := range []struct{ in, want string }{
		{"anything OR x=x", "IDENT SPACE KEYWORD SPACE IDENT OPERATOR IDENT"},
		{"SELECT * FROM t", "COMMAND SPACE WILDCARD SPACE KEYWORD SPACE IDENT"},
		{"", ""},
	} {
		if got := tokenizeLineTypesOnly(tc.in); got != tc.want {
			t.Errorf("tokenizeLineTypesOnly(%q)\n   got:  %s\n   want: %s", tc.in, got, tc.want)
		}
	}
}

func TestTokenizeLineTypesOnlyMarked(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"anything' OR 'x'='x", "IDENT QUOTE SPACE KEYWORD SPACE QUOTE IDENT QUOTE OPERATOR QUOTE IDENT"},
		{"O'Brien", "IDENT QUOTE IDENT"},
		{"'-'", "QUOTE OPERATOR QUOTE"},
		{"' --", "QUOTE SPACE COMMENT"},
		{"no quotes here", "IDENT SPACE IDENT SPACE IDENT"},
		{"'", "QUOTE"},
		{`"`, "QUOTE"},
		{"", ""},
	} {
		if got := tokenizeLineTypesOnlyMarked(tc.in); got != tc.want {
			t.Errorf("tokenizeLineTypesOnlyMarked(%q)\n   got:  %s\n   want: %s", tc.in, got, tc.want)
		}
	}
}

// TestMarkedSeparatesWhatTheStripCannot is the reason the mode exists: after the
// strip a quoted tautology and an ordinary phrase are the same sequence, so
// nothing downstream can tell them apart.
func TestMarkedSeparatesWhatTheStripCannot(t *testing.T) {
	const attack, benign = "anything' OR 'x'='x", "anything or x=x"
	strip := func(s string) string {
		return tokenizeLineTypesOnly(strings.NewReplacer("'", "", `"`, "").Replace(s))
	}
	if strip(attack) != strip(benign) {
		t.Fatalf("premise no longer holds: the strip already separates these")
	}
	if tokenizeLineTypesOnlyMarked(attack) == tokenizeLineTypesOnlyMarked(benign) {
		t.Errorf("marked encoding fails to separate them: both %s",
			tokenizeLineTypesOnlyMarked(attack))
	}
}

// TestMarkedNeverLexesAQuote guards the property the strip provides and this
// mode must not lose: a dangling quote must not swallow the rest of the input.
func TestMarkedNeverLexesAQuote(t *testing.T) {
	for _, in := range []string{
		"'; DROP TABLE users; --", "unbalanced ' quote", `a "b`, "admin'--",
	} {
		for _, tok := range strings.Fields(tokenizeLineTypesOnlyMarked(in)) {
			switch tok {
			case "STRING", "INCOMPLETE_STRING", "QUOTED_IDENT":
				t.Errorf("%q produced %s: a quote reached the lexer", in, tok)
			}
		}
	}
}

func TestReadLineStripQuotesFlag(t *testing.T) {
	const line = `"1' OR '1'='1"` + "\n" // one JSON-encoded record
	for _, tc := range []struct {
		strip bool
		want  string
	}{
		{true, "1 OR 1=1"},
		{false, "1' OR '1'='1"},
	} {
		got, err := readLine(bufio.NewReader(strings.NewReader(line)), tc.strip)
		if err != nil {
			t.Fatalf("readLine(strip=%v): %v", tc.strip, err)
		}
		if got != tc.want {
			t.Errorf("readLine(strip=%v) = %q, want %q", tc.strip, got, tc.want)
		}
	}
}

// A MySQL executable comment runs on the server but encodes as a single
// comment token, so `1/*!50000union select pw from users*/` reaches a model as
// two tokens with the union hidden inside one of them. -executable-comments
// unfolds the body; it is off by default so the shipped encodings do not move.
func TestExecutableCommentsFlag(t *testing.T) {
	const in = "1/*!50000union select pw from users*/"

	if got, want := tokenizeLineTypesOnly(in), "NUMBER MULTILINE_COMMENT"; got != want {
		t.Errorf("default\n   got:  %s\n   want: %s", got, want)
	}

	execComments = true
	defer func() { execComments = false }()

	want := "NUMBER KEYWORD SPACE COMMAND SPACE IDENT SPACE KEYWORD SPACE IDENT"
	if got := tokenizeLineTypesOnly(in); got != want {
		t.Errorf("-executable-comments\n   got:  %s\n   want: %s", got, want)
	}

	// The marked encoding picks the option up too.
	if got := tokenizeLineTypesOnlyMarked(in); got != want {
		t.Errorf("marked -executable-comments\n   got:  %s\n   want: %s", got, want)
	}

	// A quote ends the segment and the lexer scanning it, so an executable
	// comment opened before a quote does not stay open across it: the tail is
	// lexed on its own and the closing */ falls out as WILDCARD OPERATOR.
	// Quote positions are the point of this encoding, so they win.
	wantSplit := "NUMBER KEYWORD SPACE COMMAND SPACE QUOTE IDENT WILDCARD OPERATOR"
	if got := tokenizeLineTypesOnlyMarked("1/*!50000union select 'pw*/"); got != wantSplit {
		t.Errorf("marked across a quote\n   got:  %s\n   want: %s", got, wantSplit)
	}
}
