package dangv2

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/vito/dang/v2/pkg/dang"
	"github.com/vito/dang/v2/pkg/hm"
)

func TestModuleObjectStateSchemaDeclarations(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.dang"), []byte(`type Test {
  pub name: String! = "default"
  let count = 42
  let empty: [String!]! = []
  let maybe: String = null
  pub run: String! { "code" }
}
`), 0600))
	env, err := runDangDirForModuleTypes(t.Context(), dir)
	require.NoError(t, err)
	value, found, err := env.Lookup(t.Context(), "Test")
	require.NoError(t, err)
	require.True(t, found)
	schema, err := dangObjectStateSchema(value.(*dang.ConstructorFunction).ObjectType)
	require.NoError(t, err)
	require.Len(t, schema, 4)
	require.Contains(t, schema, "count")
	require.NotEqual(t, schema["name"], schema["maybe"])
	require.NotContains(t, schema, "run")
}

func TestModuleObjectStateSchemaNestedPrivateChanges(t *testing.T) {
	child := dang.NewType("Child", dang.ObjectKind)
	child.Add("hidden", hm.NewScheme(nil, dang.StringType))
	child.SetVisibility("hidden", dang.PrivateVisibility)
	parent := dang.NewType("Test", dang.ObjectKind)
	parent.Add("items", hm.NewScheme(nil, dang.ListType{Type: child}))
	parent.SetVisibility("items", dang.PrivateVisibility)
	before, err := dangObjectStateSchema(parent)
	require.NoError(t, err)
	child.Add("hidden", hm.NewScheme(nil, dang.IntType))
	after, err := dangObjectStateSchema(parent)
	require.NoError(t, err)
	require.NotEqual(t, before["items"], after["items"])

	// Self-referential schemas terminate and produce stable fingerprints.
	child.Add("next", hm.NewScheme(nil, child))
	first, err := dangObjectStateSchema(parent)
	require.NoError(t, err)
	second, err := dangObjectStateSchema(parent)
	require.NoError(t, err)
	require.Equal(t, first, second)
}
