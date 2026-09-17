package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/dop251/goja"
)

func jsonValue(v any) (any, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var result any
	err = json.Unmarshal(data, &result)
	return result, err
}

// Each evaluation gets a fresh VM and JSON copies of input; it cannot mutate
// another node's state. This is not a memory-isolated sandbox for hostile code.
func evalJS(ctx context.Context, source string, in *Input, timeout time.Duration) (any, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	vm := goja.New()
	vm.SetMaxCallStackSize(512)
	stop := context.AfterFunc(ctx, func() { vm.Interrupt(ctx.Err()) })
	defer stop()
	state, err := json.Marshal(in.RunContext)
	if err != nil {
		return nil, err
	}
	meta, err := json.Marshal(in.Metadata)
	if err != nil {
		return nil, err
	}
	// JSON.parse creates ordinary JS objects rather than exposing Go map methods.
	if err = vm.Set("__stateJSON", string(state)); err != nil {
		return nil, err
	}
	if err = vm.Set("__metaJSON", string(meta)); err != nil {
		return nil, err
	}
	_, err = vm.RunString(`const context = JSON.parse(__stateJSON); const input = context.input || {}; const nodes = context.nodes || {}; const metadata = JSON.parse(__metaJSON);`)
	if err != nil {
		return nil, err
	}
	value, err := vm.RunString(source)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, err
	}
	if goja.IsUndefined(value) {
		return nil, fmt.Errorf("JavaScript returned undefined")
	}
	// Serialize within the interrupted VM, including user-defined getters/toJSON.
	if err = vm.Set("__result", value); err != nil {
		return nil, err
	}
	encoded, err := vm.RunString(`if (__result instanceof Promise) { throw new Error("async JavaScript is not supported"); } JSON.stringify(__result)`)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, err
	}
	if goja.IsUndefined(encoded) {
		return nil, fmt.Errorf("JavaScript result is not JSON")
	}
	var out any
	err = json.Unmarshal([]byte(encoded.String()), &out)
	return out, err
}
func expression(source string) string { return "(" + source + "\n)" }
func validateJS(source string) error  { _, err := goja.Compile("workflow", source, true); return err }

// A full scalar ${{ expression }} preserves its JSON type. Build strings using
// JavaScript template literals inside the expression; partial interpolation is
// intentionally not performed.
func resolveValue(ctx context.Context, v any, in *Input, timeout time.Duration) (any, error) {
	switch x := v.(type) {
	case string:
		s := strings.TrimSpace(x)
		if strings.HasPrefix(s, "${{") && strings.HasSuffix(s, "}}") {
			return evalJS(ctx, expression(strings.TrimSpace(s[3:len(s)-2])), in, timeout)
		}
		return x, nil
	case map[string]any:
		out := map[string]any{}
		for k, v := range x {
			resolved, err := resolveValue(ctx, v, in, timeout)
			if err != nil {
				return nil, fmt.Errorf("field %s: %w", k, err)
			}
			out[k] = resolved
		}
		return out, nil
	case []any:
		out := make([]any, len(x))
		for i, v := range x {
			r, err := resolveValue(ctx, v, in, timeout)
			if err != nil {
				return nil, err
			}
			out[i] = r
		}
		return out, nil
	default:
		return v, nil
	}
}

func validateBindings(v any) error {
	switch x := v.(type) {
	case string:
		s := strings.TrimSpace(x)
		if strings.HasPrefix(s, "${{") {
			if !strings.HasSuffix(s, "}}") {
				return fmt.Errorf("unterminated workflow expression")
			}
			return validateJS(expression(strings.TrimSpace(s[3 : len(s)-2])))
		}
	case map[string]any:
		for key, value := range x {
			if err := validateBindings(value); err != nil {
				return fmt.Errorf("field %s: %w", key, err)
			}
		}
	case []any:
		for _, value := range x {
			if err := validateBindings(value); err != nil {
				return err
			}
		}
	}
	return nil
}
