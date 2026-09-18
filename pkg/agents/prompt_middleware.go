package agents

import "context"

// PromptMiddleware wraps prompt retrieval inside the provider's execution step.
// Return a prompt without calling next to short-circuit. Pass copies when
// modifying dependencies; returned errors propagate to the caller.
type PromptMiddleware interface {
	WrapGetPrompt(GetPromptFunc) GetPromptFunc
}

type GetPromptFunc func(context.Context, *Dependencies) (string, error)

func ExecuteGetPromptWithMiddleware(ctx context.Context, middlewares []PromptMiddleware, deps *Dependencies, next GetPromptFunc) (string, error) {
	for i := len(middlewares) - 1; i >= 0; i-- {
		if middlewares[i] != nil {
			next = middlewares[i].WrapGetPrompt(next)
		}
	}
	return next(ctx, deps)
}

// WrapPromptProvider binds middleware to the real provider. Durable adapters
// call its GetPrompt inside the existing prompt activity or run step.
func WrapPromptProvider(provider SystemPromptProvider, middlewares ...PromptMiddleware) SystemPromptProvider {
	if len(middlewares) == 0 {
		return provider
	}
	return &promptMiddlewareProvider{provider: provider, middlewares: middlewares}
}

type promptMiddlewareProvider struct {
	provider    SystemPromptProvider
	middlewares []PromptMiddleware
}

func (p *promptMiddlewareProvider) GetPrompt(ctx context.Context, deps *Dependencies) (string, error) {
	return ExecuteGetPromptWithMiddleware(ctx, p.middlewares, deps, func(ctx context.Context, deps *Dependencies) (string, error) {
		if p.provider == nil {
			return "", nil
		}
		return p.provider.GetPrompt(ctx, deps)
	})
}
