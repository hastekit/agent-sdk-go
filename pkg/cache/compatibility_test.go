package cache_test

import (
	"github.com/hastekit/agent-sdk-go/pkg/agents/mcpclient"
	"github.com/hastekit/agent-sdk-go/pkg/cache"
	"github.com/hastekit/agent-sdk-go/pkg/skills"
)

// Neither implementation imports a consumer or a shared interface.
var (
	_ mcpclient.SchemaCache = (*cache.MemoryCache)(nil)
	_ skills.KeyValueStore  = (*cache.MemoryCache)(nil)
	_ mcpclient.SchemaCache = (*cache.RedisCache)(nil)
	_ skills.KeyValueStore  = (*cache.RedisCache)(nil)
)
