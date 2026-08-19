package oaichat

import (
	"testing"

	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/relayconvert/convmeta"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// streamToolCallChunk 构造一个只带工具调用增量的流式帧。
func streamToolCallChunk(model string) *dto.ChatCompletionsStreamResponse {
	return &dto.ChatCompletionsStreamResponse{
		Id: "as-x", Model: model,
		Choices: []dto.ChatCompletionsStreamResponseChoice{
			{Delta: dto.ChatCompletionsStreamResponseChoiceDelta{
				ToolCalls: []dto.ToolCallResponse{{
					Index: ptr(0), ID: "call_1", Type: "function",
					Function: dto.FunctionResponse{Name: "Read", Arguments: `{"p":"a"}`},
				}},
			}},
		},
	}
}

// claudeTerminalOf 提取一组 Claude 事件里的收尾信息。
func claudeTerminalOf(responses []*dto.ClaudeResponse) (stopReason string, sawMessageStop bool) {
	for _, r := range responses {
		if r.Type == "message_delta" && r.Delta != nil && r.Delta.StopReason != nil {
			stopReason = *r.Delta.StopReason
		}
		if r.Type == "message_stop" {
			sawMessageStop = true
		}
	}
	return stopReason, sawMessageStop
}

// 真实上游形态（deepseek-v4-flash-0731）：
//
//	帧A {"choices":[{"finish_reason":"tool_calls","index":0,"delta":{}}]}
//	帧B {"choices":[{"index":0,"delta":{}}],"usage":{...}}
//
// 帧A 因缺 usage 把收尾推迟到下一帧，而帧B 的 choices 非空、自身无 finish_reason。
// 修复前两个收尾事件全部丢失，客户端拿到完整 tool_use 却收不到 message_stop，对话卡住。
func TestStreamToolCallsFinishThenUsageChunkWithNonEmptyChoices(t *testing.T) {
	info := &convmeta.Values{}

	info.SendResponseCount = 2
	require.NotEmpty(t, StreamResponseOpenAI2Claude(streamToolCallChunk("deepseek-v4-flash-0731"), info))

	// 帧A：finish_reason 到达，无 usage
	info.SendResponseCount = 3
	StreamResponseOpenAI2Claude(&dto.ChatCompletionsStreamResponse{
		Id: "as-x", Model: "deepseek-v4-flash-0731",
		Choices: []dto.ChatCompletionsStreamResponseChoice{
			{FinishReason: ptr("tool_calls"), Delta: dto.ChatCompletionsStreamResponseChoiceDelta{}},
		},
	}, info)

	// 帧B：choices 非空（delta 为空）+ usage
	info.SendResponseCount = 4
	final := StreamResponseOpenAI2Claude(&dto.ChatCompletionsStreamResponse{
		Id: "as-x", Model: "deepseek-v4-flash-0731",
		Choices: []dto.ChatCompletionsStreamResponseChoice{
			{Delta: dto.ChatCompletionsStreamResponseChoiceDelta{}},
		},
		Usage: &dto.Usage{PromptTokens: 126011, CompletionTokens: 238, TotalTokens: 126249},
	}, info)

	stopReason, sawMessageStop := claudeTerminalOf(final)
	assert.True(t, sawMessageStop, "带非空 choices 的 usage 帧必须触发 message_stop")
	assert.Equal(t, "tool_use", stopReason)
}

// 对照另一种上游形态（mimo-v2.5）：收尾 usage 帧的 choices 为空数组。
// 这条路径原本就能收尾，用于确认修复没有改变它的行为。
func TestStreamToolCallsFinishThenUsageChunkWithEmptyChoices(t *testing.T) {
	info := &convmeta.Values{}

	info.SendResponseCount = 2
	StreamResponseOpenAI2Claude(streamToolCallChunk("mimo-v2.5"), info)

	info.SendResponseCount = 3
	StreamResponseOpenAI2Claude(&dto.ChatCompletionsStreamResponse{
		Id: "as-y", Model: "mimo-v2.5",
		Choices: []dto.ChatCompletionsStreamResponseChoice{
			{FinishReason: ptr("tool_calls"), Delta: dto.ChatCompletionsStreamResponseChoiceDelta{}},
		},
	}, info)

	info.SendResponseCount = 4
	final := StreamResponseOpenAI2Claude(&dto.ChatCompletionsStreamResponse{
		Id: "as-y", Model: "mimo-v2.5",
		Choices: []dto.ChatCompletionsStreamResponseChoice{},
		Usage:   &dto.Usage{PromptTokens: 5, CompletionTokens: 2, TotalTokens: 7},
	}, info)

	stopReason, sawMessageStop := claudeTerminalOf(final)
	assert.True(t, sawMessageStop)
	assert.Equal(t, "tool_use", stopReason)
}

// 上游全程不发 finish_reason（日志实测存在）：收尾靠 Finalize 兜底，
// 此时不能因 FinishReason 为空就兜底成 end_turn，本轮已产出 tool_use 块。
func TestFinalizeAfterToolCallsWithoutFinishReason(t *testing.T) {
	info := &convmeta.Values{}
	info.SendResponseCount = 2
	StreamResponseOpenAI2Claude(streamToolCallChunk("mimo-v2.5"), info)

	stopReason, sawMessageStop := claudeTerminalOf(FinalizeStreamResponseOpenAI2Claude(info))
	assert.True(t, sawMessageStop)
	assert.Equal(t, "tool_use", stopReason)
}

// Finalize 幂等：流已正常收尾后再调用（EOF 兜底路径）不得重复发送收尾事件。
func TestFinalizeIsIdempotentAfterNormalClose(t *testing.T) {
	info := &convmeta.Values{}
	info.SendResponseCount = 2
	StreamResponseOpenAI2Claude(streamToolCallChunk("mimo-v2.5"), info)

	info.SendResponseCount = 3
	normal := StreamResponseOpenAI2Claude(&dto.ChatCompletionsStreamResponse{
		Id: "as-z", Model: "mimo-v2.5",
		Choices: []dto.ChatCompletionsStreamResponseChoice{
			{FinishReason: ptr("tool_calls"), Delta: dto.ChatCompletionsStreamResponseChoiceDelta{}},
		},
		Usage: &dto.Usage{PromptTokens: 5, CompletionTokens: 2, TotalTokens: 7},
	}, info)
	_, sawMessageStop := claudeTerminalOf(normal)
	require.True(t, sawMessageStop, "同帧带 usage 应正常收尾")

	assert.Empty(t, FinalizeStreamResponseOpenAI2Claude(info), "已收尾的流不应再补发事件")
}

// 非流式：上游给出自相矛盾的 stop + tool_calls，应按 content 实际内容纠正为 tool_use。
func TestResponseOpenAI2ClaudeStopWithToolCallsBecomesToolUse(t *testing.T) {
	msg := dto.Message{Role: "assistant"}
	msg.SetToolCalls([]dto.ToolCallRequest{{
		ID: "call_1", Type: "function",
		Function: dto.FunctionRequest{Name: "Read", Arguments: `{"p":"a"}`},
	}})
	claude := ResponseOpenAI2Claude(&dto.OpenAITextResponse{
		Id: "resp_1", Model: "gpt-test",
		Choices: []dto.OpenAITextResponseChoice{{FinishReason: "stop", Message: msg}},
	}, &convmeta.Values{})

	assert.Equal(t, "tool_use", claude.StopReason)
}

// 无工具调用的普通一轮不受影响：stop 仍映射为 end_turn。
func TestResponseOpenAI2ClaudeStopWithoutToolCallsStaysEndTurn(t *testing.T) {
	msg := dto.Message{Role: "assistant"}
	msg.SetStringContent("hello")
	claude := ResponseOpenAI2Claude(&dto.OpenAITextResponse{
		Id: "resp_2", Model: "gpt-test",
		Choices: []dto.OpenAITextResponseChoice{{FinishReason: "stop", Message: msg}},
	}, &convmeta.Values{})

	assert.Equal(t, "end_turn", claude.StopReason)
}
