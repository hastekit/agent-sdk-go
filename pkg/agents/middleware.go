package agents

// Middleware wraps agent operations. Embed NoopMiddleware and override the
// Wrap methods relevant to your implementation. All methods are statically
// checked at registration; first registered middleware is outermost.
type Middleware interface {
	ToolCallMiddleware
	ModelCallMiddleware
	HistoryMiddleware
	PromptMiddleware
}

// NoopMiddleware supplies pass-through implementations for every operation.
type NoopMiddleware struct{}

var _ Middleware = NoopMiddleware{}

func (NoopMiddleware) WrapToolCall(next ToolCallFunc) ToolCallFunc             { return next }
func (NoopMiddleware) WrapModelCall(next ModelCallFunc) ModelCallFunc          { return next }
func (NoopMiddleware) WrapLoadMessages(next LoadMessagesFunc) LoadMessagesFunc { return next }
func (NoopMiddleware) WrapSaveMessages(next SaveMessagesFunc) SaveMessagesFunc { return next }
func (NoopMiddleware) WrapGetPrompt(next GetPromptFunc) GetPromptFunc          { return next }

// ToolCallMiddlewaresOf narrows a middleware list to the tool-call side, which is what the
// executor runs. Go has no covariance for slices, so the conversion is a loop.
func ToolCallMiddlewaresOf(middlewares []Middleware) []ToolCallMiddleware {
	if len(middlewares) == 0 {
		return nil
	}
	out := make([]ToolCallMiddleware, 0, len(middlewares))
	for _, middleware := range middlewares {
		if middleware != nil {
			out = append(out, middleware)
		}
	}
	return out
}

// ModelCallMiddlewaresOf narrows a middleware list to the model-call side, which is what
// the agent loop runs.
func ModelCallMiddlewaresOf(middlewares []Middleware) []ModelCallMiddleware {
	if len(middlewares) == 0 {
		return nil
	}
	out := make([]ModelCallMiddleware, 0, len(middlewares))
	for _, middleware := range middlewares {
		if middleware != nil {
			out = append(out, middleware)
		}
	}
	return out
}

func HistoryMiddlewaresOf(middlewares []Middleware) []HistoryMiddleware {
	var out []HistoryMiddleware
	for _, m := range middlewares {
		if m != nil {
			out = append(out, m)
		}
	}
	return out
}
func PromptMiddlewaresOf(middlewares []Middleware) []PromptMiddleware {
	var out []PromptMiddleware
	for _, m := range middlewares {
		if m != nil {
			out = append(out, m)
		}
	}
	return out
}
