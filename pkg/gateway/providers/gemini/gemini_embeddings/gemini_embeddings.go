package gemini_embeddings

import (
	embeddings2 "github.com/hastekit/agent-sdk-go/pkg/gateway/llm/embeddings"
	"github.com/hastekit/agent-sdk-go/pkg/gateway/providers/gemini/gemini_responses"
	"github.com/hastekit/agent-sdk-go/pkg/utils"
)

type Request struct {
	*RequestObject
	Requests             []RequestObject `json:"requests,omitempty"`
	OutputDimensionality *int            `json:"output_dimensionality,omitempty"`
	TaskType             *string         `json:"task_type,omitempty"` // "SEMANTIC_SIMILARITY", "CLASSIFICATION", "CLUSTERING", "RETRIEVAL_DOCUMENT", "RETRIEVAL_QUERY", "CODE_RETRIEVAL_QUERY", "QUESTION_ANSWERING", "FACT_VERIFICATION"
}

func (r *Request) ToNativeRequest() *embeddings2.Request {
	req := &embeddings2.Request{
		Input:      embeddings2.InputUnion{},
		Dimensions: r.OutputDimensionality,
		ExtraFields: map[string]any{
			"task_type": r.TaskType,
		},
	}

	if r.Requests != nil && len(r.Requests) > 0 {
		arr := make([]string, len(r.Requests))
		for idx, item := range r.Requests {
			arr[idx] = item.Content.String()
			req.Model = item.Model
		}
		req.Input.OfList = arr
	} else {
		req.Model = r.Model
		req.Input.OfString = utils.Ptr(r.Content.String())
	}

	return req
}

func NativeRequestToRequest(in *embeddings2.Request) *Request {
	r := &Request{
		OutputDimensionality: in.Dimensions,
	}

	if in.ExtraFields != nil {
		if taskTypeAny, exists := in.ExtraFields["task_type"]; exists {
			if taskType, ok := taskTypeAny.(string); ok {
				r.TaskType = utils.Ptr(taskType)
			}
		}
	}

	if in.Input.OfString != nil {
		r.RequestObject = &RequestObject{
			Model: in.Model,
			Content: gemini_responses.Content{
				Parts: []gemini_responses.Part{
					{
						Text: in.Input.OfString,
					},
				},
			},
		}
	}

	if in.Input.OfList != nil {
		for _, item := range in.Input.OfList {
			r.Requests = append(r.Requests, RequestObject{
				Model: in.Model,
				Content: gemini_responses.Content{
					Parts: []gemini_responses.Part{
						{
							Text: utils.Ptr(item),
						},
					},
				},
			})
		}
	}

	return r
}

type RequestObject struct {
	Model   string                   `json:"model"`
	Content gemini_responses.Content `json:"content"`
}

type Response struct {
	Embedding  *Embedding   `json:"embedding,omitempty"`
	Embeddings []*Embedding `json:"embeddings,omitempty"`
}

func (r *Response) ToNativeResponse(model string) *embeddings2.Response {
	res := &embeddings2.Response{
		Object: "list",
		Model:  model,
	}

	if r.Embedding != nil {
		res.Data = []embeddings2.EmbeddingData{
			{
				Object: "embedding",
				Index:  0,
				Embedding: embeddings2.EmbeddingDataUnion{
					OfFloat: r.Embedding.Values,
				},
			},
		}
	}

	if r.Embeddings != nil {
		data := make([]embeddings2.EmbeddingData, len(r.Embeddings))
		for idx, e := range r.Embeddings {
			data[idx] = embeddings2.EmbeddingData{
				Object: "embedding",
				Index:  idx,
				Embedding: embeddings2.EmbeddingDataUnion{
					OfFloat: e.Values,
				},
			}
		}
		res.Data = data
	}

	return res
}

type Embedding struct {
	Values []float64 `json:"values"`
}

func NativeResponseToResponse(in *embeddings2.Response) *Response {
	res := &Response{
		Embedding:  nil,
		Embeddings: nil,
	}

	if len(in.Data) == 1 {
		res.Embedding = &Embedding{
			Values: in.Data[0].Embedding.OfFloat,
		}
	} else if len(in.Data) > 1 {
		arr := make([]*Embedding, len(in.Data))
		for idx, d := range in.Data {
			arr[idx] = &Embedding{
				Values: d.Embedding.OfFloat,
			}
		}
		res.Embeddings = arr
	}

	return res
}
