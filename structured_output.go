package sdk

import (
	"log/slog"

	json "github.com/bytedance/sonic"
	"github.com/invopop/jsonschema"
)

type OutputSchema = jsonschema.Schema

func NewOutputSchema(v any) map[string]any {
	r := &jsonschema.Reflector{
		// Inline the root struct (type/properties/required at top level)
		// instead of emitting a top-level $ref into $defs.
		ExpandedStruct: true,
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
