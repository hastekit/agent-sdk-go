package elevenlabs

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"strconv"

	"github.com/bytedance/sonic"
	speech2 "github.com/hastekit/agent-sdk-go/pkg/gateway/llm/speech"
	transcription2 "github.com/hastekit/agent-sdk-go/pkg/gateway/llm/transcription"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/providers/base"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/providers/elevenlabs/elevenlabs_speech"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/providers/elevenlabs/elevenlabs_transcription"
	"github.com/hastekit/agent-sdk-go/pkg/utils"
)

type ClientOptions struct {
	// https://api.elevenlabs.io/v1
	BaseURL string
	ApiKey  string
	Headers map[string]string

	transport *http.Client
}

type Client struct {
	*base.BaseProvider
	opts *ClientOptions
}

func NewClient(opts *ClientOptions) *Client {
	if opts.transport == nil {
		opts.transport = http.DefaultClient
	}

	if opts.BaseURL == "" {
		opts.BaseURL = "https://api.elevenlabs.io/v1"
	}

	return &Client{
		opts: opts,
	}
}

func (c *Client) NewSpeech(ctx context.Context, in *speech2.Request) (*speech2.Response, error) {
	elRequest := elevenlabs_speech.NativeRequestToRequest(in)

	payload, err := sonic.Marshal(elRequest)
	if err != nil {
		return nil, err
	}

	// ElevenLabs TTS endpoint: POST /v1/text-to-speech/{voice_id}
	voiceID := in.Voice
	if voiceID == "" {
		voiceID = "21m00Tcm4TlvDq8ikWAM" // Rachel - default voice
	}

	outputFormat := elevenlabs_speech.NativeResponseFormatToResponseFormat(in.ResponseFormat)

	req, err := http.NewRequest(http.MethodPost, c.opts.BaseURL+"/text-to-speech/"+voiceID+"?output_format="+outputFormat, bytes.NewBuffer(payload))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("xi-api-key", c.opts.ApiKey)

	res, err := c.opts.transport.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		var errResp map[string]any
		err = utils.DecodeJSON(res.Body, &errResp)
		if err != nil {
			return nil, err
		}
		if detail, ok := errResp["detail"].(map[string]any); ok {
			if message, ok := detail["message"].(string); ok {
				return nil, errors.New(message)
			}
		}
		if detail, ok := errResp["detail"].(string); ok {
			return nil, errors.New(detail)
		}
		return nil, errors.New("unknown error occurred")
	}

	// Handle gzip compressed response
	var reader io.Reader = res.Body
	if res.Header.Get("Content-Encoding") == "gzip" {
		gzipReader, err := gzip.NewReader(res.Body)
		if err != nil {
			return nil, err
		}
		defer gzipReader.Close()
		reader = gzipReader
	}

	audioData, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}

	elResponse := &elevenlabs_speech.Response{
		AudioData: audioData,
	}

	return elResponse.ToNativeResponse(), nil
}

func (c *Client) NewStreamingSpeech(ctx context.Context, in *speech2.Request) (chan *speech2.ResponseChunk, error) {
	elRequest := elevenlabs_speech.NativeRequestToRequest(in)

	payload, err := sonic.Marshal(elRequest)
	if err != nil {
		return nil, err
	}

	voiceID := in.Voice
	if voiceID == "" {
		voiceID = "21m00Tcm4TlvDq8ikWAM"
	}

	outputFormat := elevenlabs_speech.NativeResponseFormatToResponseFormat(in.ResponseFormat)

	// ElevenLabs streaming TTS endpoint: POST /v1/text-to-speech/{voice_id}/stream
	req, err := http.NewRequest(http.MethodPost, c.opts.BaseURL+"/text-to-speech/"+voiceID+"/stream?output_format="+outputFormat, bytes.NewBuffer(payload))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("xi-api-key", c.opts.ApiKey)

	res, err := c.opts.transport.Do(req)
	if err != nil {
		return nil, err
	}

	if res.StatusCode != http.StatusOK {
		defer res.Body.Close()
		var errResp map[string]any
		err = utils.DecodeJSON(res.Body, &errResp)
		if err != nil {
			return nil, err
		}
		if detail, ok := errResp["detail"].(map[string]any); ok {
			if message, ok := detail["message"].(string); ok {
				return nil, errors.New(message)
			}
		}
		if detail, ok := errResp["detail"].(string); ok {
			return nil, errors.New(detail)
		}
		return nil, errors.New("unknown error occurred")
	}

	out := make(chan *speech2.ResponseChunk)

	go func() {
		defer res.Body.Close()
		defer close(out)

		// ElevenLabs streams raw audio bytes, not SSE
		buf := make([]byte, 4096)
		for {
			n, err := res.Body.Read(buf)
			if n > 0 {
				audioCopy := make([]byte, n)
				copy(audioCopy, buf[:n])

				chunk := &speech2.ResponseChunk{
					OfAudioDelta: &speech2.ChunkAudioDelta[speech2.ChunkTypeAudioDelta]{
						Audio: string(audioCopy),
					},
				}
				out <- chunk
			}
			if err != nil {
				return
			}
		}
	}()

	return out, nil
}

func (c *Client) NewTranscription(ctx context.Context, in *transcription2.Request) (*transcription2.Response, error) {
	// Build multipart form body
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)

	// Add the audio file
	filename := in.AudioFilename
	if filename == "" {
		filename = "audio.mp3"
	}
	filePart, err := writer.CreateFormFile("file", filename)
	if err != nil {
		return nil, err
	}
	if _, err = filePart.Write(in.Audio); err != nil {
		return nil, err
	}

	// Add model field - ElevenLabs uses scribe_v2 / scribe_v1
	modelID := in.Model
	if modelID == "" {
		modelID = "scribe_v2"
	}
	if err = writer.WriteField("model_id", modelID); err != nil {
		return nil, err
	}

	// Add optional language
	if in.Language != nil {
		if err = writer.WriteField("language_code", *in.Language); err != nil {
			return nil, err
		}
	}

	// Add timestamps granularity
	if len(in.TimestampGranularities) > 0 {
		if err = writer.WriteField("timestamps_granularity", in.TimestampGranularities[0]); err != nil {
			return nil, err
		}
	} else {
		if err = writer.WriteField("timestamps_granularity", "word"); err != nil {
			return nil, err
		}
	}

	if in.Temperature != nil {
		if err = writer.WriteField("temperature", strconv.FormatFloat(*in.Temperature, 'f', -1, 64)); err != nil {
			return nil, err
		}
	}

	if err = writer.Close(); err != nil {
		return nil, err
	}

	// ElevenLabs STT endpoint: POST /v1/speech-to-text
	req, err := http.NewRequest(http.MethodPost, c.opts.BaseURL+"/speech-to-text", &buf)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("xi-api-key", c.opts.ApiKey)

	res, err := c.opts.transport.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		var errResp map[string]any
		err = utils.DecodeJSON(res.Body, &errResp)
		if err != nil {
			return nil, err
		}
		if detail, ok := errResp["detail"].(map[string]any); ok {
			if message, ok := detail["message"].(string); ok {
				return nil, errors.New(message)
			}
		}
		if detail, ok := errResp["detail"].(string); ok {
			return nil, errors.New(detail)
		}
		return nil, errors.New("unknown error occurred")
	}

	var elResponse *elevenlabs_transcription.Response
	err = utils.DecodeJSON(res.Body, &elResponse)
	if err != nil {
		return nil, err
	}

	return elResponse.ToNativeResponse(), nil
}
