package qdrant

import (
	"context"
	"encoding/binary"
	"fmt"
	"strconv"

	"github.com/hastekit/agent-sdk-go/pkg/knowledge/vectorstores"
	"github.com/qdrant/go-client/qdrant"
)

const (
	// DefaultHost is the default Qdrant host.
	DefaultHost = "localhost"
	// DefaultPort is the default Qdrant gRPC port.
	DefaultPort = 6334
)

// Store implements knowledge.VectorStore using Qdrant.
// Collection name is passed per call; each knowledge base uses its own collection.
type Store struct {
	client *qdrant.Client
}

// Config configures the Qdrant connection.
type Config struct {
	Host string
	Port int
	// APIKey is optional (e.g. for Qdrant Cloud).
	APIKey string
}

// NewStore creates a new Qdrant-backed vector store.
func NewStore(cfg Config) (*Store, error) {
	if cfg.Port == 0 {
		cfg.Port = DefaultPort
	}
	if cfg.Host == "" {
		cfg.Host = DefaultHost
	}
	clientCfg := &qdrant.Config{
		Host: cfg.Host,
		Port: cfg.Port,
	}
	if cfg.APIKey != "" {
		clientCfg.APIKey = cfg.APIKey
	}
	client, err := qdrant.NewClient(clientCfg)
	if err != nil {
		return nil, fmt.Errorf("qdrant new client: %w", err)
	}
	return &Store{client: client}, nil
}

// EnsureCollection creates the collection if it does not exist.
func (s *Store) EnsureCollection(ctx context.Context, collectionName string, dimension int) error {
	if dimension <= 0 {
		return fmt.Errorf("vector dimension must be > 0")
	}
	exists, err := s.client.CollectionExists(ctx, collectionName)
	if err != nil {
		return fmt.Errorf("collection exists: %w", err)
	}
	if exists {
		return nil
	}
	err = s.client.CreateCollection(ctx, &qdrant.CreateCollection{
		CollectionName: collectionName,
		VectorsConfig: qdrant.NewVectorsConfig(&qdrant.VectorParams{
			Size:     uint64(dimension),
			Distance: qdrant.Distance_Cosine,
		}),
	})
	if err != nil {
		return fmt.Errorf("create collection: %w", err)
	}
	return nil
}

// UpsertVectors inserts or updates vectors with metadata.
func (s *Store) UpsertVectors(ctx context.Context, collectionName string, ids []string, embeddings [][]float64, metadata []map[string]any) error {
	if len(ids) != len(embeddings) {
		return fmt.Errorf("ids length %d != embeddings length %d", len(ids), len(embeddings))
	}
	if metadata != nil && len(metadata) != len(ids) {
		return fmt.Errorf("metadata length %d != ids length %d", len(metadata), len(ids))
	}
	points := make([]*qdrant.PointStruct, len(ids))
	for i := range ids {
		payload := map[string]any{"id": ids[i]}
		if metadata != nil && i < len(metadata) {
			for k, v := range metadata[i] {
				payload[k] = v
			}
		}
		vec32 := float64ToFloat32(embeddings[i])
		points[i] = &qdrant.PointStruct{
			Id:      stringIDToPointID(ids[i]),
			Vectors: qdrant.NewVectors(vec32...),
			Payload: qdrant.NewValueMap(payload),
		}
	}
	wait := true
	_, err := s.client.Upsert(ctx, &qdrant.UpsertPoints{
		CollectionName: collectionName,
		Wait:           &wait,
		Points:         points,
	})
	if err != nil {
		return fmt.Errorf("upsert: %w", err)
	}
	return nil
}

// Search performs a vector similarity search.
func (s *Store) Search(ctx context.Context, collectionName string, queryVector []float64, limit int, filter map[string]any) ([]vectorstores.SearchResult, error) {
	if limit <= 0 {
		limit = 10
	}
	vec32 := float64ToFloat32(queryVector)
	limit64 := uint64(limit)
	req := &qdrant.QueryPoints{
		CollectionName: collectionName,
		Query:          qdrant.NewQueryNearest(qdrant.NewVectorInput(vec32...)),
		Limit:          &limit64,
		WithPayload:    qdrant.NewWithPayload(true),
	}
	if len(filter) > 0 {
		req.Filter = buildFilter(filter)
	}
	points, err := s.client.Query(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("qdrant query: %w", err)
	}
	out := make([]vectorstores.SearchResult, 0, len(points))
	for _, p := range points {
		meta := payloadToMap(p.GetPayload())
		id := ""
		if v, ok := meta["id"]; ok {
			if s, ok := v.(string); ok {
				id = s
			}
		}
		if id == "" && p.GetId() != nil {
			id = pointIDToString(p.GetId())
		}
		out = append(out, vectorstores.SearchResult{
			ID:       id,
			Score:    p.GetScore(),
			Metadata: meta,
		})
	}
	return out, nil
}

