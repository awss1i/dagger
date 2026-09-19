package core

import (
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/dagger/dagger/dagql"
	"github.com/dagger/dagger/dagql/call"
)

const rebindModuleObjectStateField = "__rebindState"

// ModuleObjectStateSchema is an optional runtime capability for preserving state
// across code revisions. Unlike the public GraphQL schema, this must describe
// ALL serialized fields, including private fields. Signatures must be stable
// across code revisions and include nullability, collection element types and
// the complete nested serialized shape. Methods are not part of that shape.
// Runtimes must fail rather than return an incomplete or unknown schema.
//
// SDKs without this capability (including container SDKs) cannot currently
// rebind state across revisions. They can still reuse an unchanged revision.
type ModuleObjectStateSchema interface {
	ObjectStateSchema(context.Context, dagql.ObjectResult[*Module], string) (map[string]string, error)
}

// RebindModuleObjectState selects an engine-owned, replayable operation on the
// target revision's initial object. The previous receiver is an ID argument, not
// a closure capture or a replacement result under the old object's identity.
// Existing values always win, even when null or empty; only new fields take
// defaults from initial. The result retains the target module and class.
func RebindModuleObjectState(ctx context.Context, srv *dagql.Server, initial, previous dagql.AnyObjectResult) (dagql.AnyObjectResult, error) {
	target, old, err := moduleObjectsForRebind(initial, previous)
	if err != nil {
		return nil, err
	}
	same, err := sameModuleImplementation(ctx, target.Module, old.Module)
	if err != nil {
		return nil, err
	}
	if same {
		return previous, nil
	}
	previousID, err := previous.ID()
	if err != nil {
		return nil, fmt.Errorf("previous receiver ID: %w", err)
	}
	// Select dispatches through the server's class registry, not just the
	// receiver wrapper. Resolve the target recipe's defining server so a caller
	// still serving the old revision cannot run its rebind field instead.
	targetID, err := initial.RecipeID(ctx)
	if err != nil {
		return nil, fmt.Errorf("target receiver recipe: %w", err)
	}
	targetClass, targetServer, found, err := srv.ObjectTypeAndServerForID(ctx, targetID)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("target receiver class is unavailable")
	}
	initial, err = targetClass.New(initial)
	if err != nil {
		return nil, fmt.Errorf("bind target receiver class: %w", err)
	}
	var result dagql.AnyObjectResult
	if err := targetServer.Select(ctx, initial, &result, dagql.Selector{
		Field: rebindModuleObjectStateField,
		Args:  []dagql.NamedInput{{Name: "previous", Value: dagql.NewAnyID(previousID)}},
	}); err != nil {
		return nil, fmt.Errorf("rebind %s state: %w", target.TypeDef.OriginalName, err)
	}
	return result, nil
}

// Equivalent implementation-scoped modules may still have distinct cache result
// handles. Compare their canonical content identities, including module variants.
func sameModuleImplementation(ctx context.Context, left, right dagql.ObjectResult[*Module]) (bool, error) {
	leftScoped, err := ImplementationScopedModule(ctx, left)
	if err != nil {
		return false, err
	}
	rightScoped, err := ImplementationScopedModule(ctx, right)
	if err != nil {
		return false, err
	}
	leftDigest, err := leftScoped.ContentPreferredDigest(ctx)
	if err != nil {
		return false, err
	}
	rightDigest, err := rightScoped.ContentPreferredDigest(ctx)
	if err != nil {
		return false, err
	}
	return leftDigest == rightDigest, nil
}

func moduleObjectsForRebind(initial, previous dagql.AnyObjectResult) (*ModuleObject, *ModuleObject, error) {
	if initial == nil || previous == nil {
		return nil, nil, fmt.Errorf("state rebind requires both target and previous receivers")
	}
	target, ok := dagql.UnwrapAs[*ModuleObject](initial)
	if !ok || target == nil || target.Module.Self() == nil || target.TypeDef == nil {
		return nil, nil, fmt.Errorf("target receiver must be a module object")
	}
	old, ok := dagql.UnwrapAs[*ModuleObject](previous)
	if !ok || old == nil || old.Module.Self() == nil || old.TypeDef == nil {
		return nil, nil, fmt.Errorf("previous receiver must be a module object")
	}
	if target.TypeDef.OriginalName != old.TypeDef.OriginalName || target.Module.Self().OriginalName != old.Module.Self().OriginalName {
		return nil, nil, fmt.Errorf("cannot rebind %s.%s to %s.%s", old.Module.Self().OriginalName, old.TypeDef.OriginalName, target.Module.Self().OriginalName, target.TypeDef.OriginalName)
	}
	return target, old, nil
}

