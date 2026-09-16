package agui

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	"github.com/hastekit/agent-sdk-go/pkg/agents/history"
)

const defaultMessagePageLimit = 50
const maxMessagePageLimit = 200

type messageCursor struct {
	Version   int    `json:"v"`
	Namespace string `json:"ns"`
	Thread    string `json:"thread"`
	Before    string `json:"before"`
}

func messagePageOptions(r *http.Request, namespace, thread string) (history.TranscriptPageOptions, error) {
	opts := history.TranscriptPageOptions{Limit: defaultMessagePageLimit}
	query := r.URL.Query()
	if values, ok := query["limit"]; ok {
		if len(values) != 1 {
			return opts, fmt.Errorf("provide one limit")
		}
		n, err := strconv.Atoi(values[0])
		if err != nil || n < 1 || n > maxMessagePageLimit {
			return opts, fmt.Errorf("limit must be between 1 and %d", maxMessagePageLimit)
		}
		opts.Limit = n
	}
	if values, ok := query["cursor"]; ok {
		if len(values) != 1 || len(values[0]) > 8192 {
			return opts, history.ErrInvalidTranscriptCursor
		}
		data, err := base64.RawURLEncoding.DecodeString(values[0])
		var cursor messageCursor
		if err != nil || json.Unmarshal(data, &cursor) != nil || cursor.Version != 1 || cursor.Namespace != namespace || cursor.Thread != thread || cursor.Before == "" {
			return opts, history.ErrInvalidTranscriptCursor
		}
		opts.BeforeRunID = cursor.Before
	}
	return opts, nil
}

func nextMessageCursor(namespace, thread, before string) string {
	if before == "" {
		return ""
	}
	data, _ := json.Marshal(messageCursor{Version: 1, Namespace: namespace, Thread: thread, Before: before})
	return base64.RawURLEncoding.EncodeToString(data)
}
