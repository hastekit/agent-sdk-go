package hastekitgateway

import (
	"context"
	"fmt"
	"net/http"
	"net/url"

	"github.com/hastekit/agent-sdk-go/pkg/knowledge/vectorstores"
	"github.com/hastekit/agent-sdk-go/pkg/utils"
)

type ExternalKnowledgePersistence struct {
	endpoint    string
	orgName     string
	projectName string
	name        string

	httpClient *http.Client
}

func (c *Config) NewExternalKnowledgePersistence(name string) *ExternalKnowledgePersistence {
	return &ExternalKnowledgePersistence{
		endpoint:    c.Endpoint,
		orgName:     c.OrgName,
		projectName: c.ProjectName,
		httpClient:  c.HttpClient,
		name:        name,
	}
}

func (kp *ExternalKnowledgePersistence) Search(ctx context.Context, query string, limit int) ([]vectorstores.SearchResult, error) {
	u, err := url.Parse(fmt.Sprintf("%s/knowledges/by-name/%s/search", projectBasePath(kp.endpoint, kp.orgName, kp.projectName), url.PathEscape(kp.name)))
	if err != nil {
		return nil, err
	}

	// Add query parameters (project scope is carried by the path).
	q := u.Query()
	q.Set("limit", fmt.Sprintf("%d", limit))
	q.Set("query", query)

	u.RawQuery = q.Encode()

	resp, err := kp.httpClient.Get(u.String())
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("knowledgePersistence response code: %d", resp.StatusCode)
	}

	data := Response[[]vectorstores.SearchResult]{}
	if err := utils.DecodeJSON(resp.Body, &data); err != nil {
		return nil, err
	}

	return data.Data, nil
}
