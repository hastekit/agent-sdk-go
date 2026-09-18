//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd

package routines

import (
	"errors"
	"os"
)

func lockJournal(*os.File) error {
	return errors.New("JSONL routines storage requires a supported Unix filesystem")
}
