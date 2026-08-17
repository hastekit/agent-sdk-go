package hastekitgateway

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/url"

	"github.com/bytedance/sonic"
	"github.com/google/uuid"
	"github.com/hastekit/agent-sdk-go/pkg/agents/history"
	"github.com/hastekit/agent-sdk-go/pkg/utils"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
)

// projectBasePath builds the GitHub-style project-scoped API base shared by all
// agent-server adapters: {endpoint}/api/agent-server/orgs/{orgName}/projects/{projectName}.
// The server resolves the org + project name to ids from the path.
func projectBasePath(endpoint, orgName, projectName string) string {
	return fmt.Sprintf("%s/api/agent-server/orgs/%s/projects/%s",
		endpoint, url.PathEscape(orgName), url.PathEscape(projectName))
}

var (
	tracer = otel.Tracer("HastekitAdapters")
)

type Response[T any] struct {
	ctx     context.Context
	Error   bool   `json:"error"`
	Message string `json:"message"`
	Data    T      `json:"data"`
	Status  int    `json:"status"`
}

type ExternalConversationPersistence struct {
	Endpoint    string
	orgName     string
	projectName string
	httpClient  *http.Client
}

func (c *Config) NewHistory() *history.CommonConversationManager {
	return history.NewConversationManager(&ExternalConversationPersistence{
		Endpoint:    c.Endpoint,
		orgName:     c.OrgName,
		projectName: c.ProjectName,
		httpClient:  c.HttpClient,
	})
}

// NewConversationID generates a unique ID for a conversation
func (p *ExternalConversationPersistence) NewConversationID(ctx context.Context) string {
	return uuid.NewString()
}

// NewRunID generates a unique ID for a run
func (p *ExternalConversationPersistence) NewRunID(ctx context.Context) string {
	return uuid.NewString()
}

// LoadMessages implements core.ChatHistory
func (p *ExternalConversationPersistence) LoadMessages(ctx context.Context, namespace string, threadId string, previousRunId string) ([]history.ConversationMessage, error) {
	ctx, span := tracer.Start(ctx, "ExternalConversationPersistence.LoadMessages")
	defer span.End()

	span.SetAttributes(
		attribute.String("namespace", namespace),
		attribute.String("thread_id", threadId),
		attribute.String("previous_run_id", previousRunId),
	)

	// If no previous message ID, return empty list
	if threadId == "" {
		return []history.ConversationMessage{}, nil
	}

	url := fmt.Sprintf("%s/messages/summary?namespace=%s&thread_id=%s&previous_run_id=%s", projectBasePath(p.Endpoint, p.orgName, p.projectName), namespace, threadId, previousRunId)

	resp, err := p.httpClient.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	data := Response[[]history.ConversationMessage]{}
	if err := utils.DecodeJSON(resp.Body, &data); err != nil {
		return nil, err
	}

	span.SetAttributes(attribute.Int("conversation_messages_count", len(data.Data)))

	return data.Data, nil
}

// LoadTranscript implements history.TranscriptReader: the thread as written,
// for display.
//
// It reads /messages, which is a plain ordered select over the thread's turns.
// The sibling /messages/summary that LoadMessages uses answers a different
// question — it substitutes a summary for the turns the summary covers, which
// is what keeps a long thread inside the context window and what would make a
// chat UI lose its own early turns. Note that endpoint fills an empty
// previous_run_id with the thread's last run, so asking it for "the
// whole thread" is exactly when the substitution kicks in.
func (p *ExternalConversationPersistence) LoadTranscript(ctx context.Context, namespace, threadID string) ([]history.ConversationMessage, error) {
	ctx, span := tracer.Start(ctx, "ExternalConversationPersistence.LoadTranscript")
	defer span.End()

	span.SetAttributes(
		attribute.String("namespace", namespace),
		attribute.String("thread_id", threadID),
	)

	if threadID == "" {
		return []history.ConversationMessage{}, nil
	}

	endpoint := fmt.Sprintf("%s/messages?namespace=%s&thread_id=%s",
		projectBasePath(p.Endpoint, p.orgName, p.projectName),
		url.QueryEscape(namespace), url.QueryEscape(threadID))

	resp, err := p.httpClient.Get(endpoint)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	data := Response[[]history.ConversationMessage]{}
	if err := utils.DecodeJSON(resp.Body, &data); err != nil {
		return nil, err
	}

	span.SetAttributes(attribute.Int("conversation_messages_count", len(data.Data)))

	return data.Data, nil
}

type AddMessageRequest struct {
	ProjectID      uuid.UUID         `json:"project_id"`
	Namespace      string            `json:"namespace"`
	RunID          string            `json:"run_id"`
	ThreadID       string            `json:"thread_id"`
	PreviousRunID  string            `json:"previous_run_id"`
	Messages       []history.Message `json:"messages"`
	Meta           map[string]any    `json:"meta"`
	ConversationID string            `json:"conversation_id"`
}

// SaveMessages implements core.ChatHistory
func (p *ExternalConversationPersistence) SaveMessages(ctx context.Context, namespace, runId, previousRunId, threadId string, conversationId string, messages []history.Message, meta map[string]any) error {
	ctx, span := tracer.Start(ctx, "ExternalConversationPersistence.SaveMessages")
	defer span.End()

	span.SetAttributes(
		attribute.String("namespace", namespace),
		attribute.String("thread_id", threadId),
		attribute.String("previous_run_id", previousRunId),
		attribute.String("conversation_id", conversationId),
		attribute.Int("messages_count", len(messages)),
	)

	// Save regular messages
	url := fmt.Sprintf("%s/messages", projectBasePath(p.Endpoint, p.orgName, p.projectName))

	payload := AddMessageRequest{
		Namespace:      namespace,
		RunID:          runId,
		ThreadID:       threadId,
		PreviousRunID:  previousRunId,
		Messages:       messages,
		Meta:           meta,
		ConversationID: conversationId,
	}

	payloadBytes, err := sonic.Marshal(payload)
	if err != nil {
		span.RecordError(err)
		return err
	}

	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(payloadBytes))
	if err != nil {
		span.RecordError(err)
		return err
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		span.RecordError(err)
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		err = fmt.Errorf("failed to save messages: status %d", resp.StatusCode)
		span.RecordError(err)
		return err
	}

	return nil
}

// SaveSummary
func (p *ExternalConversationPersistence) SaveSummary(ctx context.Context, namespace string, summary history.Summary) error {
	ctx, span := tracer.Start(ctx, "ExternalConversationPersistence.SaveSummary")
	defer span.End()

	url := fmt.Sprintf("%s/summary?namespace=%s", projectBasePath(p.Endpoint, p.orgName, p.projectName), namespace)

	payloadBytes, err := sonic.Marshal(summary)
	if err != nil {
		span.RecordError(err)
		return err
	}

	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(payloadBytes))
	if err != nil {
		span.RecordError(err)
		return err
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		span.RecordError(err)
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		err = fmt.Errorf("failed to save messages: status %d", resp.StatusCode)
		span.RecordError(err)
		return err
	}

	return nil
}
