package relay

import (
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	openaichannel "github.com/QuantumNous/new-api/relay/channel/openai"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relaykit/dto"
	relaytypes "github.com/QuantumNous/new-api/relaykit/types"
	hosttypes "github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIsResponsesEventStreamContentType(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		want        bool
	}{
		{name: "plain", contentType: "text/event-stream", want: true},
		{name: "mixed case with charset", contentType: "Text/Event-Stream; charset=utf-8", want: true},
		{name: "json", contentType: "application/json", want: false},
		{name: "empty", contentType: "", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, isResponsesEventStreamContentType(tt.contentType))
		})
	}
}

func TestRecalcQuotaFromRatiosIgnoresInvalidMultipliers(t *testing.T) {
	info := &relaycommon.RelayInfo{
		PriceData: hosttypes.PriceData{
			Quota: 100,
		},
	}
	info.PriceData.AddOtherRatio("duration", 2)

	quota, ok := recalcQuotaFromRatios(info, map[string]float64{
		"duration": 3,
		"zero":     0,
		"negative": -1,
		"nan":      math.NaN(),
		"inf":      math.Inf(1),
	})

	require.True(t, ok)
	assert.Equal(t, 150, quota)
	assert.True(t, info.PriceData.HasOtherRatio("duration"))
}

func TestRecalcQuotaFromRatiosRejectsAllInvalidAdjustedRatios(t *testing.T) {
	info := &relaycommon.RelayInfo{
		PriceData: hosttypes.PriceData{
			Quota: 100,
		},
	}
	info.PriceData.AddOtherRatio("duration", 2)

	quota, ok := recalcQuotaFromRatios(info, map[string]float64{
		"zero":     0,
		"negative": -1,
		"nan":      math.NaN(),
		"inf":      math.Inf(1),
	})

	require.False(t, ok)
	assert.Equal(t, 0, quota)
	assert.True(t, info.PriceData.HasOtherRatio("duration"))
}

func TestTextRequestViaResponsesConvertsClaudeToResponses(t *testing.T) {
	type capturedRequest struct {
		path string
		body []byte
	}
	captured := make(chan capturedRequest, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		captured <- capturedRequest{path: r.URL.Path, body: body}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"resp_1",
			"object":"response",
			"status":"completed",
			"model":"gpt-5.6-sol",
			"output":[{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],
			"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}
		}`))
	}))
	defer server.Close()

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	c.Request.Header.Set("Content-Type", "application/json")

	info := &relaycommon.RelayInfo{
		RelayMode:              relayconstant.RelayModeChatCompletions,
		RelayFormat:            relaytypes.RelayFormatClaude,
		OriginModelName:        "gpt-5.6-sol",
		RequestConversionChain: []relaytypes.RelayFormat{relaytypes.RelayFormatClaude},
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelType:       constant.ChannelTypeOpenAI,
			ChannelBaseUrl:    server.URL,
			ApiKey:            "test-key",
			UpstreamModelName: "gpt-5.6-sol",
		},
	}
	adaptor := &openaichannel.Adaptor{}
	adaptor.Init(info)
	request := &dto.ClaudeRequest{
		Model:    "gpt-5.6-sol",
		Thinking: &dto.Thinking{Type: "adaptive", Display: "summarized"},
		Messages: []dto.ClaudeMessage{{Role: "user", Content: "hello"}},
	}

	usage, apiErr := textRequestViaResponses(c, info, adaptor, request)

	require.Nil(t, apiErr)
	require.NotNil(t, usage)
	assert.Equal(t, 5, usage.TotalTokens)
	// Claude 入站先归一化成 chat 请求再升级到 Responses，参数覆盖脚本才能按渠道原本的
	// chat/completions 语义生效，故转换链是三段。
	assert.Equal(t, []relaytypes.RelayFormat{relaytypes.RelayFormatClaude, relaytypes.RelayFormatOpenAI, relaytypes.RelayFormatOpenAIResponses}, info.RequestConversionChain)

	upstream := <-captured
	assert.Equal(t, "/v1/responses", upstream.path)
	var upstreamBody map[string]any
	require.NoError(t, common.Unmarshal(upstream.body, &upstreamBody))
	assert.NotContains(t, upstreamBody, "messages")
	reasoning, ok := upstreamBody["reasoning"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "high", reasoning["effort"])
	assert.Equal(t, "detailed", reasoning["summary"])

	var response dto.ClaudeResponse
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
	require.Len(t, response.Content, 1)
	assert.Equal(t, "ok", response.Content[0].GetText())
}

// museEffortOverride 复刻生产上 muse 渠道的两类规则：chat 语义的 reasoning_effort 降档，
// 以及 set_header 注入对话 ID（上游 opencode 要求每个请求带）。后者不作用于请求体，
// 必须随规则应用时机一起搬动，否则协议升级路径上会静默丢头。
func museEffortOverride() map[string]interface{} {
	return map[string]interface{}{
		"operations": []interface{}{
			map[string]interface{}{
				"mode":  "set",
				"path":  "reasoning_effort",
				"value": "minimal",
				"logic": "AND",
				"conditions": []interface{}{
					map[string]interface{}{"mode": "contains", "path": "upstream_model", "value": "muse-spark"},
					map[string]interface{}{"mode": "full", "path": "client_thinking_type", "value": "enabled", "invert": true},
					map[string]interface{}{"mode": "full", "path": "client_thinking_type", "value": "adaptive", "invert": true},
				},
			},
			map[string]interface{}{
				"mode":  "set_header",
				"path":  "X-Opencode-Session",
				"value": "${client_session_id}",
			},
		},
	}
}

func newResponsesUpgradeInfo(baseURL string, format relaytypes.RelayFormat) *relaycommon.RelayInfo {
	return &relaycommon.RelayInfo{
		RelayMode:              relayconstant.RelayModeChatCompletions,
		RelayFormat:            format,
		OriginModelName:        "muse-spark-1.3",
		RequestConversionChain: []relaytypes.RelayFormat{format},
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelType:       constant.ChannelTypeOpenAI,
			ChannelBaseUrl:    baseURL,
			ApiKey:            "test-key",
			UpstreamModelName: "muse-spark-1.3-contributor",
			ParamOverride:     museEffortOverride(),
		},
	}
}

func newResponsesUpgradeServer(t *testing.T, captured chan<- []byte, capturedHeader chan<- string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		capturedHeader <- r.Header.Get("X-Opencode-Session")
		captured <- body
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"resp_1",
			"object":"response",
			"status":"completed",
			"model":"muse-spark-1.3-contributor",
			"output":[{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],
			"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}
		}`))
	}))
}

