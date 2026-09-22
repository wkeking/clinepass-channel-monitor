package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// 这些基准量化「插件在请求路径上多花多少时间」。宿主对每个钩子的载荷是 JSON，且
// OriginalRequest / TranslatedRequest / Body 都是 []byte（JSON 里是 base64），所以一次
// response.normalize_before 调用的开销 = 解码载荷 + 对原始请求体做一次 sha256 +
// 对响应体做一次 bytes.Contains（命中渠道标记时再多一次 JSON 解码）。
//
// 用法：make bench（GOCACHE 等已在 Makefile 里指向仓库内）

const benchChannelBody = `{"choices":[{"finish_reason":"stop","index":0,"message":{"content":"%s","provider_metadata":{"deepseek":{"choiceIndex":0,"promptCacheHitTokens":0,"promptCacheMissTokens":33,"systemFingerprint":"fp_fixture0000000000000000000001"},"gateway":{"cost":"0.0000447","generationId":"gen_01M3","inputInferenceCost":"0.0000099","outputInferenceCost":"0.0000348","routing":{"finalProvider":"deepseek","resolvedProvider":"deepseek","canonicalSlug":"deepseek/deepseek-v4.1-flash","originalModelId":"deepseek/deepseek-v4.1-flash","modelAttemptCount":1,"totalProviderAttemptCount":1,"planningReasoning":"System credentials planned for: fixture provider (synthetic test data)."}}}}}]}`

func paddedJSON(total int) []byte {
	filler := strings.Repeat("x", 1024)
	var builder strings.Builder
	builder.WriteString(`{"choices":[{"message":{"content":"`)
	for builder.Len() < total {
		builder.WriteString(filler)
	}
	builder.WriteString(`"}}]}`)
	return []byte(builder.String())
}

