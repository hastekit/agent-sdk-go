package agents

import (
	"fmt"
	"github.com/hastekit/agent-sdk-go/pkg/attachments"
	"strings"

	"github.com/hastekit/agent-sdk-go/pkg/agents/history"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/constants"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
)

// publishInputMessages announces the turns a run has taken in.
//
// Without it the stream carries only the agent's half of the conversation, and
// a client that joins a run in flight — or rejoins one it dropped — replays a
// transcript of answers with the questions missing. The client that sent the
// turn already has it and treats the echo as an acknowledgement; every other
// client is hearing it for the first time.
//
// Called at the point the run takes the message in rather than when it
// arrives, so a turn sent mid-run lands between the two model calls it
// actually arrived between.
func publishInputMessages(publish func(*responses.ResponseChunk), bundles ...history.Message) {
	if publish == nil {
		return
	}
	for _, bundle := range bundles {
		// A background task's result is delivered as a user turn because that
		// is the only shape a provider takes it in, but nobody wrote it — the
		// same reason it is kept out of a rehydrated transcript.
		if bundle.BackgroundTaskID != "" {
			continue
		}
		for i, msg := range bundle.Messages {
			id, role, text := inputMessageText(msg)
			parts := inputMessageReferences(msg)
			if text == "" && len(parts) == 0 {
				continue
			}
			if id == "" {
				// Every message needs an id of its own: it is what lets the
				// client that sent this turn recognise its own echo, and what
				// keeps two turns from being folded into one. The bundle's id
				// and the position within it, rather than a fresh uuid, so a
				// workflow replaying this announces the same turn twice under
				// the same name instead of two.
				if bundle.ID == "" {
					continue
				}
				id = fmt.Sprintf("%s#%d", bundle.ID, i)
			}
			publish(&responses.ResponseChunk{
				OfInputMessage: &responses.ChunkInputMessage[constants.ChunkTypeInputMessage]{
					MessageID:    id,
					Role:         role,
					Content:      text,
					ContentParts: parts,
					SenderID:     bundle.SenderID,
				},
			})
		}
	}
}

// inputMessageText flattens an authored turn to id, role and text.
//
// Only the two arms a person's turn arrives in are read. Everything else in
// the union — a tool's output, an interrupt resolution, the model's own reply
// coming back round — is either not authored or already has its own place in
// the stream, and reporting it again would double it.
func inputMessageText(msg responses.InputMessageUnion) (id, role, text string) {
	switch {
	case msg.OfInputMessage != nil:
		m := msg.OfInputMessage
		return m.ID, string(m.Role), inputContentText(m.Content)
	case msg.OfEasyInput != nil:
		m := msg.OfEasyInput
		if m.Content.OfString != nil {
			return m.ID, string(m.Role), *m.Content.OfString
		}
		return m.ID, string(m.Role), inputContentText(m.Content.OfInputMessageList)
	}
	return "", "", ""
}

func inputContentText(content responses.InputContent) string {
	parts := make([]string, 0, len(content))
	for _, c := range content {
		if c.OfInputText != nil && c.OfInputText.Text != "" {
			parts = append(parts, c.OfInputText.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// Announce only owned references, never inline bytes or provider URLs.
func inputMessageReferences(msg responses.InputMessageUnion) responses.InputContent {
	var content responses.InputContent
	if msg.OfInputMessage != nil {
		content = msg.OfInputMessage.Content
	}
	if msg.OfEasyInput != nil {
		content = msg.OfEasyInput.Content.OfInputMessageList
	}
	var out responses.InputContent
	hasRef := false
	for _, c := range content {
		switch {
		case c.OfInputText != nil:
			out = append(out, responses.InputContentUnion{OfInputText: c.OfInputText})
		case c.OfInputImage != nil && c.OfInputImage.FileID != nil && attachments.IsFileID(*c.OfInputImage.FileID):
			hasRef = true
			out = append(out, responses.InputContentUnion{OfInputImage: &responses.InputImageContent{FileID: c.OfInputImage.FileID, Detail: c.OfInputImage.Detail}})
		case c.OfInputFile != nil && c.OfInputFile.FileID != nil && attachments.IsFileID(*c.OfInputFile.FileID):
			hasRef = true
			out = append(out, responses.InputContentUnion{OfInputFile: &responses.InputFileContent{FileID: c.OfInputFile.FileID, FileName: c.OfInputFile.FileName}})
		}
	}
	if !hasRef {
		return nil
	}
	return out
}
