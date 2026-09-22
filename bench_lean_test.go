package main

import "encoding/json"

// leanTransformRequest 是只保留插件真正需要的字段的载荷视图：TranslatedRequest 用
// RawMessage 接住（不做 base64 解码），Body/OriginalRequest 才解码。它只用于对照基准，
// 用来估算「不解码用不到的字段」能省下的时间。
type leanTransformRequest struct {
	FromFormat        string          `json:"FromFormat"`
	ToFormat          string          `json:"ToFormat"`
	Model             string          `json:"Model"`
	Stream            bool            `json:"Stream"`
	OriginalRequest   []byte          `json:"OriginalRequest"`
	TranslatedRequest json.RawMessage `json:"TranslatedRequest"`
	Body              []byte          `json:"Body"`
}
