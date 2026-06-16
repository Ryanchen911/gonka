package types

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// catalogExclusions lists keys.go prefixes intentionally NOT in StatePrefixCatalog.
// Keep this empty unless a prefix is deliberately uncatalogued — and document why.
var catalogExclusions = map[string]bool{}

// TestStatePrefixCatalogCoversKeysGo fails when a collections.NewPrefix prefix
// declared in keys.go's persistent block is missing from StatePrefixCatalog, so
// the catalog can't silently drift (otherwise such keys land in <unmatched> buckets).
func TestStatePrefixCatalogCoversKeysGo(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "keys.go", nil, 0)
	require.NoError(t, err)

	catalogued := map[string]bool{}
	for _, p := range StatePrefixCatalog() {
		catalogued[string(p.Bytes)] = true
	}

	// Only the first `var (...)` block holds the persistent committed-store
	// prefixes. The later "TransientStore prefixes" block reuses the small
	// integer space (1..5) for the transient store, which state-stats skips and
	// the catalog intentionally omits — so don't treat those as catalog gaps.
	persistent := firstVarBlock(t, f)

	checked := 0
	for _, spec := range persistent.Specs {
		vs, ok := spec.(*ast.ValueSpec)
		if !ok {
			continue
		}
		for i, name := range vs.Names {
			if !strings.HasSuffix(name.Name, "Prefix") || catalogExclusions[name.Name] || i >= len(vs.Values) {
				continue
			}
			call, ok := vs.Values[i].(*ast.CallExpr)
			if !ok {
				continue
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "NewPrefix" || len(call.Args) != 1 {
				continue
			}
			lit, ok := call.Args[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.INT {
				continue
			}
			num, err := strconv.Atoi(lit.Value)
			require.NoError(t, err)
			checked++
			require.Truef(t, catalogued[string([]byte{byte(num)})],
				"keys.go prefix %s = NewPrefix(%d) is missing from StatePrefixCatalog", name.Name, num)
		}
	}
	require.NotZero(t, checked, "parsed no NewPrefix prefixes from the persistent var block — check the test")
}

// firstVarBlock returns the first top-level `var (...)` declaration in f, which
// in keys.go is the persistent committed-store prefix block.
func firstVarBlock(t *testing.T, f *ast.File) *ast.GenDecl {
	t.Helper()
	for _, d := range f.Decls {
		if gd, ok := d.(*ast.GenDecl); ok && gd.Tok == token.VAR {
			return gd
		}
	}
	t.Fatal("no var block found in keys.go")
	return nil
}
