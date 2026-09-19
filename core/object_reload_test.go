package core

import (
	"context"
	"testing"

	"github.com/dagger/dagger/dagql"
	"github.com/dagger/dagger/engine"
	"github.com/stretchr/testify/require"
)

func TestModuleObjectStateMerge(t *testing.T) {
	oldSchema := map[string]string{"null": "String?", "empty": "[String!]!", "private": "String!", "ref": "Container!"}
	newSchema := map[string]string{"null": "String?", "empty": "[String!]!", "private": "String!", "ref": "Container!", "added": "Int!"}
	ref := &Container{}
	old := map[string]any{"null": nil, "empty": []any{}, "private": "saved", "ref": ref}
	initial := map[string]any{"null": "default", "empty": []any{"default"}, "private": "default", "ref": &Container{}, "added": 42}
	merged, err := mergeModuleObjectState(initial, old, newSchema, oldSchema)
	require.NoError(t, err)
	require.Nil(t, merged["null"])
	require.Empty(t, merged["empty"])
	require.Equal(t, "saved", merged["private"])
	require.Same(t, ref, merged["ref"])
	require.Equal(t, 42, merged["added"])
	merged["private"] = "changed"
	require.Equal(t, "saved", old["private"])
	require.Equal(t, "default", initial["private"])
}

func TestModuleObjectStateMergeRejectsIncompatibleState(t *testing.T) {
	for _, test := range []struct {
		name        string
		old, target map[string]string
		state       map[string]any
		want        string
	}{
		{"removed private field", map[string]string{"hidden": "String!"}, map[string]string{}, map[string]any{"hidden": "saved"}, "was removed"},
		{"null type change", map[string]string{"hidden": "String?"}, map[string]string{"hidden": "Int?"}, map[string]any{"hidden": nil}, "changed type"},
		{"empty collection type change", map[string]string{"hidden": "[String!]!"}, map[string]string{"hidden": "[Int!]!"}, map[string]any{"hidden": []any{}}, "changed type"},
		{"unknown private field", map[string]string{}, map[string]string{}, map[string]any{"hidden": "saved"}, "no validated state schema"},
		{"unknown schema", map[string]string{"hidden": ""}, map[string]string{"hidden": ""}, map[string]any{"hidden": nil}, "changed type"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := mergeModuleObjectState(nil, test.state, test.target, test.old)
			require.ErrorContains(t, err, test.want)
		})
	}
}

type stateRebindTestSDK struct {
	SDK
	ModuleRuntime
	schema      map[string]string
	schemaCalls int
}

func (sdk *stateRebindTestSDK) AsRuntime() (Runtime, bool) { return sdk, true }
func (sdk *stateRebindTestSDK) Runtime(context.Context, *SchemaBuilder, dagql.ObjectResult[*ModuleSource]) (ModuleRuntime, error) {
	return sdk, nil
}
func (sdk *stateRebindTestSDK) ObjectStateSchema(context.Context, dagql.ObjectResult[*Module], string) (map[string]string, error) {
	sdk.schemaCalls++
	return sdk.schema, nil
}
func (sdk *stateRebindTestSDK) CloneForModuleSource(*ModuleSource) SDK { return sdk }
func (sdk *stateRebindTestSDK) AttachDependencyResults(context.Context, func(dagql.AnyResult) (dagql.AnyResult, error)) ([]dagql.AnyResult, error) {
	return nil, nil
}

