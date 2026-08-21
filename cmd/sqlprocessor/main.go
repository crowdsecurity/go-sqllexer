package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/DataDog/go-sqllexer"
)

// execComments mirrors -executable-comments. It is read-only after flag
// parsing, so the tokenizers can reach it without threading a parameter
// through every call site.
//
// It defaults off because the encodings this tool emits are what downstream
// models were trained on, and turning it on changes them: a payload that used
// to encode as NUMBER MULTILINE_COMMENT now yields the tokens MySQL actually
// executes. Enable it and retrain together.
var execComments bool

func newLexer(input string) *sqllexer.Lexer {
	if execComments {
		return sqllexer.New(input, sqllexer.WithExecutableComments(true))
	}
	return sqllexer.New(input)
}

type tokenOut struct {
	Type  string `json:"type"`
	Value string `json:"value"`
}

type record struct {
	Line     int        `json:"line"`
	Query    string     `json:"query"`
	Tokens   []tokenOut `json:"tokens"`
	HasError bool       `json:"has_error,omitempty"`
}

// encoded is the lean wire shape for -mode encode: the joined token type names
// plus the input line number the consumer needs to correlate its own payload.
// The counter burns a number for skipped blank input, so output lines are a
// subsequence of input positions, not a 1:1 mapping.
type encoded struct {
	Line int    `json:"line"`
	Text string `json:"text"`
}

func main() {
	format := flag.String("format", "json", "Output format: json, jsonl or txt")
	input := flag.String("input", "", "Input file (optional; can also pass files as args)")
	query := flag.String("query", "", "SQL query string input (optional)")
	output := flag.String("output", "", "Output file (optional; default stdout for query/stdin)")
	outDir := flag.String("outdir", "", "Output directory (default: same as input file)")
	includeEmpty := flag.Bool("include-empty", false, "Include empty/whitespace-only lines")
	mode := flag.String("mode", "analyze", "Processing mode: analyze, tokenize or encode")
	flag.BoolVar(&execComments, "executable-comments", false,
		"Lex the body of MySQL executable comments (/*! ... */) as SQL instead of emitting one comment token")
	flag.Parse()

	inputs := make([]string, 0, 1+len(flag.Args()))
	if *input != "" {
		inputs = append(inputs, *input)
	}
	inputs = append(inputs, flag.Args()...)

	if *format != "json" && *format != "txt" && *format != "jsonl" {
		fmt.Fprintf(os.Stderr, "Invalid -format %q (expected json, jsonl, or txt)\n", *format)
		os.Exit(2)
	}

	switch *mode {
	case "analyze", "tokenize", "encode", "encode-marked":
	default:
		fmt.Fprintf(os.Stderr, "Invalid -mode %q (expected analyze, tokenize or encode)\n", *mode)
		os.Exit(2)
	}

	if *output != "" && *outDir != "" {
		fmt.Fprintln(os.Stderr, "Cannot use -output with -outdir")
		os.Exit(2)
	}

	if *query != "" && len(inputs) > 0 {
		fmt.Fprintln(os.Stderr, "Cannot use -query with file inputs")
		os.Exit(2)
	}

	if *output != "" && len(inputs) > 1 {
		fmt.Fprintln(os.Stderr, "-output only supports a single input file or -query/stdin")
		os.Exit(2)
	}

	if *outDir != "" {
		if err := os.MkdirAll(*outDir, 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to create outdir %q: %v\n", *outDir, err)
			os.Exit(1)
		}
	}

	exitCode := 0
	if *query != "" {
		encoded, _ := json.Marshal(*query)
		if err := processReaderToPath(strings.NewReader(string(encoded)+"\n"), *format, *output, *includeEmpty, *mode); err != nil {
			fmt.Fprintf(os.Stderr, "Error processing query: %v\n", err)
			exitCode = 1
		}
		os.Exit(exitCode)
	}

	if len(inputs) == 0 {
		if err := processReaderToPath(os.Stdin, *format, *output, *includeEmpty, *mode); err != nil {
			fmt.Fprintf(os.Stderr, "Error processing stdin: %v\n", err)
			exitCode = 1
		}
		os.Exit(exitCode)
	}

	for _, path := range inputs {
		if err := processFile(path, *format, *outDir, *output, *includeEmpty, *mode); err != nil {
			fmt.Fprintf(os.Stderr, "Error processing %s: %v\n", path, err)
			exitCode = 1
		}
	}

	os.Exit(exitCode)
}

