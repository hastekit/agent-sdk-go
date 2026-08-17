package sdk

import (
	"fmt"

	"github.com/hastekit/agent-sdk-go/pkg/agents"
	"github.com/hastekit/agent-sdk-go/pkg/agents/streambroker"
)

func NewStreamBroker() (agents.StreamBroker, error) {
	return streambroker.NewMemoryStreamBroker(), nil
}

func NewRedisStreamBroker(redisEndpoint string) (agents.StreamBroker, error) {
	broker, err := streambroker.NewRedisStreamBroker(streambroker.RedisStreamBrokerOptions{
		Addr: redisEndpoint,
	})
	if err != nil {
		return nil, fmt.Errorf("error creating redis stream broker: %w", err)
	}

	return broker, nil
}
