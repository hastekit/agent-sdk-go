package attachments

import (
	"fmt"
	"github.com/google/uuid"
	"path"
	"strings"
	"unicode"
	"unicode/utf8"
)

// originalFilename strips client-supplied directory components on all platforms.
func originalFilename(name string) string {
	return path.Base(strings.ReplaceAll(name, `\`, "/"))
}

func cleanFilename(name string) string {
	name = strings.Map(func(r rune) rune {
		switch {
		case unicode.IsSpace(r):
			return ' '
		case unicode.IsLetter(r), unicode.IsNumber(r), strings.ContainsRune(" ._-", r):
			return r
		default:
			return '_'
		}
	}, originalFilename(name))
	name = strings.Trim(name, " .")
	if name == "" {
		name = "attachment"
	}
	if strings.HasPrefix(name, "-") {
		name = "attachment_" + name
	}
	return name
}

// attachmentFilename produces a readable, bounded, single path component.
func attachmentFilename(name string) string {
	name = cleanFilename(name)
	ext := path.Ext(name)
	if len(ext) > 32 {
		ext = ""
	}
	return strings.TrimRight(truncateFilename(strings.TrimSuffix(name, ext), 180-len(ext)), " .") + ext
}

func truncateFilename(name string, limit int) string {
	if len(name) <= limit {
		return name
	}
	name = name[:limit]
	for !utf8.ValidString(name) {
		name = name[:len(name)-1]
	}
	return name
}

func numberedFilename(name string, number int) string {
	if number == 1 {
		return name
	}
	ext := path.Ext(name)
	suffix := fmt.Sprintf("_%d", number)
	return truncateFilename(strings.TrimSuffix(name, ext), 200-len(ext)-len(suffix)) + suffix + ext
}

func validFilename(id string) bool {
	return id != "" && len(id) <= 200 && utf8.ValidString(id) && id == cleanFilename(id)
}

func validID(id string) bool {
	parsed, err := uuid.Parse(id)
	return err == nil && parsed.String() == id
}

func validateMountPath(mountPath string) error {
	if mountPath != "" && (!path.IsAbs(mountPath) || strings.ContainsAny(mountPath, "\\\x00")) {
		return ErrInvalid
	}
	return nil
}

func mountedPath(mountPath, filename string) string {
	if mountPath == "" {
		return ""
	}
	return path.Join(mountPath, filename)
}

// ParseRef accepts attachment:// IDs (or bare UUIDs) supplied to tools.
// Namespace and session scope must come from the trusted tool call.
func ParseRef(value string) (Ref, error) {
	if IsFileID(value) {
		return RefFromFileID(value)
	}
	if !validID(value) {
		return Ref{}, ErrInvalid
	}
	return Ref{ID: value}, nil
}
