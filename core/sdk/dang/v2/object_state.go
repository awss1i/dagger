package dangv2

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/dagger/dagger/core"
	dangshared "github.com/dagger/dagger/core/sdk/dang/shared"
	"github.com/dagger/dagger/dagql"
	"github.com/vito/dang/v2/pkg/dang"
	"github.com/vito/dang/v2/pkg/hm"
)

var _ core.ModuleObjectStateSchema = (*runtime)(nil)

// ObjectStateSchema inspects declarations, never invokes a user method or
// reconstructs a receiver. In particular private let fields are included even
// though they do not occur in the module's GraphQL schema.
func (r *runtime) ObjectStateSchema(ctx context.Context, mod dagql.ObjectResult[*core.Module], name string) (map[string]string, error) {
	query, err := core.CurrentQuery(ctx)
	if err != nil {
		return nil, err
	}
	schema, err := r.deps.SchemaIntrospectionJSONFileForModule(ctx)
	if err != nil {
		return nil, err
	}
	metadata, err := dangshared.NewNestedClientMetadata(ctx)
	if err != nil {
		return nil, err
	}
	var fields map[string]string
	_, err = evalDangSource(ctx, query, r.modSource, schema, metadata, true, nil, mod,
		runDangDirForModuleTypes, func(ctx context.Context, env dang.ValueScope) ([]byte, error) {
			value, found, err := env.Lookup(ctx, name)
			if err != nil {
				return nil, err
			}
			constructor, ok := value.(*dang.ConstructorFunction)
			if !found || !ok {
				return nil, fmt.Errorf("state schema: unknown object %q", name)
			}
			fields, err = dangObjectStateSchema(constructor.ObjectType)
			return nil, err
		})
	return fields, err
}

func dangObjectStateSchema(obj *dang.Type) (map[string]string, error) {
	fields := make(map[string]string)
	for name, scheme := range obj.Bindings(dang.PrivateVisibility) {
		typ, mono := scheme.Type()
		if !mono {
			return nil, fmt.Errorf("state schema: non-monotype field %q", name)
		}
		if _, method := typ.(*hm.FunctionType); method {
			continue
		}
		shape, err := dangStateTypeShape(typ, map[*dang.Type]bool{})
		if err != nil {
			return nil, fmt.Errorf("state schema field %q: %w", name, err)
		}
		encoded, err := json.Marshal(shape)
		if err != nil {
			return nil, err
		}
		fields[name] = string(encoded)
	}
	return fields, nil
}

// Use structural, deterministic signatures rather than Type.String: named
// objects print only their name, losing private/nested schema changes. Imported
// objects are engine handles, so their query methods are not serialized state.
func dangStateTypeShape(typ hm.Type, visiting map[*dang.Type]bool) (any, error) {
	switch typ := typ.(type) {
	case hm.NonNullType:
		inner, err := dangStateTypeShape(typ.Type, visiting)
		return []any{"nonNull", inner}, err
	case dang.ListType:
		inner, err := dangStateTypeShape(typ.Type, visiting)
		return []any{"list", inner}, err
	case dang.MapType:
		inner, err := dangStateTypeShape(typ.Type, visiting)
		return []any{"map", inner}, err
	case *dang.Type:
		identity := []any{typ.Kind.String(), typ.Qualifier, typ.Named}
		if typ.SourceSchema != nil && typ.Kind != dang.EnumKind {
			return append([]any{"reference"}, identity...), nil
		}
		if visiting[typ] {
			return append([]any{"recursive"}, identity...), nil
		}
		visiting[typ] = true
		defer delete(visiting, typ)
		switch typ.Kind {
		case dang.ScalarKind:
			return identity, nil
		case dang.ObjectKind, dang.EnumKind:
			fields := make(map[string]any)
			for name, scheme := range typ.Bindings(dang.PrivateVisibility) {
				fieldType, mono := scheme.Type()
				if !mono {
					return nil, fmt.Errorf("non-monotype nested field %q", name)
				}
				if _, method := fieldType.(*hm.FunctionType); method {
					continue
				}
				shape, err := dangStateTypeShape(fieldType, visiting)
				if err != nil {
					return nil, fmt.Errorf("nested field %q: %w", name, err)
				}
				fields[name] = shape
			}
			return append(identity, fields), nil
		default:
			return nil, fmt.Errorf("unsupported state type %s", typ)
		}
	case *dang.RecordType:
		fields := make(map[string]any, len(typ.Fields))
		for _, field := range typ.Fields {
			fieldType, mono := field.Value.Type()
			if !mono {
				return nil, fmt.Errorf("non-monotype record field %q", field.Key)
			}
			shape, err := dangStateTypeShape(fieldType, visiting)
			if err != nil {
				return nil, err
			}
			fields[field.Key] = shape
		}
		return []any{"record", fields}, nil
	default:
		return nil, fmt.Errorf("unsupported state type %T (%s)", typ, typ)
	}
}
