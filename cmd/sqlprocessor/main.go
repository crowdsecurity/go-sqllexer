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

func main() {
	format := flag.String("format", "json", "Output format: json, jsonl or txt")
	input := flag.String("input", "", "Input file (optional; can also pass files as args)")
	query := flag.String("query", "", "SQL query string input (optional)")
	output := flag.String("output", "", "Output file (optional; default stdout for query/stdin)")
	outDir := flag.String("outdir", "", "Output directory (default: same as input file)")
	includeEmpty := flag.Bool("include-empty", false, "Include empty/whitespace-only lines")
	mode := flag.String("mode", "analyze", "Processing mode: analyze or tokenize")
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

	if *mode != "analyze" && *mode != "tokenize" {
		fmt.Fprintf(os.Stderr, "Invalid -mode %q (expected analyze or tokenize)\n", *mode)
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
			line, err := readLine(reader)
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

			var v any
			if mode == "tokenize" {
				v = tokenizeLineTypesOnly(line)
			} else {
				v = tokenizeLine(line, lineNum)
			}
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
			line, err := readLine(reader)
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

			var v any
			if mode == "tokenize" {
				v = tokenizeLineTypesOnly(line)
			} else {
				v = tokenizeLine(line, lineNum)
			}
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
			line, err := readLine(reader)
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

func tokenizeLineTypesOnly(line string) string {
	lexer := sqllexer.New(line)
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

func tokenizeLine(line string, lineNum int) record {
	lexer := sqllexer.New(line)

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

func readLine(reader *bufio.Reader) (string, error) {
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
	return s, nil
}
