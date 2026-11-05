package sandbox

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Files are read, written and removed by running commands, so these work on any
// Sandbox. Content crosses Exec as base64 because Exec carries text: arguments
// cannot hold NUL, and providers that pass through JSON rewrite invalid UTF-8.
//
// Paths are confined to the sandbox's default working directory, including through
// symlinks. That scopes these helpers, not the sandbox: a shell can go anywhere the
// container lets it. They need a POSIX sh with base64, cat, mkdir, mv, readlink, rm,
// rmdir, tr and wc in the image; sync is used when present.

const (
	// MaxReadFile is the largest file ReadFile returns. A command's output is capped
	// at 4 MiB per stream, and 3 MB of base64 fits in that with room for its size.
	MaxReadFile = 3_000_000

	// writeChunk bounds each command writing a file. A single argument cannot
	// exceed 128 KiB, and some providers pass a whole command line as one.
	writeChunk = 48 << 10

	exitFileNotFound = 3
	exitFileOutside  = 4
	exitFileTooLarge = 5
)

var (
	ErrFileNotFound = errors.New("file not found")
	ErrFileTooLarge = errors.New("file too large")
)

// fileScript is the start of every file operation. Its first argument is the path,
// and it lets a script check that a path stays inside the workspace once symlinks
// are followed.
const fileScript = `root=$(pwd -P) target=$1
case "$target" in /*) ;; *) target=$root/$target ;; esac
inside() {
	real=$(readlink -f -- "$1") || return 1
	case "$real/" in "${root%/}"/*) return 0 ;; esac
	return 1
}
parent=${target%/*}
parent=${parent:-/}
`

// readFileScript prints the file's size, so that a read cut short cannot pass for a
// shorter file. A pipeline only reports its last command, so a missing base64 has
// to be caught first.
const readFileScript = fileScript + `command -v base64 >/dev/null || { echo "base64 is required" >&2; exit 1; }
[ -e "$target" ] || exit 3
inside "$target" || exit 4
[ -f "$target" ] || { echo "not a regular file" >&2; exit 1; }
size=$(wc -c <"$target") || exit 1
[ $size -le "$2" ] || exit 5
exec 4<"$target" || exit 1
printf '%d:' $size
base64 <&4 | tr -d '\n'
`

// writeFileScript appends a chunk to a temporary file beside the target, and renames
// it over the target with the last chunk, so that the target is only ever replaced
// whole. It checks the deepest directory that exists, so that mkdir cannot create
// anything through a symlink leading out of the workspace.
const writeFileScript = fileScript + `existing=$parent
while [ ! -e "$existing" ] && [ ! -L "$existing" ]; do
	existing=${existing%/*}
	existing=${existing:-/}
done
inside "$existing" || exit 4
mkdir -p -- "$parent" || exit 1
[ ! -d "$target" ] || { echo "target is a directory" >&2; exit 1; }
tmp=$parent/$2
umask 077
printf %s "$4" | base64 -d >>"$tmp" || exit 1
[ "$3" = last ] || exit 0
sync -- "$tmp" 2>/dev/null
exec mv -f -- "$tmp" "$target"
`

const discardFileScript = fileScript + `exec rm -f -- "$parent/$2"
`

const removeFileScript = fileScript + `[ -e "$parent" ] || exit 3
inside "$parent" || exit 4
[ -e "$target" ] || [ -L "$target" ] || exit 3
if [ -d "$target" ] && [ ! -L "$target" ]; then
	exec rmdir -- "$target"
fi
exec rm -f -- "$target"
`

func runFileScript(ctx context.Context, sb Sandbox, name, script string, args ...string) (*ExecResult, error) {
	res, err := sb.Exec(ctx, ExecRequest{Argv: append([]string{"sh", "-c", script, "sandbox"}, args...)})
	if err != nil {
		return nil, err
	}
	switch res.ExitCode {
	case 0:
		return res, nil
	case exitFileNotFound:
		return nil, fmt.Errorf("%w: %s", ErrFileNotFound, name)
	case exitFileOutside:
		return nil, fmt.Errorf("path %q is outside the sandbox workspace", name)
	case exitFileTooLarge:
		return nil, fmt.Errorf("%w: %s", ErrFileTooLarge, name)
	}
	return nil, fmt.Errorf("file operation on %q failed with exit code %d: %s", name, res.ExitCode, strings.TrimSpace(res.Stderr+res.Stdout))
}

func cleanFilePath(name string) (string, error) {
	if strings.TrimSpace(name) == "" {
		return "", fmt.Errorf("path is required")
	}
	cleaned := path.Clean(name)
	if cleaned == "." {
		return "", fmt.Errorf("a file path is required")
	}
	return cleaned, nil
}

// ReadFile returns a file's content. A file over MaxReadFile is ErrFileTooLarge.
func ReadFile(ctx context.Context, sb Sandbox, name string) ([]byte, error) {
	target, err := cleanFilePath(name)
	if err != nil {
		return nil, err
	}
	res, err := runFileScript(ctx, sb, name, readFileScript, target, strconv.Itoa(MaxReadFile))
	if err != nil {
		return nil, err
	}
	if res.OutputTruncated {
		return nil, fmt.Errorf("%w: %s", ErrFileTooLarge, name)
	}
	size, encoded, ok := strings.Cut(strings.TrimSpace(res.Stdout), ":")
	if !ok {
		return nil, fmt.Errorf("reading %q: unexpected output", name)
	}
	content, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("reading %q: %w", name, err)
	}
	if strconv.Itoa(len(content)) != size {
		return nil, fmt.Errorf("reading %q: got %d of %s bytes", name, len(content), size)
	}
	return content, nil
}

// WriteFile creates or replaces a file, creating its parents. The file changes only
// once all of the content has arrived. It takes one command per 48 KiB, so large
// files are better mounted, or fetched from inside the sandbox.
func WriteFile(ctx context.Context, sb Sandbox, name string, content []byte) error {
	target, err := cleanFilePath(name)
	if err != nil {
		return err
	}
	tmp := ".upload-" + uuid.NewString()
	for {
		chunk, state := content, "last"
		if len(chunk) > writeChunk {
			chunk, state = chunk[:writeChunk], "more"
		}
		if _, err := runFileScript(ctx, sb, name, writeFileScript, target, tmp, state, base64.StdEncoding.EncodeToString(chunk)); err != nil {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			_, _ = runFileScript(cleanup, sb, name, discardFileScript, target, tmp)
			return err
		}
		if state == "last" {
			return nil
		}
		content = content[len(chunk):]
	}
}

// RemoveFile removes a file or an empty directory.
func RemoveFile(ctx context.Context, sb Sandbox, name string) error {
	target, err := cleanFilePath(name)
	if err != nil {
		return err
	}
	_, err = runFileScript(ctx, sb, name, removeFileScript, target)
	return err
}