func processFile(path, format, outDir, output string, includeEmpty bool, mode string) error {
	inFile, err := os.Open(path)
	if err != nil {
		return err
	}
	defer inFile.Close()

	var out io.Writer
	if output != "" {
		outFile, err := os.Create(output)
		if err != nil {
			return err
		}
		defer outFile.Close()
		out = outFile
	} else {
		outPath, err := outputPath(path, outDir, format)
		if err != nil {
			return err
		}
		outFile, err := os.Create(outPath)
		if err != nil {
			return err
		}
		defer outFile.Close()
		out = outFile
	}

	return processReader(inFile, format, out, includeEmpty, mode)
}

func outputPath(inputPath, outDir, format string) (string, error) {
	base := filepath.Base(inputPath)
	base = strings.TrimSuffix(base, filepath.Ext(base))

	suffix := ".tokens.json"
	switch format {
	case "txt":
		suffix = ".tokens.txt"
	case "jsonl":
		suffix = ".tokens.jsonl"
	}
	filename := base + suffix

	if outDir == "" {
		return filepath.Join(filepath.Dir(inputPath), filename), nil
	}
	return filepath.Join(outDir, filename), nil
}

func processReaderToPath(r io.Reader, format, output string, includeEmpty bool, mode string) error {
	if output == "" {
		return processReader(r, format, os.Stdout, includeEmpty, mode)
	}
	outFile, err := os.Create(output)
	if err != nil {
		return err
	}
	defer outFile.Close()
	return processReader(r, format, outFile, includeEmpty, mode)
}

func processReader(r io.Reader, format string, out io.Writer, includeEmpty bool, mode string) error {
	reader := bufio.NewReader(r)
	lineNum := 0

	switch format {
	case "json":
		if _, err := out.Write([]byte("[\n")); err != nil {
			return err
		}
		first := true
		for {
			line, err := readLine(reader, mode != "encode-marked")
			if err != nil {
				if errors.Is(err, io.EOF) {
					break
				}
				return err
			}
			lineNum++
			if !includeEmpty && strings.TrimSpace(line) == "" {
				continue
			}

			v := encodeValue(mode, line, lineNum)
			if !first {
				if _, err := out.Write([]byte(",\n")); err != nil {
					return err
				}
			}
			first = false

			blob, err := json.Marshal(v)
			if err != nil {
				return err
			}
			if _, err := out.Write(blob); err != nil {
				return err
			}
		}
		if _, err := out.Write([]byte("\n]\n")); err != nil {
			return err
		}
	case "jsonl":
		writer := bufio.NewWriter(out)
		for {
			line, err := readLine(reader, mode != "encode-marked")
			if err != nil {
				if errors.Is(err, io.EOF) {
					break
				}
				return err
			}
			lineNum++
			if !includeEmpty && strings.TrimSpace(line) == "" {
				continue
			}

			v := encodeValue(mode, line, lineNum)
			blob, err := json.Marshal(v)
			if err != nil {
				return err
			}
			blob = append(blob, '\n')
			if _, err := writer.Write(blob); err != nil {
				return err
			}
		}
		if err := writer.Flush(); err != nil {
			return err
		}
	case "txt":
		writer := bufio.NewWriter(out)
		for {
			line, err := readLine(reader, mode != "encode-marked")
			if err != nil {
				if errors.Is(err, io.EOF) {
					break
				}
				return err
			}
			lineNum++
			if !includeEmpty && strings.TrimSpace(line) == "" {
				continue
			}

			if mode == "tokenize" {
				if _, err := fmt.Fprintln(writer, tokenizeLineTypesOnly(line)); err != nil {
					return err
				}
			} else if mode == "encode" {
				if _, err := fmt.Fprintf(writer, "%d\t%s\n", lineNum, tokenizeLineTypesOnly(line)); err != nil {
					return err
				}
			} else if mode == "encode-marked" {
				if _, err := fmt.Fprintf(writer, "%d\t%s\n", lineNum, tokenizeLineTypesOnlyMarked(line)); err != nil {
					return err
				}
			} else {
				rec := tokenizeLine(line, lineNum)
				if err := writeTxtRecord(writer, rec); err != nil {
					return err
				}
			}
		}
		if err := writer.Flush(); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unsupported format: %s", format)
	}

	return nil
}

