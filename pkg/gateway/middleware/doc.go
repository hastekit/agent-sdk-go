// Package middleware holds the gateway's built-in middleware: tracing, retry,
// and provider fallback.
//
// The gateway.Middleware interface and the handler types it composes stay in
// package gateway, which is what lets this package depend on gateway rather
// than the other way round. Nothing here is installed on its own —
// sdk.NewLLMClient decides what a client calls through, and callers composing
// a gateway by hand pass these to LLMGateway.UseMiddleware.
//
// Order is outermost first, and it matters:
//
//	gw.UseMiddleware(
//	    middleware.NewFallback(store, fallbackCfg), // changes provider
//	    middleware.NewRetry(retryCfg),              // retries within one provider
//	    middleware.NewTracing(),                    // one span per attempt
//	)
//
// Fallback wraps retry so a provider is retried on its own before the chain
// gives up on it, and both wrap tracing so every attempt gets its own span
// rather than collapsing into one slow one.
//
// Retry and Fallback share one rule about streams, in pumpStream: an attempt
// that fails before delivering a chunk can be replaced invisibly, and one
// that has already delivered anything cannot.
package middleware
