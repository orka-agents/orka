/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package scan

import (
	"bytes"
	"encoding/json"
	"errors"
	"go/scanner"
	"go/token"
	"slices"

	"mvdan.cc/sh/v3/syntax"
)

var errChanged = errors.New("deletion would change more than comments; delete this comment by hand")

func verifyDeletion(path string, before, after []byte) error {
	var a, b []string
	var err error
	switch language(path, before) {
	case langGo:
		a, b = goTokens(before), goTokens(after)
	case langShell:
		if a, err = shellCanonical(path, before); err == nil {
			b, err = shellCanonical(path, after)
		}
	case langPython:
		if a, err = pythonTokens(before); err == nil {
			b, err = pythonTokens(after)
		}
	}
	if err != nil {
		return err
	}
	if !slices.Equal(a, b) {
		return errChanged
	}
	return nil
}

func goTokens(src []byte) []string {
	var s scanner.Scanner
	fset := token.NewFileSet()
	s.Init(fset.AddFile("", fset.Base(), len(src)), src, nil, 0)
	var out []string
	for {
		_, tok, lit := s.Scan()
		if tok == token.EOF {
			return out
		}
		if tok == token.SEMICOLON {
			lit = ";"
		}
		out = append(out, tok.String()+" "+lit)
	}
}

func shellCanonical(path string, src []byte) ([]string, error) {
	file, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(bytes.NewReader(src), path)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if err := syntax.NewPrinter(syntax.Minify(true)).Print(&out, file); err != nil {
		return nil, err
	}
	return []string{out.String()}, nil
}

const pythonTokenDump = `import ast, io, json, sys, tokenize
if sys.version_info < (3, 12):
    sys.exit("python3 3.12 or newer is required to tokenize f-strings")
skip = {tokenize.COMMENT, tokenize.NL}
data = sys.stdin.buffer.read()
toks = [[t.type, t.string, t.start[0]] for t in tokenize.tokenize(io.BytesIO(data).readline)
        if t.type not in skip]
lines = {}
for kind, text, row in toks:
    lines.setdefault(row, len(lines))
dump = [f"{kind} {text} {lines[row]}" for kind, text, row in toks]
dump.append(ast.dump(ast.parse(data)))
print(json.dumps(dump))
`

func pythonTokens(src []byte) ([]string, error) {
	out, err := runPython(src, pythonTokenDump)
	if err != nil {
		return nil, err
	}
	var toks []string
	if err := json.Unmarshal(out, &toks); err != nil {
		return nil, err
	}
	return toks, nil
}