// encodeValue picks the value a mode serialises. Every mode belongs here: a
// missing case falls through to the analyze record and ships the wrong shape
// without erroring.
func encodeValue(mode, line string, lineNum int) any {
	switch mode {
	case "tokenize":
		return tokenizeLineTypesOnly(line)
	case "encode":
		return encoded{Line: lineNum, Text: tokenizeLineTypesOnly(line)}
	case "encode-marked":
		return encoded{Line: lineNum, Text: tokenizeLineTypesOnlyMarked(line)}
	default:
		return tokenizeLine(line, lineNum)
	}
}

func tokenizeLineTypesOnly(line string) string {
	lexer := newLexer(line)
	var types []string
	for {
		tok := lexer.Scan()
		if tok.Type == sqllexer.EOF {
			break
		}
		types = append(types, tokenTypeName(tok.Type))
	}
	return strings.Join(types, " ")
}

// tokenizeLineTypesOnlyMarked is tokenizeLineTypesOnly with quote positions
// preserved as a QUOTE token.
//
// Deleting quotes stops a dangling one swallowing the input, but it also erases
// the most common injection shape there is. After the strip,
//
//	"anything' OR 'x'='x"  and  "anything or x=x"
//
// are the same token sequence, so nothing downstream can tell an attack from an
// ordinary phrase. Here the line is split *at* each quote, each segment lexed
// separately, and a QUOTE emitted between them:
//
//	IDENT QUOTE SPACE KEYWORD SPACE QUOTE IDENT QUOTE OPERATOR QUOTE IDENT
//
// A quote still never reaches the lexer, so the swallowing problem stays fixed.
// This is a different feature encoding, not a refinement of the other one: a
// quote becomes a lexical boundary, so `12'34` is NUMBER QUOTE NUMBER here and a
// single NUMBER under the strip. A model trained on one cannot score the other.
//
// Callers must read this line with readLine(reader, false); with the quotes
// already deleted it degrades to tokenizeLineTypesOnly.
func tokenizeLineTypesOnlyMarked(line string) string {
	var types []string
	start := 0
	for i := 0; i < len(line); i++ {
		if c := line[i]; c == '\'' || c == '"' {
			types = appendSegmentTypes(types, line[start:i])
			types = append(types, quoteTokenName)
			start = i + 1
		}
	}
	types = appendSegmentTypes(types, line[start:])
	return strings.Join(types, " ")
}

// quoteTokenName is not a go-sqllexer type; the encoding inserts it. Upper case
// and space-free like every other name, so it survives a whitespace split.
const quoteTokenName = "QUOTE"

func appendSegmentTypes(dst []string, segment string) []string {
	if segment == "" {
		return dst
	}
	lexer := newLexer(segment)
	for {
		tok := lexer.Scan()
		if tok == nil || tok.Type == sqllexer.EOF {
			return dst
		}
		dst = append(dst, tokenTypeName(tok.Type))
	}
}

func tokenizeLine(line string, lineNum int) record {
	lexer := newLexer(line)

	tokens := make([]tokenOut, 0, 32)
	hasError := false
	for {
		tok := lexer.Scan()
		if tok.Type == sqllexer.EOF {
			break
		}
		if tok.Type == sqllexer.ERROR {
			hasError = true
		}
		tokens = append(tokens, tokenOut{
			Type:  tokenTypeName(tok.Type),
			Value: tok.Value,
		})
	}

	return record{
		Line:     lineNum,
		Query:    line,
		Tokens:   tokens,
		HasError: hasError,
	}
}

