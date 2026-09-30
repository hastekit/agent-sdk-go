// An agent whose tools come from an MCP server that runs as a child process
// rather than as a service — the stdio transport.
//
// Many MCP servers ship as a command instead of a URL, which means there is
// nothing to deploy or point at: the SDK starts the process, speaks to it over
// its stdin and stdout, and keeps it around for as long as the tools are in use.
//
// This one uses the filesystem server, so it needs npx on PATH:
//
//	npx -y @modelcontextprotocol/server-filesystem /tmp
//
// Any command that speaks MCP over stdio works the same way.
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/bytedance/sonic"
	hastekit "github.com/hastekit/agent-sdk-go"
	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/history"
	"github.com/hastekit/agent-sdk-go/pkg/agents/mcpclient"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/llm/responses"
)

func main() {
	client := hastekit.NewLLMClient([]hastekit.ProviderConfig{
		{
			ProviderName: hastekit.ProviderOpenAI,
			ApiKeys: []*hastekit.APIKeyConfig{
				{
					Name:   "Key 1",
					APIKey: os.Getenv("OPENAI_API_KEY"),
				},
			},
		},
	})

	model := client.Model("OpenAI/gpt-4.1-mini")

	// WithCommand selects the stdio transport, so WithTransport is not needed
	// as well. There is no URL to connect to, so the endpoint is empty.
	mcpClient := mcpclient.NewClient(mcpclient.NewMemoryStore().WithMCPServerConfig([]mcpclient.ServerConfig{{Name: "filesystem",
		Endpoint:              "",
		Transport:             mcpclient.TransportStdio,
		Command:               []string{"npx", "-y", "@modelcontextprotocol/server-filesystem", "/tmp"},
		ToolPrefix:            "fs__",
		ToolFilter:            mcpclient.ToolFilter{Include: []string{"read_file", "list_directory"}},
		ApprovalRequiredTools: []string{"read_file"},
		Env: map[string]string{
			"WORKSPACE": "{{workspace}}",
		}}}))

	agent := hastekit.MustNewAgent(&hastekit.AgentConfig{
		Name:        "Filesystem_Agent",
		Instruction: hastekit.NewPrompt("You are a helpful assistant that can read files under /tmp."),
		LLM:         model,
		MCPClient:   mcpClient,
	})

	handle, err := agent.Execute(context.Background(), &agents.AgentInput{
		Message: history.Message{
			Messages: []responses.InputMessageUnion{
				responses.UserMessage("What files are in /tmp?"),
			},
		},
		// Whatever the env templates above refer to.
		RunContext: map[string]any{"workspace": "/tmp"},
	})
	if err != nil {
		log.Fatal(err)
	}

	out, err := handle.Result()
	if err != nil {
		log.Fatal(err)
	}

	b, _ := sonic.Marshal(out)
	fmt.Println(string(b))
}
