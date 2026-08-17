package sarvam

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/bytedance/sonic"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/speech"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/transcription"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/providers/sarvam/sarvam_speech"
	"github.com/hastekit/agent-sdk-go/pkg/utils"
)

func TestNewSpeech(t *testing.T) {
	var gotPath, gotKey string
	var gotBody sarvam_speech.Request

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotKey = r.Header.Get(APIKeyHeader)
		body, _ := io.ReadAll(r.Body)
		_ = sonic.Unmarshal(body, &gotBody)

		audio := base64.StdEncoding.EncodeToString([]byte("RIFFfake"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"request_id":"req_1","audios":["` + audio + `"]}`))
	}))
	defer server.Close()

	client := NewClient(&ClientOptions{BaseURL: server.URL, ApiKey: "sk_test"})

	out, err := client.NewSpeech(context.Background(), &speech.Request{
		Input:    "नमस्ते",
		Voice:    "anushka",
		Language: utils.Ptr("hi-IN"),
	})
	if err != nil {
		t.Fatalf("NewSpeech: %v", err)
	}

	if gotPath != "/text-to-speech" {
		t.Errorf("path = %q, want /text-to-speech", gotPath)
	}
	if gotKey != "sk_test" {
		t.Errorf("%s = %q", APIKeyHeader, gotKey)
	}
	if gotBody.LanguageCode != "hi-IN" || gotBody.Speaker != "anushka" {
		t.Errorf("request = %+v", gotBody)
	}
	if gotBody.Model != sarvam_speech.DefaultModel {
		t.Errorf("model = %q, want the default %q", gotBody.Model, sarvam_speech.DefaultModel)
	}

	// Sarvam returns base64 inside JSON, not a binary body.
	if string(out.Audio) != "RIFFfake" {
		t.Errorf("audio = %q, want the decoded bytes", out.Audio)
	}
	if out.ContentType != "audio/wav" {
		t.Errorf("content type = %q, want audio/wav", out.ContentType)
	}
}

// language_code is required by the API and has no server-side default.
func TestNewSpeechDefaultsLanguage(t *testing.T) {
	var gotBody sarvam_speech.Request

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = sonic.Unmarshal(body, &gotBody)
		_, _ = w.Write([]byte(`{"request_id":"r","audios":[]}`))
	}))
	defer server.Close()

	client := NewClient(&ClientOptions{BaseURL: server.URL, ApiKey: "sk_test"})

	if _, err := client.NewSpeech(context.Background(), &speech.Request{Input: "hello"}); err != nil {
		t.Fatalf("NewSpeech: %v", err)
	}

	if gotBody.LanguageCode != sarvam_speech.DefaultLanguageCode {
		t.Errorf("language_code = %q, want %q", gotBody.LanguageCode, sarvam_speech.DefaultLanguageCode)
	}
}

func TestNewTranscription(t *testing.T) {
	var gotPath string
	fields := map[string]string{}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("ParseMultipartForm: %v", err)
		}
		for key := range r.MultipartForm.Value {
			fields[key] = r.FormValue(key)
		}
		if file, header, err := r.FormFile("file"); err == nil {
			defer file.Close()
			fields["__filename"] = header.Filename
			data, _ := io.ReadAll(file)
			fields["__audio"] = string(data)
		} else {
			t.Errorf("FormFile: %v", err)
		}

		_, _ = w.Write([]byte(`{"request_id":"req_1","transcript":"नमस्ते","language_code":"hi-IN",
			"timestamps":{"words":["नमस्ते"],"start_time_seconds":[0.1],"end_time_seconds":[0.8]}}`))
	}))
	defer server.Close()

	client := NewClient(&ClientOptions{BaseURL: server.URL, ApiKey: "sk_test"})

	out, err := client.NewTranscription(context.Background(), &transcription.Request{
		Audio:                  []byte("audio-bytes"),
		AudioFilename:          "clip.wav",
		Model:                  "saaras:v3",
		Language:               utils.Ptr("hi-IN"),
		TimestampGranularities: []string{"word"},
	})
	if err != nil {
		t.Fatalf("NewTranscription: %v", err)
	}

	if gotPath != "/speech-to-text" {
		t.Errorf("path = %q, want /speech-to-text", gotPath)
	}
	if fields["model"] != "saaras:v3" || fields["language_code"] != "hi-IN" || fields["with_timestamps"] != "true" {
		t.Errorf("form fields = %+v", fields)
	}
	if fields["__filename"] != "clip.wav" || fields["__audio"] != "audio-bytes" {
		t.Errorf("uploaded file = %q/%q", fields["__filename"], fields["__audio"])
	}

	if out.Text != "नमस्ते" || out.Language == nil || *out.Language != "hi-IN" {
		t.Errorf("response = %+v", out)
	}

	// Sarvam's parallel timing arrays have to become per-word entries.
	if len(out.Words) != 1 || out.Words[0].Word != "नमस्ते" || out.Words[0].Start != 0.1 || out.Words[0].End != 0.8 {
		t.Errorf("words = %+v", out.Words)
	}
}

// Model is left off when unset: the accepted enum moves with the model
// generation and an unknown value is a hard error.
func TestNewTranscriptionOmitsEmptyModel(t *testing.T) {
	var hasModel bool
	var languageCode string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseMultipartForm(1 << 20)
		_, hasModel = r.MultipartForm.Value["model"]
		languageCode = r.FormValue("language_code")
		_, _ = w.Write([]byte(`{"transcript":"hi"}`))
	}))
	defer server.Close()

	client := NewClient(&ClientOptions{BaseURL: server.URL, ApiKey: "sk_test"})

	if _, err := client.NewTranscription(context.Background(), &transcription.Request{Audio: []byte("a")}); err != nil {
		t.Fatalf("NewTranscription: %v", err)
	}

	if hasModel {
		t.Error("model was sent even though the request did not set one")
	}
	if languageCode != "unknown" {
		t.Errorf("language_code = %q, want unknown (auto-detect)", languageCode)
	}
}

// Chat has to reach the OpenAI-compatible surface under /v1, carrying both
// the bearer token and the subscription key.
func TestChatUsesV1AndBothAuthHeaders(t *testing.T) {
	var gotPath, gotAuth, gotKey string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotKey = r.Header.Get(APIKeyHeader)

		_, _ = w.Write([]byte(`{"id":"c1","model":"sarvam-105b","choices":[{"index":0,"finish_reason":"stop",
			"message":{"role":"assistant","content":"नमस्ते"}}]}`))
	}))
	defer server.Close()

	client := NewClient(&ClientOptions{BaseURL: server.URL, ApiKey: "sk_test"})

	out, err := client.NewResponses(context.Background(), &responses.Request{
		Model: "sarvam-105b",
		Input: responses.InputUnion{OfString: utils.Ptr("hi")},
	})
	if err != nil {
		t.Fatalf("NewResponses: %v", err)
	}

	if gotPath != "/v1/chat/completions" {
		t.Errorf("path = %q, want /v1/chat/completions", gotPath)
	}
	if gotAuth != "Bearer sk_test" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotKey != "sk_test" {
		t.Errorf("%s = %q", APIKeyHeader, gotKey)
	}
	if len(out.Output) != 1 {
		t.Fatalf("output = %+v", out.Output)
	}
}
