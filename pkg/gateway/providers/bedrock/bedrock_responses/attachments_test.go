package bedrock_responses

import (
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/constants"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/agent-sdk-go/pkg/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// attachedBlocks runs one native attachment through the full input conversion,
// so the tests cover the path a real request takes rather than the helper on
// its own.
func attachedBlocks(t *testing.T, content responses.InputContentUnion) []ContentBlock {
	t.Helper()
	msgs := nativeInputToConverseMessages(responses.InputUnion{
		OfInputMessageList: []responses.InputMessageUnion{{
			OfInputMessage: &responses.InputMessage{
				Role:    constants.RoleUser,
				Content: responses.InputContent{content},
			},
		}},
	})
	require.Len(t, msgs, 1)
	return msgs[0].Content
}

func TestImageAsDataURL(t *testing.T) {
	blocks := attachedBlocks(t, responses.InputContentUnion{
		OfInputImage: &responses.InputImageContent{
			ImageURL: utils.Ptr("data:image/jpeg;base64,aGVsbG8="),
		},
	})

	require.Len(t, blocks, 1)
	require.NotNil(t, blocks[0].Image)
	assert.Equal(t, "jpeg", blocks[0].Image.Format)
	assert.Equal(t, "aGVsbG8=", blocks[0].Image.Source.Bytes)
}

// Converse has no URL transport at all. That is a reason to say so, not a
// reason to send the turn as though nothing had been attached.
func TestImageAsHTTPSURLIsReported(t *testing.T) {
	blocks := attachedBlocks(t, responses.InputContentUnion{
		OfInputImage: &responses.InputImageContent{
			ImageURL: utils.Ptr("https://example.com/cat.png"),
		},
	})

	require.Len(t, blocks, 1)
	assert.Nil(t, blocks[0].Image)
	require.NotNil(t, blocks[0].Text, "the image is undeliverable here, and the model is told so")
	assert.Contains(t, *blocks[0].Text, "could not be sent")
}

// ImageURL is nil whenever the caller chose the file-id transport, which used
// to be dereferenced unguarded — Bedrock guarded it and dropped instead.
func TestImageByFileIDIsReported(t *testing.T) {
	blocks := attachedBlocks(t, responses.InputContentUnion{
		OfInputImage: &responses.InputImageContent{FileID: utils.Ptr("file_abc")},
	})

	require.Len(t, blocks, 1)
	require.NotNil(t, blocks[0].Text)
	assert.Contains(t, *blocks[0].Text, "could not be sent")
}

func TestFileAsDocument(t *testing.T) {
	blocks := attachedBlocks(t, responses.InputContentUnion{
		OfInputFile: &responses.InputFileContent{
			FileName: utils.Ptr("report.pdf"),
			FileData: utils.Ptr("data:application/pdf;base64,JVBERi0="),
		},
	})

	require.Len(t, blocks, 1)
	require.NotNil(t, blocks[0].Document, "a PDF used to be ignored entirely")
	assert.Equal(t, "pdf", blocks[0].Document.Format)
	assert.Equal(t, "report", blocks[0].Document.Name, "the extension is both redundant and illegal here")
	assert.Equal(t, "JVBERi0=", blocks[0].Document.Source.Bytes)
}

// The name Converse rejects is the ordinary one: a person uploads 123.csv and
// the dot fails the request, with nothing about the document itself at fault.
func TestDocumentNameDropsTheExtension(t *testing.T) {
	blocks := attachedBlocks(t, responses.InputContentUnion{
		OfInputFile: &responses.InputFileContent{
			FileName: utils.Ptr("123.csv"),
			FileData: utils.Ptr("data:text/csv;base64,YSxiCg=="),
		},
	})

	require.Len(t, blocks, 1)
	require.NotNil(t, blocks[0].Document)
	assert.Equal(t, "csv", blocks[0].Document.Format, "the extension still decides the format")
	assert.Equal(t, "123", blocks[0].Document.Name)
}

// Underscores arrive by way of the attachment store's own collision suffixes
// (report_2.json), so this is not only what a person happens to type.
func TestDocumentNameReplacesIllegalRunsWithOneSpace(t *testing.T) {
	for _, tc := range []struct{ filename, want string }{
		{"q3_sales.xlsx", "q3 sales"},
		{"report_2.json", "report 2"},
		{"a..b__c.txt", "a b c"},
		{"notes (final) [v2].md", "notes (final) [v2]"},
		{"my  spaced  name.txt", "my spaced name"},
		{"_leading and trailing_.txt", "leading and trailing"},
	} {
		blocks := attachedBlocks(t, responses.InputContentUnion{
			OfInputFile: &responses.InputFileContent{
				FileName: utils.Ptr(tc.filename),
				FileData: utils.Ptr("aGVsbG8="),
			},
		})

		require.Len(t, blocks, 1)
		require.NotNil(t, blocks[0].Document, tc.filename)
		assert.Equal(t, tc.want, blocks[0].Document.Name, tc.filename)
	}
}

// Nothing legal survives a name written in another script, and an empty name
// is its own 400. Losing the name is the cheaper failure.
func TestDocumentNameFallsBackWhenNothingSurvives(t *testing.T) {
	for _, filename := range []string{"\u6587\u4ef6.csv", ".hidden", "___.txt"} {
		blocks := attachedBlocks(t, responses.InputContentUnion{
			OfInputFile: &responses.InputFileContent{
				FileName: utils.Ptr(filename),
				FileData: utils.Ptr("aGVsbG8="),
			},
		})

		require.Len(t, blocks, 1)
		require.NotNil(t, blocks[0].Document, filename)
		assert.Equal(t, "document", blocks[0].Document.Name, filename)
	}
}

// Converse names formats in its own vocabulary — a bare word, not a media type.
func TestDocumentFormatComesFromTheName(t *testing.T) {
	blocks := attachedBlocks(t, responses.InputContentUnion{
		OfInputFile: &responses.InputFileContent{
			FileName: utils.Ptr("sales.csv"),
			FileData: utils.Ptr("aGVsbG8="),
		},
	})

	require.Len(t, blocks, 1)
	require.NotNil(t, blocks[0].Document)
	assert.Equal(t, "csv", blocks[0].Document.Format)
}

func TestFileByURLIsReported(t *testing.T) {
	blocks := attachedBlocks(t, responses.InputContentUnion{
		OfInputFile: &responses.InputFileContent{FileURL: utils.Ptr("https://example.com/a.pdf")},
	})

	require.Len(t, blocks, 1)
	require.NotNil(t, blocks[0].Text)
	assert.Contains(t, *blocks[0].Text, "could not be sent")
}

// A tool result is how an image reaches the model with nobody having typed a
// message — an MCP tool returning a screenshot. Only text used to survive, and
// Converse's toolResult takes the same blocks a message does.
func TestToolResultCarriesAnImage(t *testing.T) {
	msgs := nativeInputToConverseMessages(responses.InputUnion{
		OfInputMessageList: []responses.InputMessageUnion{{
			OfFunctionCallOutput: &responses.FunctionCallOutputMessage{
				CallID: "call_1",
				Output: responses.FunctionCallOutputContentUnion{
					OfList: responses.InputContent{
						{OfInputText: &responses.InputTextContent{Text: "here it is"}},
						{OfInputImage: &responses.InputImageContent{
							ImageURL: utils.Ptr("data:image/png;base64,aGVsbG8="),
						}},
					},
				},
			},
		}},
	})

	require.Len(t, msgs, 1)
	require.Len(t, msgs[0].Content, 1)
	result := msgs[0].Content[0].ToolResult
	require.NotNil(t, result)
	require.Len(t, result.Content, 2)
	require.NotNil(t, result.Content[0].Text)
	require.NotNil(t, result.Content[1].Image, "the screenshot the tool returned used to be dropped")
	assert.Equal(t, "aGVsbG8=", result.Content[1].Image.Source.Bytes)
}