func writeTxtRecord(w io.Writer, rec record) error {
	parts := make([]string, 0, len(rec.Tokens))
	for _, tok := range rec.Tokens {
		parts = append(parts, tok.Type+":"+strconv.Quote(tok.Value))
	}
	line := strings.Join(parts, " ")
	if rec.HasError {
		line = line + " ERROR:true"
	}
	_, err := fmt.Fprintf(w, "%d\t%s\n", rec.Line, line)
	return err
}

func tokenTypeName(t sqllexer.TokenType) string {
	switch t {
	case sqllexer.ERROR:
		return "ERROR"
	case sqllexer.EOF:
		return "EOF"
	case sqllexer.SPACE:
		return "SPACE"
	case sqllexer.STRING:
		return "STRING"
	case sqllexer.INCOMPLETE_STRING:
		return "INCOMPLETE_STRING"
	case sqllexer.NUMBER:
		return "NUMBER"
	case sqllexer.IDENT:
		return "IDENT"
	case sqllexer.QUOTED_IDENT:
		return "QUOTED_IDENT"
	case sqllexer.OPERATOR:
		return "OPERATOR"
	case sqllexer.WILDCARD:
		return "WILDCARD"
	case sqllexer.COMMENT:
		return "COMMENT"
	case sqllexer.MULTILINE_COMMENT:
		return "MULTILINE_COMMENT"
	case sqllexer.PUNCTUATION:
		return "PUNCTUATION"
	case sqllexer.DOLLAR_QUOTED_FUNCTION:
		return "DOLLAR_QUOTED_FUNCTION"
	case sqllexer.DOLLAR_QUOTED_STRING:
		return "DOLLAR_QUOTED_STRING"
	case sqllexer.POSITIONAL_PARAMETER:
		return "POSITIONAL_PARAMETER"
	case sqllexer.BIND_PARAMETER:
		return "BIND_PARAMETER"
	case sqllexer.FUNCTION:
		return "FUNCTION"
	case sqllexer.SYSTEM_VARIABLE:
		return "SYSTEM_VARIABLE"
	case sqllexer.UNKNOWN:
		return "UNKNOWN"
	case sqllexer.COMMAND:
		return "COMMAND"
	case sqllexer.KEYWORD:
		return "KEYWORD"
	case sqllexer.JSON_OP:
		return "JSON_OP"
	case sqllexer.BOOLEAN:
		return "BOOLEAN"
	case sqllexer.NULL:
		return "NULL"
	case sqllexer.PROC_INDICATOR:
		return "PROC_INDICATOR"
	case sqllexer.CTE_INDICATOR:
		return "CTE_INDICATOR"
	case sqllexer.ALIAS_INDICATOR:
		return "ALIAS_INDICATOR"
	default:
		return fmt.Sprintf("TokenType(%d)", int(t))
	}
}

// readLine reads one JSONL record. stripQuotes selects the feature encoding's
// preprocessing: true deletes quotes before lexing (the "encode" contract),
// false keeps them for a caller that marks their positions itself
// ("encode-marked"). See tokenizeLineTypesOnlyMarked for why that matters.
func readLine(reader *bufio.Reader, stripQuotes bool) (string, error) {
	var buf []byte
	for {
		chunk, err := reader.ReadSlice('\n')
		buf = append(buf, chunk...)
		if err == nil {
			break
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(err, io.EOF) {
			if len(buf) == 0 {
				return "", io.EOF
			}
			break
		}
		return "", err
	}

	line := strings.TrimSpace(string(buf))
	if line == "" {
		return "", nil
	}

	var s string
	if err := json.Unmarshal([]byte(line), &s); err != nil {
		return "", fmt.Errorf("invalid JSONL input %q: %w", line, err)
	}

	// Quotes are stripped before lexing because SQLi payloads are full of
	// unbalanced ones, and a single dangling quote makes the lexer swallow the
	// whole remainder of the query into one INCOMPLETE_STRING or ERROR token --
	// collapsing exactly the structure the model needs to see. Callers that run
	// inference must apply the same strip or their features will not match.
	s = strings.TrimSuffix(s, "\n")
	s = strings.TrimSuffix(s, "\r")
	if !stripQuotes {
		return s, nil
	}
	s = strings.ReplaceAll(s, "'", "")
	s = strings.ReplaceAll(s, "\"", "")
	return s, nil
}