func (obj *ModuleObject) stateRebindField(ctx context.Context, srv *dagql.Server) (dagql.Field[*ModuleObject], error) {
	module, err := NewUserMod(obj.Module).ResultCallModule(ctx)
	if err != nil {
		return dagql.Field[*ModuleObject]{}, err
	}
	return dagql.Field[*ModuleObject]{
		Spec: &dagql.FieldSpec{
			Name:          rebindModuleObjectStateField,
			Type:          obj,
			Module:        module,
			IsPersistable: true,
			Args:          dagql.NewInputSpecs(dagql.InputSpec{Name: "previous", Type: dagql.AnyID{}}),
		},
		Func: func(ctx context.Context, self dagql.ObjectResult[*ModuleObject], args map[string]dagql.Input, _ call.View) (dagql.AnyResult, error) {
			id, ok := args["previous"].(dagql.AnyID)
			if !ok {
				return nil, fmt.Errorf("invalid previous receiver ID")
			}
			previousID, err := id.ID()
			if err != nil {
				return nil, err
			}
			previous, err := srv.Load(ctx, previousID)
			if err != nil {
				return nil, fmt.Errorf("load previous receiver: %w", err)
			}
			target, old, err := moduleObjectsForRebind(self, previous)
			if err != nil {
				return nil, err
			}
			// Recipe provenance installs an implementation-scoped module, while
			// the receiver may retain its original workspace-bound module. Compare
			// the canonical implementation identities rather than raw handles.
			sameClass, err := sameModuleImplementation(ctx, target.Module, obj.Module)
			if err != nil {
				return nil, err
			}
			if !sameClass {
				return nil, fmt.Errorf("target receiver class belongs to a different module revision")
			}
			oldSchema, err := moduleObjectStateSchema(ctx, old)
			if err != nil {
				return nil, fmt.Errorf("previous receiver schema: %w", err)
			}
			newSchema, err := moduleObjectStateSchema(ctx, target)
			if err != nil {
				return nil, fmt.Errorf("target receiver schema: %w", err)
			}
			fields, err := mergeModuleObjectState(target.Fields, old.Fields, newSchema, oldSchema)
			if err != nil {
				return nil, err
			}
			return dagql.NewObjectResultForCurrentCall(ctx, srv, &ModuleObject{
				Module: target.Module, TypeDef: target.TypeDef, Fields: fields,
			})
		},
	}, nil
}

func moduleObjectStateSchema(ctx context.Context, obj *ModuleObject) (map[string]string, error) {
	mod := obj.Module.Self()
	if mod.Runtime.Valid || !mod.Source.Valid || mod.Source.Value.Self().SDKImpl == nil {
		return nil, fmt.Errorf("SDK does not support complete receiver-state validation")
	}
	runtime, err := mod.LoadRuntime(ctx)
	if err != nil {
		return nil, err
	}
	validator, ok := runtime.(ModuleObjectStateSchema)
	if !ok {
		return nil, fmt.Errorf("SDK does not support complete receiver-state validation")
	}
	return validator.ObjectStateSchema(ctx, obj.Module, obj.TypeDef.OriginalName)
}

func mergeModuleObjectState(initial, previous map[string]any, targetSchema, previousSchema map[string]string) (map[string]any, error) {
	for _, name := range slices.Sorted(maps.Keys(previousSchema)) {
		oldType := previousSchema[name]
		newType, exists := targetSchema[name]
		if !exists {
			return nil, fmt.Errorf("receiver field %q was removed", name)
		}
		if oldType == "" || oldType != newType {
			return nil, fmt.Errorf("receiver field %q changed type (%s -> %s)", name, oldType, newType)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(previous)) {
		if _, exists := previousSchema[name]; !exists {
			return nil, fmt.Errorf("previous receiver field %q has no validated state schema", name)
		}
	}
	fields := maps.Clone(initial)
	if fields == nil {
		fields = make(map[string]any)
	}
	maps.Copy(fields, previous)
	return fields, nil
}
