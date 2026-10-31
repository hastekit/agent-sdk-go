package sdk

import (
	"log/slog"
	"reflect"

	json "github.com/bytedance/sonic"
	"github.com/invopop/jsonschema"
)

type OutputSchema = jsonschema.Schema

// NewOutputSchema is the JSON schema of v's type, self-contained (no $ref or
// $defs) as the LLM tool APIs require. A nil v — the zero value of an interface
// type such as any — has no type to describe, and gets the schema that accepts
// anything.
func NewOutputSchema(v any) map[string]any {
	t := reflect.TypeOf(v)
	if t == nil {
		return map[string]any{}
	}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}

	r := &jsonschema.Reflector{
		// Inline the root struct (type/properties/required at top level)
		// instead of emitting a top-level $ref into $defs. Only a named
		// struct has a definition to inline: the reflector looks it up by
		// type name, and for an anonymous struct, a map, a slice or a scalar
		// that lookup finds nothing and panics. Those reflect inline without
		// it anyway.
		ExpandedStruct: t.Kind() == reflect.Struct && t.Name() != "",
		// Inline all nested definitions so the schema is self-contained
		// (no $ref/$defs), which the LLM tool APIs require.
		DoNotReference: true,
	}

	schema := r.Reflect(v)
	// Drop $schema and $id metadata that the tool APIs reject.
	schema.Version = ""
	schema.ID = ""

	buf, err := schema.MarshalJSON()
	if err != nil {
		slog.Warn("failed to reflect output schema: " + err.Error())
		return nil
	}

	ss := map[string]any{}
	err = json.Unmarshal(buf, &ss)
	if err != nil {
		slog.Warn("failed to unmarshal output schema: " + err.Error())
		return nil
	}

	return ss
}

func NewOutputFormatJSONSchema(v any, strict bool) map[string]any {
	return map[string]any{
		"type":   "json_schema",
		"name":   "structured_output",
		"strict": strict,
		"schema": NewOutputSchema(v),
	}
}
