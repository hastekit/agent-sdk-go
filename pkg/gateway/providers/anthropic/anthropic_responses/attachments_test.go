package anthropic_responses

import (
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/constants"
	responses2 "github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/agent-sdk-go/pkg/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// attachedContents runs one native attachment through the full input
// conversion, so the tests cover the path a real request takes rather than the
// helper on its own.
func attachedContents(t *testing.T, content responses2.InputContentUnion) Contents {
	t.Helper()
	msgs := NativeMessagesToMessage(responses2.InputUnion{
		OfInputMessageList: []responses2.InputMessageUnion{{
			OfInputMessage: &responses2.InputMessage{
				Role:    constants.RoleUser,
				Content: responses2.InputContent{content},
			},
		}},
	})
	require.Len(t, msgs, 1)
	return msgs[0].Content.OfList
}

func TestImageAsDataURL(t *testing.T) {
	contents := attachedContents(t, responses2.InputContentUnion{
		OfInputImage: &responses2.InputImageContent{
			ImageURL: utils.Ptr("data:image/png;base64,aGVsbG8="),
		},
	})

	require.Len(t, contents, 1)
	require.NotNil(t, contents[0].OfImage)
	assert.Equal(t, "base64", contents[0].OfImage.Source.Type)
	assert.Equal(t, "aGVsbG8=", *contents[0].OfImage.Source.Data)
	assert.Equal(t, "image/png", *contents[0].OfImage.Source.MediaType)
}

// Anthropic fetches an https URL itself. This used to fall through the data:
// check and be dropped, so the model answered about a picture it never saw.
func TestImageAsHTTPSURL(t *testing.T) {
	contents := attachedContents(t, responses2.InputContentUnion{
		OfInputImage: &responses2.InputImageContent{
			ImageURL: utils.Ptr("https://example.com/cat.png"),
		},
	})

	require.Len(t, contents, 1)
	require.NotNil(t, contents[0].OfImage, "an https URL is a transport Anthropic has, not a reason to drop the image")
	assert.Equal(t, "url", contents[0].OfImage.Source.Type)
	assert.Equal(t, "https://example.com/cat.png", *contents[0].OfImage.Source.URL)
}

// ImageURL is nil whenever the caller chose the file-id transport, which used
// to be dereferenced unguarded and panic.
func TestImageByFileID(t *testing.T) {
	contents := attachedContents(t, responses2.InputContentUnion{
		OfInputImage: &responses2.InputImageContent{FileID: utils.Ptr("file_abc")},
	})

	require.Len(t, contents, 1)
	require.NotNil(t, contents[0].OfImage)
	assert.Equal(t, "file", contents[0].OfImage.Source.Type)
	assert.Equal(t, "file_abc", *contents[0].OfImage.Source.FileID)
}

func TestFileAsDocument(t *testing.T) {
	contents := attachedContents(t, responses2.InputContentUnion{
		OfInputFile: &responses2.InputFileContent{
			FileName: utils.Ptr("report.pdf"),
			FileData: utils.Ptr("data:application/pdf;base64,JVBERi0="),
		},
	})

	require.Len(t, contents, 1)
	require.NotNil(t, contents[0].OfDocument, "a PDF used to be ignored entirely")
	assert.Equal(t, "base64", contents[0].OfDocument.Source.Type)
	assert.Equal(t, "JVBERi0=", *contents[0].OfDocument.Source.Data)
	assert.Equal(t, "application/pdf", *contents[0].OfDocument.Source.MediaType)
	assert.Equal(t, "report.pdf", *contents[0].OfDocument.Title)
}

func TestFileByURLAndFileID(t *testing.T) {
	byURL := attachedContents(t, responses2.InputContentUnion{
		OfInputFile: &responses2.InputFileContent{FileURL: utils.Ptr("https://example.com/a.pdf")},
	})
	require.Len(t, byURL, 1)
	require.NotNil(t, byURL[0].OfDocument)
	assert.Equal(t, "url", byURL[0].OfDocument.Source.Type)

	byID := attachedContents(t, responses2.InputContentUnion{
		OfInputFile: &responses2.InputFileContent{FileID: utils.Ptr("file_xyz")},
	})
	require.Len(t, byID, 1)
	require.NotNil(t, byID[0].OfDocument)
	assert.Equal(t, "file", byID[0].OfDocument.Source.Type)
	assert.Equal(t, "file_xyz", *byID[0].OfDocument.Source.FileID)
}

// Bare base64 carries no media type of its own, so the filename supplies it.
func TestFileDataWithoutADataURL(t *testing.T) {
	contents := attachedContents(t, responses2.InputContentUnion{
		OfInputFile: &responses2.InputFileContent{
			FileName: utils.Ptr("notes.txt"),
			FileData: utils.Ptr("aGVsbG8="),
		},
	})

	require.Len(t, contents, 1)
	require.NotNil(t, contents[0].OfDocument)
	assert.Equal(t, "text/plain", *contents[0].OfDocument.Source.MediaType)
	assert.Equal(t, "aGVsbG8=", *contents[0].OfDocument.Source.Data)
}

// An attachment carrying nothing usable is said out loud rather than dropped:
// a model that is told it cannot see something says so.
func TestAnEmptyAttachmentBecomesAVisibleNote(t *testing.T) {
	contents := attachedContents(t, responses2.InputContentUnion{
		OfInputImage: &responses2.InputImageContent{},
	})

	require.Len(t, contents, 1)
	require.NotNil(t, contents[0].OfText)
	assert.Contains(t, contents[0].OfText.Text, "could not be sent")
}

// A url source that also carried "data": null and "media_type": null is a
// shape with no reason to be accepted.
func TestURLSourceOmitsTheBase64Fields(t *testing.T) {
	block := ContentUnion{OfImage: &ImageContent{
		Source: ImageContentSource{Type: "url", URL: utils.Ptr("https://example.com/cat.png")},
	}}
	data, err := block.MarshalJSON()
	require.NoError(t, err)
	assert.JSONEq(t, `{"type":"image","source":{"type":"url","url":"https://example.com/cat.png"}}`, string(data))
}

// nativeAgain sends one native attachment out to Anthropic's shape and back,
// which is the property that matters: the gateway accepts Anthropic-shaped
// requests as well as producing them, and a transport the forward direction
// learned to emit is one the reverse has to learn to read.
func nativeAgain(t *testing.T, content responses2.InputContentUnion) responses2.InputContentUnion {
	t.Helper()
	contents := attachedContents(t, content)
	require.Len(t, contents, 1)

	msgs := MessagesToNativeMessages([]MessageUnion{{
		Role:    RoleUser,
		Content: ContentUnionParam{OfList: contents},
	}})
	require.Len(t, msgs.OfInputMessageList, 1)
	native := msgs.OfInputMessageList[0].OfInputMessage
	require.NotNil(t, native)
	require.Len(t, native.Content, 1)
	return native.Content[0]
}

func TestImageRoundTrips(t *testing.T) {
	dataURL := nativeAgain(t, responses2.InputContentUnion{
		OfInputImage: &responses2.InputImageContent{ImageURL: utils.Ptr("data:image/png;base64,aGVsbG8=")},
	})
	require.NotNil(t, dataURL.OfInputImage)
	assert.Equal(t, "data:image/png;base64,aGVsbG8=", *dataURL.OfInputImage.ImageURL)

	url := nativeAgain(t, responses2.InputContentUnion{
		OfInputImage: &responses2.InputImageContent{ImageURL: utils.Ptr("https://example.com/cat.png")},
	})
	require.NotNil(t, url.OfInputImage, "a url source survived the way out; it has to survive the way back")
	assert.Equal(t, "https://example.com/cat.png", *url.OfInputImage.ImageURL)

	byID := nativeAgain(t, responses2.InputContentUnion{
		OfInputImage: &responses2.InputImageContent{FileID: utils.Ptr("file_abc")},
	})
	require.NotNil(t, byID.OfInputImage)
	require.NotNil(t, byID.OfInputImage.FileID)
	assert.Equal(t, "file_abc", *byID.OfInputImage.FileID)
}

func TestFileRoundTrips(t *testing.T) {
	inline := nativeAgain(t, responses2.InputContentUnion{
		OfInputFile: &responses2.InputFileContent{
			FileName: utils.Ptr("report.pdf"),
			FileData: utils.Ptr("data:application/pdf;base64,JVBERi0="),
		},
	})
	require.NotNil(t, inline.OfInputFile, "a document survived the way out; it has to survive the way back")
	assert.Equal(t, "data:application/pdf;base64,JVBERi0=", *inline.OfInputFile.FileData)
	assert.Equal(t, "report.pdf", *inline.OfInputFile.FileName)

	byID := nativeAgain(t, responses2.InputContentUnion{
		OfInputFile: &responses2.InputFileContent{FileID: utils.Ptr("file_xyz")},
	})
	require.NotNil(t, byID.OfInputFile)
	assert.Equal(t, "file_xyz", *byID.OfInputFile.FileID)
}

// A base64 source that names the transport but carries none of it used to be
// dereferenced on the way in.
func TestInboundBase64ImageWithNoDataIsReported(t *testing.T) {
	msgs := MessagesToNativeMessages([]MessageUnion{{
		Role: RoleUser,
		Content: ContentUnionParam{OfList: Contents{
			{OfImage: &ImageContent{Source: ImageContentSource{Type: "base64"}}},
		}},
	}})

	require.Len(t, msgs.OfInputMessageList, 1)
	content := msgs.OfInputMessageList[0].OfInputMessage.Content
	require.Len(t, content, 1)
	require.NotNil(t, content[0].OfInputText)
	assert.Contains(t, content[0].OfInputText.Text, "could not be")
}

// toolResult sends one native tool output through the conversion and hands
// back the blocks inside Anthropic's tool_result.
func toolResult(t *testing.T, out responses2.FunctionCallOutputContentUnion) Contents {
	t.Helper()
	msgs := NativeMessagesToMessage(responses2.InputUnion{
		OfInputMessageList: []responses2.InputMessageUnion{{
			OfFunctionCallOutput: &responses2.FunctionCallOutputMessage{CallID: "call_1", Output: out},
		}},
	})
	require.Len(t, msgs, 1)
	require.Len(t, msgs[0].Content.OfList, 1)
	require.NotNil(t, msgs[0].Content.OfList[0].OfToolResult)
	return msgs[0].Content.OfList[0].OfToolResult.Content.OfList
}

// A tool result is how an image reaches the model with nobody having typed a
// message — an MCP tool returning a screenshot. Only text used to survive.
func TestToolResultCarriesAnImage(t *testing.T) {
	blocks := toolResult(t, responses2.FunctionCallOutputContentUnion{
		OfList: responses2.InputContent{
			{OfInputText: &responses2.InputTextContent{Text: "here it is"}},
			{OfInputImage: &responses2.InputImageContent{ImageURL: utils.Ptr("data:image/png;base64,aGVsbG8=")}},
		},
	})

	require.Len(t, blocks, 2)
	require.NotNil(t, blocks[0].OfText)
	assert.Equal(t, "here it is", blocks[0].OfText.Text)
	require.NotNil(t, blocks[1].OfImage, "the screenshot the tool returned used to be dropped")
	assert.Equal(t, "base64", blocks[1].OfImage.Source.Type)
}

func TestToolResultCarriesAFile(t *testing.T) {
	blocks := toolResult(t, responses2.FunctionCallOutputContentUnion{
		OfList: responses2.InputContent{
			{OfInputFile: &responses2.InputFileContent{
				FileName: utils.Ptr("out.pdf"),
				FileData: utils.Ptr("data:application/pdf;base64,JVBERi0="),
			}},
		},
	})

	require.Len(t, blocks, 1)
	require.NotNil(t, blocks[0].OfDocument)
	assert.Equal(t, "base64", blocks[0].OfDocument.Source.Type)
}

func TestToolResultRoundTrips(t *testing.T) {
	blocks := toolResult(t, responses2.FunctionCallOutputContentUnion{
		OfList: responses2.InputContent{
			{OfInputText: &responses2.InputTextContent{Text: "here it is"}},
			{OfInputImage: &responses2.InputImageContent{ImageURL: utils.Ptr("data:image/png;base64,aGVsbG8=")}},
		},
	})

	msgs := MessagesToNativeMessages([]MessageUnion{{
		Role: RoleUser,
		Content: ContentUnionParam{OfList: Contents{
			{OfToolResult: &ToolUseResultContent{ToolUseID: "call_1", Content: ContentUnionParam{OfList: blocks}}},
		}},
	}})

	require.Len(t, msgs.OfInputMessageList, 1)
	out := msgs.OfInputMessageList[0].OfFunctionCallOutput
	require.NotNil(t, out)
	require.Len(t, out.Output.OfList, 2)
	assert.Equal(t, "here it is", out.Output.OfList[0].OfInputText.Text)
	require.NotNil(t, out.Output.OfList[1].OfInputImage, "an image survived the way out; it has to survive the way back")
	assert.Equal(t, "data:image/png;base64,aGVsbG8=", *out.Output.OfList[1].OfInputImage.ImageURL)
}
