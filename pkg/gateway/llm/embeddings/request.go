package embeddings

import (
	"errors"

	"github.com/bytedance/sonic"
	"github.com/hastekit/agent-sdk-go/pkg/utils"
)

type Request struct {
	Model          string         `json:"model"`
	Input          InputUnion     `json:"input"`
	Dimensions     *int           `json:"dimensions"`
	EncodingFormat *string        `json:"encoding_format"` // "float" or "base64"
	ExtraFields    map[string]any `json:",omitempty"`
}

type InputUnion struct {
	OfString *string  `json:",omitempty"`
	OfList   []string `json:",omitempty"`
}

func (u *InputUnion) UnmarshalJSON(data []byte) error {
	var s string
	if err := sonic.Unmarshal(data, &s); err == nil {
		u.OfString = utils.Ptr(s)
		return nil
	}

	var list []string
	if err := sonic.Unmarshal(data, &list); err == nil {
		u.OfList = list
		return nil
	}

	return errors.New("invalid input union")
}

func (u *InputUnion) MarshalJSON() ([]byte, error) {
	if u.OfString != nil {
		return sonic.Marshal(u.OfString)
	}

	if u.OfList != nil {
		return sonic.Marshal(u.OfList)
	}

	return nil, nil
}
