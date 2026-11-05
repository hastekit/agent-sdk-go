package docker

import (
	"context"
	"fmt"
	"time"

	"github.com/hastekit/agent-sdk-go/pkg/agents/sandbox/internal/execsandbox"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/client"
)

// exitedWithin bounds the wait for an exit code once the stream has ended.
const exitedWithin = 10 * time.Second

// runner runs processes through the Engine's exec API. The Engine closes a
// process's stdin when its connection closes, however that happened.
type runner struct {
	engine Engine
	id     string
}

func (r *runner) Run(ctx context.Context, req execsandbox.Request) (int, error) {
	created, err := r.engine.ExecCreate(ctx, r.id, client.ExecCreateOptions{AttachStdin: true, AttachStdout: true, AttachStderr: true, Cmd: req.Argv()})
	if err != nil {
		return 0, mapError(err)
	}
	attached, err := r.engine.ExecAttach(ctx, created.ID, client.ExecAttachOptions{})
	if err != nil {
		return 0, mapError(err)
	}
	defer attached.Close()
	stop := context.AfterFunc(ctx, attached.Close)
	defer stop()
	_, streamErr := stdcopy.StdCopy(req.Stdout, req.Stderr, attached.Reader)
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if streamErr != nil {
		return 0, streamErr
	}
	return r.exitCode(ctx, created.ID)
}

func (r *runner) exitCode(ctx context.Context, id string) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, exitedWithin)
	defer cancel()
	for {
		inspected, err := r.engine.ExecInspect(ctx, id, client.ExecInspectOptions{})
		if err != nil {
			return 0, mapError(err)
		}
		if !inspected.Running {
			return inspected.ExitCode, nil
		}
		select {
		case <-ctx.Done():
			return 0, fmt.Errorf("exec %s still running after its stream ended: %w", id, ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
}
