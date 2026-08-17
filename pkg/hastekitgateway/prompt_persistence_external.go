package hastekitgateway

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/google/uuid"
	"github.com/hastekit/agent-sdk-go/pkg/agents/prompts"
	"github.com/hastekit/agent-sdk-go/pkg/utils"
)

// ExternalPromptPersistence resolves a (prompt name, alias) pair
// against the gateway and returns the underlying template. The
// gateway's /by-alias/{alias_name} endpoint runs alias resolution
// (including weighted traffic split for two-version aliases) and
// returns the chosen version's template — keeping the SDK to a
// single round trip per LoadPrompt call.
type ExternalPromptPersistence struct {
	Endpoint    string
	orgName     string
	projectName string
	name        string
	alias       string
	httpClient  *http.Client
}

func (c *Config) NewPrompt(name, alias string) *prompts.SimplePrompt {
	return prompts.NewWithLoader(&ExternalPromptPersistence{
		Endpoint:    c.Endpoint,
		orgName:     c.OrgName,
		projectName: c.ProjectName,
		httpClient:  c.HttpClient,
		name:        name,
		alias:       alias,
	})
}

// PromptVersion represents a version of a prompt template
type PromptVersion struct {
	ID        uuid.UUID `json:"id" db:"id"`
	PromptID  uuid.UUID `json:"prompt_id" db:"prompt_id"`
	Version   int       `json:"version" db:"version"`
	Template  string    `json:"template" db:"template"`
	Immutable bool      `json:"immutable" db:"immutable"`
	CreatedAt time.Time `json:"created_at" db:"created_at"`
	UpdatedAt time.Time `json:"updated_at" db:"updated_at"`
}

// PromptVersionWithPrompt combines a prompt version with its prompt information
type PromptVersionWithPrompt struct {
	PromptVersion
	PromptName string `json:"prompt_name" db:"prompt_name"`
}

func (p *ExternalPromptPersistence) LoadPrompt(ctx context.Context) (string, error) {
	url := fmt.Sprintf("%s/prompts/%s/by-alias/%s", projectBasePath(p.Endpoint, p.orgName, p.projectName), url.PathEscape(p.name), url.PathEscape(p.alias))

	resp, err := p.httpClient.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	data := Response[PromptVersionWithPrompt]{}
	if err := utils.DecodeJSON(resp.Body, &data); err != nil {
		return "", err
	}

	return data.Data.Template, nil
}
