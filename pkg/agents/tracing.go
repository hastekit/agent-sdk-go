package agents

import (
	"context"

	"github.com/bytedance/sonic"
	"github.com/hastekit/agent-sdk-go/pkg/genai"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

// ExecuteWithTrace brackets a single tool execution with an OpenTelemetry
// execute_tool span following the GenAI semantic conventions: it opens the
// span, stamps the tool identity and arguments, runs exec, records the result
// (or the error), and ends the span.
//
// Every runtime calls this from its single durable tool-execution point — the
// in-process executor goroutine, the Temporal activity, the Restate step — so
// the span fires exactly once per real tool call, never on replay, and is
// always complete because each call runs start-to-finish inside one step.
//
// tool supplies the span's description metadata and may be nil; exec performs
// the real execution and is usually a method value — tool.Execute for a plain
// tool, or an MCP toolset's call method. Splitting them lets callers whose
// execution isn't a Tool.Execute (e.g. MCP wrappers) still report the tool's
// identity.
//
// The run-wrapping invoke_agent span is intentionally not opened here: under a
// durable runtime a run's start and end can execute in different processes, so
// a span opened in the loop cannot reliably bracket the run. Callers that want
// one open it outside the durable boundary (e.g. around the workflow
// invocation) and let these tool spans nest under it.
func ExecuteWithTrace(
	ctx context.Context,
	tool Tool,
	params *ToolCall,
	exec func(context.Context, *ToolCall) (*ToolCallResponse, error),
) (*ToolCallResponse, error) {
	ctx, span := tracer.Start(ctx, genai.OpExecuteTool+" "+params.Name)
	span.SetAttributes(
		attribute.String(genai.AttrOperationName, genai.OpExecuteTool),
		attribute.String(genai.AttrToolName, params.Name),
		attribute.String(genai.AttrToolCallID, params.CallID),
		attribute.String(genai.AttrToolArguments, params.Arguments),
		attribute.String(genai.AttrSessionID, params.ThreadID),
	)
	if tool != nil {
		if def := tool.Tool(ctx); def != nil && def.OfFunction != nil && def.OfFunction.Description != nil {
			span.SetAttributes(attribute.String(genai.AttrToolDescription, *def.OfFunction.Description))
		}
	}
	defer span.End()

	resp, err := exec(ctx, params)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return resp, err
	}
	if resp != nil && resp.FunctionCallOutputMessage != nil {
		if out, mErr := sonic.Marshal(resp.Output); mErr == nil {
			span.SetAttributes(attribute.String(genai.AttrToolResult, string(out)))
		}
	}
	return resp, nil
}