func TestModuleObjectStateRebindIdentity(t *testing.T) {
	ctx := t.Context()
	cache, err := dagql.NewCache(ctx, "", nil, nil)
	require.NoError(t, err)
	root := &Query{}
	server := &moduleObjectTestServer{mockServer: &mockServer{}, cache: cache, root: root}
	root.Server = server
	dag := newCoreDagqlServerForTest(t, root)
	server.dag = dag
	ctx = engine.ContextWithClientMetadata(ContextWithQuery(ctx, root), &engine.ClientMetadata{ClientID: "rebind-client", SessionID: "rebind-session"})
	ctx = dagql.ContextWithCache(ctx, cache)
	installModuleObjectTestModuleClass(dag)
	installTypeDefTestClasses(dag)

	makeObject := func(revision string, fields map[string]any) dagql.ObjectResult[*ModuleObject] {
		schema := make(map[string]string, len(fields))
		for name := range fields {
			schema[name] = "String?"
		}
		source := newTypeDefAttachedResult(t, ctx, cache, dag, "source-"+revision, &ModuleSource{
			Kind: ModuleSourceKindDir, SDKImpl: &stateRebindTestSDK{schema: schema},
		})
		typeDef := NewObjectTypeDef("Test", "", nil)
		objDef := newTypeDefAttachedResult(t, ctx, cache, dag, "object-def-"+revision, typeDef)
		mod := newTypeDefAttachedResult(t, ctx, cache, dag, "module-"+revision, &Module{
			NameField: "Test", OriginalName: "Test", Source: dagql.NonNull(source),
			Deps:       NewSchemaBuilder(root, nil),
			ObjectDefs: dagql.ObjectResultArray[*TypeDef]{newTypeDefAttachedResult(t, ctx, cache, dag, "type-def-"+revision, (&TypeDef{}).WithObjectTypeDef(objDef))},
		})
		obj := &ModuleObject{Module: mod, TypeDef: typeDef, Fields: fields}
		require.NoError(t, obj.Install(ctx, dag, InstallOpts{SkipConstructor: true}))
		class, ok := dag.ObjectType(typeDef.Name)
		require.True(t, ok)
		class.Extend(dagql.FieldSpec{Name: "revision", Type: dagql.String("")},
			func(ctx context.Context, _ dagql.AnyResult, _ map[string]dagql.Input) (dagql.AnyResult, error) {
				return dagql.NewResultForCurrentCall(ctx, dagql.String(revision))
			})
		return newTypeDefAttachedResult(t, ctx, cache, dag, "receiver-"+revision, obj)
	}
	previous := makeObject("old", map[string]any{"hidden": "saved", "null": nil})
	// Each revision has its own schema, even when both expose the same names.
	dag = newCoreDagqlServerForTest(t, root)
	server.dag = dag
	installModuleObjectTestModuleClass(dag)
	installTypeDefTestClasses(dag)
	initial := makeObject("new", map[string]any{"hidden": "default", "null": "default", "added": "new default"})
	result, err := RebindModuleObjectState(ctx, dag, initial, previous)
	require.NoError(t, err)
	rebound, ok := dagql.UnwrapAs[*ModuleObject](result)
	require.True(t, ok)
	require.Equal(t, "saved", rebound.Fields["hidden"])
	require.Nil(t, rebound.Fields["null"])
	require.Equal(t, "new default", rebound.Fields["added"])
	require.Same(t, initial.Self().TypeDef, rebound.TypeDef)
	var revision dagql.String
	require.NoError(t, dag.Select(ctx, result, &revision, dagql.Selector{Field: "revision"}))
	require.Equal(t, dagql.String("new"), revision)
	oldID, err := previous.ID()
	require.NoError(t, err)
	initialID, err := initial.ID()
	require.NoError(t, err)
	newID, err := result.ID()
	require.NoError(t, err)
	require.NotEqual(t, oldID.EngineResultID(), newID.EngineResultID())
	require.NotEqual(t, initialID.EngineResultID(), newID.EngineResultID())
	frame, err := result.ResultCall()
	require.NoError(t, err)
	require.Equal(t, rebindModuleObjectStateField, frame.Field)
	require.Equal(t, initialID.EngineResultID(), frame.Receiver.ResultID)
	provenance, err := NewUserMod(initial.Self().Module).ResultCallModule(ctx)
	require.NoError(t, err)
	require.Equal(t, provenance.ResultRef.ResultID, frame.Module.ResultRef.ResultID)
	require.Len(t, frame.Args, 1)
	require.Equal(t, "previous", frame.Args[0].Name)

	// Selecting the same recipe yields the same object, not an alias of either
	// input, and loading its recipe resolves the state-bearing target object.
	again, err := RebindModuleObjectState(ctx, dag, initial, previous)
	require.NoError(t, err)
	againID, err := again.ID()
	require.NoError(t, err)
	require.Equal(t, newID.EngineResultID(), againID.EngineResultID())
	recipe, err := result.RecipeID(ctx)
	require.NoError(t, err)
	loaded, err := dag.Load(ctx, recipe)
	require.NoError(t, err)
	loadedObj, ok := dagql.UnwrapAs[*ModuleObject](loaded)
	require.True(t, ok)
	require.Equal(t, "saved", loadedObj.Fields["hidden"])

	// No code change requires no private-schema validation and retains identity.
	sdk := initial.Self().Module.Self().Source.Value.Self().SDKImpl.(*stateRebindTestSDK)
	calls := sdk.schemaCalls
	unchanged, err := RebindModuleObjectState(ctx, dag, initial, result)
	require.NoError(t, err)
	require.Equal(t, calls, sdk.schemaCalls)
	unchangedID, err := unchanged.ID()
	require.NoError(t, err)
	require.Equal(t, newID.EngineResultID(), unchangedID.EngineResultID())

	unsupported := newTypeDefDetachedResult(t, dag, "unsupported-sdk", &Module{})
	_, err = moduleObjectStateSchema(ctx, &ModuleObject{Module: unsupported, TypeDef: initial.Self().TypeDef})
	require.ErrorContains(t, err, "SDK does not support complete receiver-state validation")

	_, _, err = moduleObjectsForRebind(nil, previous)
	require.ErrorContains(t, err, "requires both target and previous receivers")
}
