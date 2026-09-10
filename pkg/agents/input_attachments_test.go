package agents

import (
	"github.com/hastekit/agent-sdk-go/pkg/agents/history"
	"github.com/hastekit/agent-sdk-go/pkg/attachments"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/constants"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/agent-sdk-go/pkg/utils"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestPublishAttachmentOnlyInput(t *testing.T) {
	ref := &attachments.Ref{ID: "owned", Version: "immutable"}
	var chunks []*responses.ResponseChunk
	publishInputMessages(func(c *responses.ResponseChunk) { chunks = append(chunks, c) }, history.Message{Messages: []responses.InputMessageUnion{{OfInputMessage: &responses.InputMessage{ID: "msg_file", Role: constants.RoleUser, Content: responses.InputContent{{OfInputImage: &responses.InputImageContent{FileID: utils.Ptr(attachments.FileID(*ref))}}}}}}})
	require.Len(t, chunks, 1)
	require.Equal(t, "msg_file", chunks[0].OfInputMessage.MessageID)
	require.Equal(t, utils.Ptr(attachments.FileID(*ref)), chunks[0].OfInputMessage.ContentParts[0].OfInputImage.FileID)
}
