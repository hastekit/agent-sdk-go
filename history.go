package sdk

import "github.com/hastekit/agent-sdk-go/pkg/agents/history"

type History = history.CommonConversationManager

// NewFileHistory opens file-backed history. The caller must Close it after all
// agents sharing it have finished. Agents never close caller-owned history.
func NewFileHistory(path string, opts ...history.ConversationManagerOptions) (*History, error) {
	p, err := history.NewFileConversationPersistence(path)
	if err != nil {
		return nil, err
	}
	return history.NewConversationManager(p, opts...), nil
}

// OpenFileHistory is an alias for NewFileHistory.
func OpenFileHistory(path string, opts ...history.ConversationManagerOptions) (*History, error) {
	return NewFileHistory(path, opts...)
}

// MustNewFileHistory panics if the history cannot be opened. Prefer OpenFileHistory
// when the application can recover from initialization errors.
func MustNewFileHistory(path string, opts ...history.ConversationManagerOptions) *History {
	h, err := NewFileHistory(path, opts...)
	if err != nil {
		panic(err)
	}
	return h
}