// DeleteCollection removes the entire collection.
func (s *Store) DeleteCollection(ctx context.Context, collectionName string) error {
	return s.client.DeleteCollection(ctx, collectionName)
}

// DeleteVectors removes specific vectors by ID.
func (s *Store) DeleteVectors(ctx context.Context, collectionName string, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	pointIDs := make([]*qdrant.PointId, len(ids))
	for i, id := range ids {
		pointIDs[i] = stringIDToPointID(id)
	}
	wait := true
	_, err := s.client.Delete(ctx, &qdrant.DeletePoints{
		CollectionName: collectionName,
		Wait:           &wait,
		Points:         qdrant.NewPointsSelector(pointIDs...),
	})
	if err != nil {
		return fmt.Errorf("delete vectors: %w", err)
	}
	return nil
}

// Close closes the Qdrant client.
func (s *Store) Close() error {
	return s.client.Close()
}

var _ vectorstores.VectorStore = (*Store)(nil)

func float64ToFloat32(a []float64) []float32 {
	out := make([]float32, len(a))
	for i := range a {
		out[i] = float32(a[i])
	}
	return out
}

// buildFilter constructs a Qdrant filter from a simple map (key-value match).
// For complex filters the knowledge API would need to pass a structured type.
func buildFilter(m map[string]any) *qdrant.Filter {
	if len(m) == 0 {
		return nil
	}
	conditions := make([]*qdrant.Condition, 0, len(m))
	for k, v := range m {
		c := matchCondition(k, v)
		if c != nil {
			conditions = append(conditions, c)
		}
	}
	if len(conditions) == 0 {
		return nil
	}
	return &qdrant.Filter{
		Must: conditions,
	}
}

func matchCondition(key string, v any) *qdrant.Condition {
	switch val := v.(type) {
	case string:
		return qdrant.NewMatch(key, val)
	case int:
		return qdrant.NewMatchInt(key, int64(val))
	case int64:
		return qdrant.NewMatchInt(key, val)
	case bool:
		return qdrant.NewMatchBool(key, val)
	default:
		return nil
	}
}

// payloadToMap converts Qdrant payload map to map[string]any for SearchResult.Metadata.
func payloadToMap(m map[string]*qdrant.Value) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		if v != nil {
			out[k] = valueToAny(v)
		}
	}
	return out
}

func valueToAny(v *qdrant.Value) any {
	if v == nil {
		return nil
	}
	switch v.GetKind().(type) {
	case *qdrant.Value_StringValue:
		return v.GetStringValue()
	case *qdrant.Value_IntegerValue:
		return v.GetIntegerValue()
	case *qdrant.Value_DoubleValue:
		return v.GetDoubleValue()
	case *qdrant.Value_BoolValue:
		return v.GetBoolValue()
	case *qdrant.Value_NullValue:
		return nil
	case *qdrant.Value_ListValue:
		lv := v.GetListValue()
		if lv == nil {
			return nil
		}
		vals := lv.GetValues()
		arr := make([]any, len(vals))
		for i, u := range vals {
			arr[i] = valueToAny(u)
		}
		return arr
	case *qdrant.Value_StructValue:
		st := v.GetStructValue()
		if st == nil {
			return nil
		}
		return payloadToMap(st.GetFields())
	default:
		return nil
	}
}

// stringIDToPointID converts a string ID to Qdrant PointId (numeric).
// Uses first 8 bytes of hex-decoded ID when possible, else FNV-1a hash.
func stringIDToPointID(id string) *qdrant.PointId {
	var num uint64
	if len(id) >= 16 {
		var b [8]byte
		for i := 0; i < 8 && (i+1)*2 <= len(id); i++ {
			lo := hexChar(id[i*2])
			hi := hexChar(id[i*2+1])
			b[i] = lo<<4 | hi
		}
		num = binary.BigEndian.Uint64(b[:])
	}
	if num == 0 {
		const prime = 1099511628211
		num = 14695981039346656037
		for _, c := range id {
			num ^= uint64(c)
			num *= prime
		}
	}
	return qdrant.NewIDNum(num)
}

func pointIDToString(id *qdrant.PointId) string {
	if id == nil {
		return ""
	}
	return strconv.FormatUint(id.GetNum(), 16)
}

func hexChar(c byte) byte {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10
	}
	return 0
}
