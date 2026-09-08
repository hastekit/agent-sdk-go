package middleware

import (
	"context"

	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
)

// nextStreamFn supplies the stream for another attempt after prev failed.
//
// Returning ok=false ends the sequence, and prev's failure is what the caller
// sees. Returning a non-nil errChunk ends it with that failure instead —
// which is how an attempt that could not even be started is reported.
type nextStreamFn func(ctx context.Context, prev *responses.StreamError) (
	stream <-chan *responses.ResponseChunk,
	errChunk *responses.ResponseChunk,
	ok bool,
)

// pumpStream forwards stream to out, replacing it via nextStream for as long
// as an attempt fails before delivering anything.
//
// The commit rule is the whole point: a stream that errors on its very first
// chunk delivered nothing, so abandoning it is invisible to the caller and
// another attempt is free to take its place. Once any chunk has been
// forwarded the attempt is committed — there is no way to un-send it, and no
// provider supports resuming a stream from the middle — so every later
// failure is passed straight through.
//
// An abandoned attempt is always drained. Its producer is blocked on an
// unbuffered send, and dropping the channel on the floor would leak that
// goroutine for the life of the process.
func pumpStream(
	ctx context.Context,
	out chan<- *responses.ResponseChunk,
	stream <-chan *responses.ResponseChunk,
	nextStream nextStreamFn,
) {
	defer close(out)

	// last is the failure the most recent attempt reported, kept so that a
	// replacement attempt which ends up saying nothing at all still leaves
	// the caller with a reason.
	var last *responses.ResponseChunk

	for {
		first, ok := <-stream
		if !ok {
			if last != nil {
				sendChunk(ctx, out, last)
			}
			return
		}

		if first.OfError == nil {
			if !sendChunk(ctx, out, first) {
				go drainChunks(stream)
				return
			}
			for chunk := range stream {
				if !sendChunk(ctx, out, chunk) {
					go drainChunks(stream)
					return
				}
			}
			return
		}

		go drainChunks(stream)
		last = first

		next, errChunk, ok := nextStream(ctx, first.OfError)
		switch {
		case errChunk != nil:
			sendChunk(ctx, out, errChunk)
			return
		case !ok:
			sendChunk(ctx, out, first)
			return
		}
		stream = next
	}
}

// sendChunk reports whether the chunk was delivered; false means the caller
// is gone and the stream should be abandoned.
func sendChunk(ctx context.Context, out chan<- *responses.ResponseChunk, chunk *responses.ResponseChunk) bool {
	select {
	case <-ctx.Done():
		return false
	case out <- chunk:
		return true
	}
}

func drainChunks(ch <-chan *responses.ResponseChunk) {
	for range ch {
	}
}
