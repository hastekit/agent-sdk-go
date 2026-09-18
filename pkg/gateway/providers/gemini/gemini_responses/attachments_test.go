package gemini_responses

import (
	"testing"

	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/constants"
	responses2 "github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/agent-sdk-go/pkg/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// attachedParts runs one native attachment through the full input conversion,
// so the tests cover the path a real request takes rather than the helper on
// its own.
func attachedParts(t *testing.T, content responses2.InputContentUnion) []Part {
	t.Helper()
	contents := NativeMessagesToMessages(responses2.InputUnion{
		OfInputMessageList: []responses2.InputMessageUnion{{
			OfInputMessage: &responses2.InputMessage{
				Role:    constants.RoleUser,
				Content: responses2.InputContent{content},
			},
		}},
	})
	require.Len(t, contents, 1)
	return contents[0].Parts
}

func TestImageAsDataURL(t *testing.T) {
	parts := attachedParts(t, responses2.InputContentUnion{
		OfInputImage: &responses2.InputImageContent{
			ImageURL: utils.Ptr("data:image/png;base64,aGVsbG8="),
		},
	})

	require.Len(t, parts, 1)
	require.NotNil(t, parts[0].InlineData)
	assert.Equal(t, "image/png", parts[0].InlineData.MimeType)
	assert.Equal(t, "aGVsbG8=", parts[0].InlineData.Data)
}

// Gemini will not fetch an arbitrary URL, but it does take a URI for bytes it
// already holds — and passing it through gets Gemini's own verdict, which is
// better than the silent drop this replaced.
func TestImageAsHTTPSURL(t *testing.T) {
	parts := attachedParts(t, responses2.InputContentUnion{
		OfInputImage: &responses2.InputImageContent{
			ImageURL: utils.Ptr("https://generativelanguage.googleapis.com/v1beta/files/abc123"),
		},
	})

	require.Len(t, parts, 1)
	require.NotNil(t, parts[0].FileData, "a URL is passed on as a fileUri, not dropped")
	assert.Equal(t, "https://generativelanguage.googleapis.com/v1beta/files/abc123", parts[0].FileData.FileURI)
}

// ImageURL is nil whenever the caller chose the file-id transport, which used
// to be dereferenced unguarded and panic.
func TestImageByFileID(t *testing.T) {
	parts := attachedParts(t, responses2.InputContentUnion{
		OfInputImage: &responses2.InputImageContent{
			FileID: utils.Ptr("https://generativelanguage.googleapis.com/v1beta/files/xyz"),
		},
	})

	require.Len(t, parts, 1)
	require.NotNil(t, parts[0].FileData)
	assert.Equal(t, "https://generativelanguage.googleapis.com/v1beta/files/xyz", parts[0].FileData.FileURI)
}

func TestFileAsInlinePDF(t *testing.T) {
	parts := attachedParts(t, responses2.InputContentUnion{
		OfInputFile: &responses2.InputFileContent{
			FileName: utils.Ptr("report.pdf"),
			FileData: utils.Ptr("data:application/pdf;base64,JVBERi0="),
		},
	})

	require.Len(t, parts, 1)
	require.NotNil(t, parts[0].InlineData, "a PDF used to be ignored entirely")
	assert.Equal(t, "application/pdf", parts[0].InlineData.MimeType)
	assert.Equal(t, "JVBERi0=", parts[0].InlineData.Data)
}

func TestFileByURI(t *testing.T) {
	parts := attachedParts(t, responses2.InputContentUnion{
		OfInputFile: &responses2.InputFileContent{
			FileName: utils.Ptr("report.pdf"),
			FileURL:  utils.Ptr("gs://bucket/report.pdf"),
		},
	})

	require.Len(t, parts, 1)
	require.NotNil(t, parts[0].FileData)
	assert.Equal(t, "gs://bucket/report.pdf", parts[0].FileData.FileURI)
	assert.Equal(t, "application/pdf", parts[0].FileData.MimeType)
}

// An attachment carrying nothing usable is said out loud rather than dropped.
func TestAnEmptyAttachmentBecomesAVisibleNote(t *testing.T) {
	parts := attachedParts(t, responses2.InputContentUnion{
		OfInputImage: &responses2.InputImageContent{},
	})

	require.Len(t, parts, 1)
	require.NotNil(t, parts[0].Text)
	assert.Contains(t, *parts[0].Text, "could not be sent")
}

// nativeAgain sends one native attachment out to Gemini's shape and back,
// which is the property that matters: the gateway accepts Gemini-shaped
// requests as well as producing them, and a transport the forward direction
// learned to emit is one the reverse has to learn to read.
func nativeAgain(t *testing.T, content responses2.InputContentUnion) responses2.InputContentUnion {
	t.Helper()
	parts := attachedParts(t, content)
	require.Len(t, parts, 1)

	msgs := MessagesToNativeMessages([]Content{{Role: RoleUser, Parts: parts}})
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

	// Gemini has one field where the native shape has two, so a URL comes back
	// as a file id. The URI is unchanged, which is what reaches Gemini either
	// way; only which native field holds it differs.
	byID := nativeAgain(t, responses2.InputContentUnion{
		OfInputImage: &responses2.InputImageContent{FileID: utils.Ptr("files/abc123")},
	})
	require.NotNil(t, byID.OfInputImage, "a fileUri survived the way out; it has to survive the way back")
	require.NotNil(t, byID.OfInputImage.FileID)
	assert.Equal(t, "files/abc123", *byID.OfInputImage.FileID)
}

func TestFileRoundTrips(t *testing.T) {
	inline := nativeAgain(t, responses2.InputContentUnion{
		OfInputFile: &responses2.InputFileContent{
			FileName: utils.Ptr("report.pdf"),
			FileData: utils.Ptr("data:application/pdf;base64,JVBERi0="),
		},
	})
	require.NotNil(t, inline.OfInputFile, "a PDF used to be dropped on the way in, not just on the way out")
	assert.Equal(t, "data:application/pdf;base64,JVBERi0=", *inline.OfInputFile.FileData)

	byURI := nativeAgain(t, responses2.InputContentUnion{
		OfInputFile: &responses2.InputFileContent{
			FileName: utils.Ptr("report.pdf"),
			FileURL:  utils.Ptr("gs://bucket/report.pdf"),
		},
	})
	require.NotNil(t, byURI.OfInputFile)
	assert.Equal(t, "gs://bucket/report.pdf", *byURI.OfInputFile.FileID)
}

// An inbound PDF sent inline used to be dropped by the mime-prefix check,
// which only let images through.
func TestInboundInlinePDFBecomesAFile(t *testing.T) {
	msgs := MessagesToNativeMessages([]Content{{
		Role: RoleUser,
		Parts: []Part{{InlineData: &InlinePartData{
			MimeType: "application/pdf", Data: "JVBERi0=",
		}}},
	}})

	require.Len(t, msgs.OfInputMessageList, 1)
	content := msgs.OfInputMessageList[0].OfInputMessage.Content
	require.Len(t, content, 1)
	require.NotNil(t, content[0].OfInputFile)
	assert.Equal(t, "data:application/pdf;base64,JVBERi0=", *content[0].OfInputFile.FileData)
}

// toolResponse sends one native tool output through the conversion and hands
// back Gemini's functionResponse.
func toolResponse(t *testing.T, out responses2.FunctionCallOutputContentUnion) *FunctionResponse {
	t.Helper()
	contents := NativeMessagesToMessages(responses2.InputUnion{
		OfInputMessageList: []responses2.InputMessageUnion{{
			OfFunctionCallOutput: &responses2.FunctionCallOutputMessage{CallID: "call_1", Output: out},
		}},
	})
	require.Len(t, contents, 1)
	require.Len(t, contents[0].Parts, 1, "one response answers one call")
	require.NotNil(t, contents[0].Parts[0].FunctionResponse)
	return contents[0].Parts[0].FunctionResponse
}

// Gemini's response field is JSON and has nowhere to put an image, so the
// bytes go in parts alongside it. They used to go nowhere.
func TestToolResultCarriesAnImage(t *testing.T) {
	fr := toolResponse(t, responses2.FunctionCallOutputContentUnion{
		OfList: responses2.InputContent{
			{OfInputText: &responses2.InputTextContent{Text: "here it is"}},
			{OfInputImage: &responses2.InputImageContent{ImageURL: utils.Ptr("data:image/png;base64,aGVsbG8=")}},
		},
	})

	assert.Equal(t, "here it is", fr.Response["output"])
	require.Len(t, fr.Parts, 1, "the screenshot the tool returned used to be dropped")
	require.NotNil(t, fr.Parts[0].InlineData)
	assert.Equal(t, "image/png", fr.Parts[0].InlineData.MimeType)
	assert.Equal(t, "aGVsbG8=", fr.Parts[0].InlineData.Data)
	assert.Equal(t, "attachment_1", fr.Parts[0].InlineData.DisplayName)
}

// Several outputs used to become several functionResponse parts for one call
// id. A function response answers a function call, and Gemini pairs them by id.
func TestToolResultIsOneResponsePerCall(t *testing.T) {
	fr := toolResponse(t, responses2.FunctionCallOutputContentUnion{
		OfList: responses2.InputContent{
			{OfInputText: &responses2.InputTextContent{Text: "first"}},
			{OfInputText: &responses2.InputTextContent{Text: "second"}},
			{OfInputImage: &responses2.InputImageContent{ImageURL: utils.Ptr("data:image/png;base64,aGVsbG8=")}},
		},
	})

	assert.Equal(t, "first\nsecond", fr.Response["output"])
	assert.Len(t, fr.Parts, 1)
}

func TestToolResultRoundTrips(t *testing.T) {
	fr := toolResponse(t, responses2.FunctionCallOutputContentUnion{
		OfList: responses2.InputContent{
			{OfInputText: &responses2.InputTextContent{Text: "here it is"}},
			{OfInputImage: &responses2.InputImageContent{ImageURL: utils.Ptr("data:image/png;base64,aGVsbG8=")}},
		},
	})

	msgs := MessagesToNativeMessages([]Content{{Role: RoleUser, Parts: []Part{{FunctionResponse: fr}}}})
	require.Len(t, msgs.OfInputMessageList, 1)
	out := msgs.OfInputMessageList[0].OfFunctionCallOutput
	require.NotNil(t, out)
	require.Len(t, out.Output.OfList, 2)
	assert.Equal(t, "here it is", out.Output.OfList[0].OfInputText.Text)
	require.NotNil(t, out.Output.OfList[1].OfInputImage, "an image survived the way out; it has to survive the way back")
	assert.Equal(t, "data:image/png;base64,aGVsbG8=", *out.Output.OfList[1].OfInputImage.ImageURL)
}

// A plain text result stays a plain string, which is what every other provider
// produces for one and what a reader of history expects.
func TestPlainToolResultStaysAString(t *testing.T) {
	fr := toolResponse(t, responses2.FunctionCallOutputContentUnion{OfString: utils.Ptr("22C")})
	msgs := MessagesToNativeMessages([]Content{{Role: RoleUser, Parts: []Part{{FunctionResponse: fr}}}})

	out := msgs.OfInputMessageList[0].OfFunctionCallOutput
	require.NotNil(t, out.Output.OfString)
	assert.Equal(t, "22C", *out.Output.OfString)
	assert.Empty(t, out.Output.OfList)
}

// The reverse used to pull an arbitrary value out of the response map and
// assert it to a string, which panics on a tool that answers with anything
// else — a number, a nested object, the $ref form Gemini itself documents.
func TestInboundNonStringToolResultDoesNotPanic(t *testing.T) {
	msgs := MessagesToNativeMessages([]Content{{
		Role: RoleUser,
		Parts: []Part{{FunctionResponse: &FunctionResponse{
			ID:       "call_1",
			Response: map[string]any{"temperature": 22, "unit": "C"},
		}}},
	}})

	require.Len(t, msgs.OfInputMessageList, 1)
	out := msgs.OfInputMessageList[0].OfFunctionCallOutput
	require.NotNil(t, out)
	require.NotNil(t, out.Output.OfString)
	assert.JSONEq(t, `{"temperature":22,"unit":"C"}`, *out.Output.OfString,
		"a tool speaking its own JSON is rendered as that JSON")
}