// 参数覆盖脚本按渠道原本的 chat/completions 语义书写，协议升级到 Responses 是网关单方面的
// 行为。Claude 入站必须和 OpenAI 入站一样先归一化成 chat 请求再套规则，否则 chat 语义的
// reasoning_effort 会原样落到 Responses 请求顶层，被上游判为未知参数直接 400。
func TestTextRequestViaResponsesAppliesParamOverrideOnChatSemantics(t *testing.T) {
	claudeRequest := &dto.ClaudeRequest{
		Model:    "muse-spark-1.3",
		Messages: []dto.ClaudeMessage{{Role: "user", Content: "hello"}},
	}
	openaiRequest := &dto.GeneralOpenAIRequest{
		Model:    "muse-spark-1.3",
		Messages: []dto.Message{{Role: "user", Content: "hello"}},
	}

	tests := []struct {
		name      string
		format    relaytypes.RelayFormat
		path      string
		request   any
		wantChain []relaytypes.RelayFormat
	}{
		{
			name:      "claude input",
			format:    relaytypes.RelayFormatClaude,
			path:      "/v1/messages",
			request:   claudeRequest,
			wantChain: []relaytypes.RelayFormat{relaytypes.RelayFormatClaude, relaytypes.RelayFormatOpenAI, relaytypes.RelayFormatOpenAIResponses},
		},
		{
			name:      "openai input",
			format:    relaytypes.RelayFormatOpenAI,
			path:      "/v1/chat/completions",
			request:   openaiRequest,
			wantChain: []relaytypes.RelayFormat{relaytypes.RelayFormatOpenAI, relaytypes.RelayFormatOpenAIResponses},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			captured := make(chan []byte, 1)
			capturedHeader := make(chan string, 1)
			server := newResponsesUpgradeServer(t, captured, capturedHeader)
			defer server.Close()

			gin.SetMode(gin.TestMode)
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, tt.path, nil)
			c.Request.Header.Set("Content-Type", "application/json")

			info := newResponsesUpgradeInfo(server.URL, tt.format)
			// client_session_id 由 info.Request 推导，两种入站都要挂上；
			// client_thinking_type 则只在 Claude 请求上有值，OpenAI 入站时该条件路径缺失。
			switch req := tt.request.(type) {
			case *dto.ClaudeRequest:
				info.Request = req
			case *dto.GeneralOpenAIRequest:
				info.Request = req
			}
			adaptor := &openaichannel.Adaptor{}
			adaptor.Init(info)

			usage, apiErr := textRequestViaResponses(c, info, adaptor, tt.request)
			require.Nil(t, apiErr)
			require.NotNil(t, usage)
			assert.Equal(t, tt.wantChain, info.RequestConversionChain)

			var upstreamBody map[string]any
			require.NoError(t, common.Unmarshal(<-captured, &upstreamBody))
			assert.NotContains(t, upstreamBody, "reasoning_effort", "chat 语义字段不能出现在 Responses 请求顶层")
			assert.NotContains(t, upstreamBody, "messages")
			// set_header 不作用于请求体，规则应用时机变化不能把它丢掉。
			assert.NotEmpty(t, <-capturedHeader, "set_header 注入的对话 ID 必须出现在出站请求头上")
		})
	}
}

// Claude 入站时 client_thinking_type 可用，降档规则命中后必须体现在 Responses 的
// reasoning.effort 上，而不是被丢弃。
func TestTextRequestViaResponsesFoldsOverriddenEffortIntoReasoning(t *testing.T) {
	captured := make(chan []byte, 1)
	capturedHeader := make(chan string, 1)
	server := newResponsesUpgradeServer(t, captured, capturedHeader)
	defer server.Close()

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	c.Request.Header.Set("Content-Type", "application/json")

	request := &dto.ClaudeRequest{
		Model:    "muse-spark-1.3",
		Messages: []dto.ClaudeMessage{{Role: "user", Content: "hello"}},
	}
	info := newResponsesUpgradeInfo(server.URL, relaytypes.RelayFormatClaude)
	info.Request = request
	adaptor := &openaichannel.Adaptor{}
	adaptor.Init(info)

	_, apiErr := textRequestViaResponses(c, info, adaptor, request)
	require.Nil(t, apiErr)

	var upstreamBody map[string]any
	require.NoError(t, common.Unmarshal(<-captured, &upstreamBody))
	reasoning, ok := upstreamBody["reasoning"].(map[string]any)
	require.True(t, ok, "降档规则命中后应产生 Responses 的 reasoning 对象")
	assert.Equal(t, "minimal", reasoning["effort"])
}
