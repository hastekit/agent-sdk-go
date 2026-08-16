package sdk

import "github.com/hastekit/hastekit-sdk-go/pkg/agents/history"

type History = history.CommonConversationManager

func NewFileHistory(path string, opts ...history.ConversationManagerOptions) *History {
	p, err := history.NewFileConversationPersistence(path)
	if err != nil {
		panic(err)
	}

	return history.NewConversationManager(p, opts...)
}
