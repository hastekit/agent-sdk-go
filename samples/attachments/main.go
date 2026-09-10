// Run with OPENAI_API_KEY set, then open http://localhost:8080.
package main

import (
	"log"
	"os"

	sdk "github.com/hastekit/agent-sdk-go"
	"github.com/hastekit/agent-sdk-go/pkg/agents"
	agentmiddleware "github.com/hastekit/agent-sdk-go/pkg/agents/middleware"
	"github.com/hastekit/agent-sdk-go/pkg/agui"
	"github.com/hastekit/agent-sdk-go/pkg/agui/web"
	"github.com/hastekit/agent-sdk-go/pkg/attachments"
)

func main() {
	// Files are stored under the agent namespace: the chat handler's default
	// namespace here, an identity-guarded one in a hosted app.
	store, err := attachments.NewFileStore("./attachment-files", attachments.FileStoreConfig{})
	if err != nil {
		log.Fatal(err)
	}
	defer store.Close()
	client := sdk.NewLLMClient([]sdk.ProviderConfig{{ProviderName: sdk.ProviderOpenAI,
		ApiKeys: []*sdk.APIKeyConfig{{APIKey: os.Getenv("OPENAI_API_KEY")}},
	}})
	sdk.NewAgent(&sdk.AgentConfig{
		Name: "Attachments", Instruction: sdk.NewPrompt("Help the user understand their attached images and PDF documents."),
		LLM: client.Model("OpenAI/gpt-4.1-mini"),
		// One middleware, both directions: what a tool returns inline is stored and
		// replaced by a reference, and every reference in a request becomes
		// inline data for the model inside the model call itself. Nothing on
		// the LLM client is involved.
		Middlewares: []agents.Middleware{agentmiddleware.NewAttachmentMiddleware(agentmiddleware.AttachmentMiddlewareConfig{Store: store})},
		History:     sdk.NewFileHistory("./attachment-conversations"),
	})
	log.Fatal(web.Serve("localhost:8080", &sdk.AgentRegistry{}, agui.WithAttachmentStore(store)))
}