func channelJSON(total int) []byte {
	filler := strings.Repeat("y", maxInt(0, total-512))
	return []byte(strings.Replace(benchChannelBody, "%s", filler, 1))
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// hookPayload 构造一次真实的钩子调用载荷：客户端原始请求体 + 上游请求体 + 一段响应体。
func hookPayload(tb testing.TB, requestSize int, body []byte, stream bool) []byte {
	tb.Helper()
	request := paddedJSON(requestSize)
	payload, errMarshal := json.Marshal(pluginapi.ResponseTransformRequest{
		FromFormat:        "openai",
		ToFormat:          "openai-response",
		Model:             "deepseek-flash",
		Stream:            stream,
		OriginalRequest:   request,
		TranslatedRequest: request,
		Body:              body,
	})
	if errMarshal != nil {
		tb.Fatalf("marshal payload: %v", errMarshal)
	}
	return payload
}

// 一次 sha256（requestHash 的主体），对应钩子里「按原始请求体算请求指纹」这一步。
func BenchmarkRequestHash(b *testing.B) {
	for _, size := range []int{8 << 10, 64 << 10, 256 << 10} {
		body := paddedJSON(size)
		b.Run(sizeName(size), func(b *testing.B) {
			b.SetBytes(int64(len(body)))
			for i := 0; i < b.N; i++ {
				if hash := requestHash(body); hash == "" {
					b.Fatal("empty hash")
				}
			}
		})
	}
}

// 最常见的路径：响应里没有渠道标记（非 Cline 请求或还没到带标记的那一帧），
// 插件解码载荷 + 算指纹 + 扫描一遍响应体就返回空 body。
func BenchmarkNormalizeHookMarkerMissing(b *testing.B) {
	for _, size := range []int{8 << 10, 64 << 10, 1 << 20} {
		body := paddedJSON(size)
		payload := hookPayload(b, 64<<10, body, true)
		loadConfig([]byte("enabled: true\nhosts: [\"api.cline.bot\"]\n"))
		b.Run(sizeName(size), func(b *testing.B) {
			b.SetBytes(int64(len(payload)))
			for i := 0; i < b.N; i++ {
				if _, errHook := handleResponseNormalizeBefore(payload); errHook != nil {
					b.Fatalf("hook: %v", errHook)
				}
			}
		})
	}
}

// 对照：只用得到的字段解码（跳过 TranslatedRequest），用于估算可以省下多少。
func BenchmarkPayloadDecodeFull(b *testing.B) {
	payload := hookPayload(b, 1<<20, paddedJSON(2<<10), true)
	b.SetBytes(int64(len(payload)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var req pluginapi.ResponseTransformRequest
		if errDecode := json.Unmarshal(payload, &req); errDecode != nil {
			b.Fatalf("decode: %v", errDecode)
		}
	}
}

func BenchmarkPayloadDecodeLean(b *testing.B) {
	payload := hookPayload(b, 1<<20, paddedJSON(2<<10), true)
	b.SetBytes(int64(len(payload)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var req leanTransformRequest
		if errDecode := json.Unmarshal(payload, &req); errDecode != nil {
			b.Fatalf("decode: %v", errDecode)
		}
	}
}

// 1MB 请求体下的完整钩子调用（大请求体的真实量级）。
func BenchmarkNormalizeHookWithBigRequest(b *testing.B) {
	payload := hookPayload(b, 1<<20, paddedJSON(2<<10), true)
	loadConfig([]byte("enabled: true\nhosts: [\"api.cline.bot\"]\n"))
	b.SetBytes(int64(len(payload)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, errHook := handleResponseNormalizeBefore(payload); errHook != nil {
			b.Fatalf("hook: %v", errHook)
		}
	}
}

// 命中渠道标记：多一次 JSON 解码与渠道字段提取。
func BenchmarkNormalizeHookWithChannel(b *testing.B) {
	body := channelJSON(4 << 10)
	payload := hookPayload(b, 64<<10, body, false)
	loadConfig([]byte("enabled: true\nhosts: [\"api.cline.bot\"]\n"))
	b.SetBytes(int64(len(payload)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		currentIdentities().remember(&requestIdentity{Hash: "bench", Model: "deepseek-flash"})
		if _, errHook := handleResponseNormalizeBefore(payload); errHook != nil {
			b.Fatalf("hook: %v", errHook)
		}
	}
}

// 落盘前的序列化（sinkWrite 里唯一在请求路径上的部分，入队后由后台 goroutine 写文件）。
func BenchmarkEventMarshal(b *testing.B) {
	e := event{
		Schema: 1, EventID: "evt-0123456789abcdef", PluginVersion: "0.1.0", Timestamp: time.Now(),
		Provider: "openai-compatible-cline", BaseURL: "https://api.cline.bot/api/v1", Host: "api.cline.bot",
		Model: "deepseek-flash", ModelAlias: "cline-pass/deepseek-v4.1-flash",
		APIKey: "sk-TESTKEY00000000000000001", SessionID: "sess-4f2a1c9b7d3e5a6081726354a1b2c3d4",
		AuthIndex: "0000000000000001", AuthType: "apikey",
		LatencyMS: 71800, TTFTMS: 2870, TokensPerSecond: 28.19, TokensPerSecondAfterTT: 29.36,
		InputTokens: 428070, OutputTokens: 2020, ReasoningTokens: 194, TotalTokens: 430090,
		CachedTokens: 427780, PromptCacheHitTokens: 0, PromptCacheMissTokens: 33,
		SystemFingerprint: "fp_fixture0000000000000000000001",
		FinalProvider: "deepseek", ResolvedProvider: "deepseek",
		CanonicalSlug: "deepseek/deepseek-v4.1-flash", OriginalModelID: "deepseek/deepseek-v4.1-flash",
		ModelAttemptCount: 1, TotalProviderAttemptCount: 1,
		Cost: "0.0000447", InputCost: "0.0000099", OutputCost: "0.0000348",
		GenerationID: "gen_FIXTURE0000000000000000008",
		ClientProtocol: "openai-response", UpstreamProtocol: "openai", Stream: true,
		ReasoningEffort: "high", ServiceTier: "default", PlanningReasoningLen: 214,
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		encoded, errMarshal := json.Marshal(&e)
		if errMarshal != nil {
			b.Fatalf("marshal: %v", errMarshal)
		}
		if len(encoded) == 0 {
			b.Fatal("empty")
		}
	}
}

func sizeName(size int) string {
	switch {
	case size >= 1<<20:
		return "1MB"
	case size >= 256<<10:
		return "256KB"
	case size >= 64<<10:
		return "64KB"
	default:
		return "8KB"
	}
}
