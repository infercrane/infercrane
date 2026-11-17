package controlapi

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// internalWords are the control plane's own names for a run, a setup and a
// roll out. They stay in paths, codes, fields and state values, which are a
// contract. An error message is read by a person in the dashboard or the CLI,
// so it uses the words those surfaces use.
var internalWords = regexp.MustCompile(`(?i)\b(campaigns?|candidates?|promot(e|ed|ion))\b`)

// featureNames are product names that contain one of those words and keep it.
// Environment promotion copies a release between environments; it is not a
// roll out of a setup.
var featureNames = strings.NewReplacer("environment promotion", "")

// TestErrorMessagesUseCustomerWords fails when a control API error message
// says campaign, candidate or promote. Error codes are not checked: they are
// stable identifiers that clients match on.
func TestErrorMessagesUseCustomerWords(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fileSet := token.NewFileSet()
	checked := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		source, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := parser.ParseFile(fileSet, name, source, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(parsed, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			function, ok := call.Fun.(*ast.Ident)
			if !ok || function.Name != "writeError" || len(call.Args) != 4 {
				return true
			}
			literal, ok := call.Args[3].(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
				return true
			}
			message, err := strconv.Unquote(literal.Value)
			if err != nil {
				return true
			}
			checked++
			if word := internalWords.FindString(featureNames.Replace(message)); word != "" {
				t.Errorf("%s: error message says %q; say run, setup or roll out: %q", fileSet.Position(literal.Pos()), word, message)
			}
			return true
		})
	}
	if checked == 0 {
		t.Fatal("no writeError messages were found; the check no longer reads this package")
	}
}
