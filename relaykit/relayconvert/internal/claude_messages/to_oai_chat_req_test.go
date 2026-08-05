package claudemessages

import (
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/relaykit/dto"
	kitutil "github.com/QuantumNous/new-api/relaykit/relayconvert/kitutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// wantMessage 描述转换后单条 OpenAI 消息的期望：reasoning 为 nil 表示期望字段缺失。
type wantMessage struct {
	role      string
	reasoning *string
	hasTools  bool
}

func ptr(s string) *string { return &s }

// 思考模式下，带 tool_calls 的 assistant 消息必须回传 reasoning_content，
// 否则 DeepSeek 等上游会拒绝整个请求（400），且与该字段在历史中的位置无关。
// 有 thinking 块则透传真实文本，没有则补空串占位；无 tool_calls 的消息不碰，
// 因为官方明确那种情况下传了也会被忽略。
func TestClaudeMessagesRequestToOpenAIChatReasoningContentForToolCalls(t *testing.T) {
	tests := []struct {
		name         string
		body         string
		wantMessages []wantMessage
	}{
		{
			name: "thinking 与 tool_use 共存时透传思考文本",
			body: `{"model":"deepseek-v4","messages":[
				{"role":"assistant","content":[
					{"type":"thinking","thinking":"先查目录再决定","signature":"sig-abc"},
					{"type":"tool_use","id":"toolu_1","name":"ls","input":{"path":"."}}
				]}
			]}`,
			wantMessages: []wantMessage{
				{role: "assistant", reasoning: ptr("先查目录再决定"), hasTools: true},
			},
		},
		{
			name: "thinking 块存在但文本为空时带空的 reasoning_content",
			body: `{"model":"deepseek-v4","messages":[
				{"role":"assistant","content":[
					{"type":"thinking","thinking":"","signature":"sig-abc"},
					{"type":"tool_use","id":"toolu_1","name":"ls","input":{"path":"."}}
				]}
			]}`,
			wantMessages: []wantMessage{
				{role: "assistant", reasoning: ptr(""), hasTools: true},
			},
		},
		{
			name: "完全没有 thinking 块时补空串占位",
			body: `{"model":"deepseek-v4","messages":[
				{"role":"assistant","content":[
					{"type":"text","text":"直接调用工具"},
					{"type":"tool_use","id":"toolu_1","name":"ls","input":{"path":"."}}
				]}
			]}`,
			wantMessages: []wantMessage{
				{role: "assistant", reasoning: ptr(""), hasTools: true},
			},
		},
		{
			name: "redacted_thinking 密文无法解出明文，走空串占位",
			body: `{"model":"deepseek-v4","messages":[
				{"role":"assistant","content":[
					{"type":"redacted_thinking","data":"EncryptedPayload=="},
					{"type":"tool_use","id":"toolu_1","name":"ls","input":{"path":"."}}
				]}
			]}`,
			wantMessages: []wantMessage{
				{role: "assistant", reasoning: ptr(""), hasTools: true},
			},
		},
		{
			name: "多个 thinking 块按顺序拼接",
			body: `{"model":"deepseek-v4","messages":[
				{"role":"assistant","content":[
					{"type":"thinking","thinking":"第一段"},
					{"type":"redacted_thinking","data":"EncryptedPayload=="},
					{"type":"thinking","thinking":"第二段"},
					{"type":"tool_use","id":"toolu_1","name":"ls","input":{"path":"."}}
				]}
			]}`,
			wantMessages: []wantMessage{
				{role: "assistant", reasoning: ptr("第一段第二段"), hasTools: true},
			},
		},
		{
			name: "无 tool_calls 的 assistant 消息不带 reasoning_content",
			body: `{"model":"deepseek-v4","messages":[
				{"role":"assistant","content":[
					{"type":"thinking","thinking":"推理过程"},
					{"type":"text","text":"结论是这样"}
				]}
			]}`,
			wantMessages: []wantMessage{
				{role: "assistant", reasoning: nil},
			},
		},
		{
			name: "非 assistant 角色即便带 tool_use 也不得挂 reasoning_content",
			body: `{"model":"deepseek-v4","messages":[
				{"role":"user","content":[
					{"type":"thinking","thinking":"不该出现在这里"},
					{"type":"tool_use","id":"toolu_1","name":"ls","input":{"path":"."}}
				]}
			]}`,
			wantMessages: []wantMessage{
				{role: "user", reasoning: nil, hasTools: true},
			},
		},
		{
			name: "user 消息里的 tool_result 与 thinking 都不得污染 tool 消息",
			body: `{"model":"deepseek-v4","messages":[
				{"role":"user","content":[
					{"type":"thinking","thinking":"不该出现在这里"},
					{"type":"tool_result","tool_use_id":"toolu_1","content":"file.txt"},
					{"type":"text","text":"继续"}
				]}
			]}`,
			wantMessages: []wantMessage{
				{role: "tool", reasoning: nil},
				{role: "user", reasoning: nil},
			},
		},
		{
			name: "完整的思考模式工具调用往返",
			body: `{"model":"deepseek-v4","messages":[
				{"role":"user","content":"列一下当前目录"},
				{"role":"assistant","content":[
					{"type":"thinking","thinking":"需要调用 ls","signature":"sig-abc"},
					{"type":"tool_use","id":"toolu_1","name":"ls","input":{"path":"."}}
				]},
				{"role":"user","content":[
					{"type":"tool_result","tool_use_id":"toolu_1","content":"file.txt"}
				]}
			]}`,
			wantMessages: []wantMessage{
				{role: "user", reasoning: nil},
				{role: "assistant", reasoning: ptr("需要调用 ls"), hasTools: true},
				{role: "tool", reasoning: nil},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var claudeRequest dto.ClaudeRequest
			require.NoError(t, kitutil.UnmarshalJsonStr(tc.body, &claudeRequest))

			openAIRequest, err := ClaudeMessagesRequestToOpenAIChat(claudeRequest, nil)
			require.NoError(t, err)
			require.Len(t, openAIRequest.Messages, len(tc.wantMessages))

			for i, want := range tc.wantMessages {
				got := openAIRequest.Messages[i]
				assert.Equal(t, want.role, got.Role, "第 %d 条消息 role 不符", i)
				assert.Equal(t, want.hasTools, len(got.ToolCalls) > 0, "第 %d 条消息 tool_calls 不符", i)

				if want.reasoning == nil {
					assert.Nil(t, got.ReasoningContent, "第 %d 条消息不应带 reasoning_content", i)
					continue
				}
				require.NotNil(t, got.ReasoningContent, "第 %d 条消息应带 reasoning_content", i)
				assert.Equal(t, *want.reasoning, *got.ReasoningContent, "第 %d 条消息 reasoning_content 不符", i)
			}
		})
	}
}

// reasoning_content 是 *string + omitempty：空串指针必须真的序列化出去，
// 否则占位那一格会退化成字段缺失，重新触发上游 400。
func TestClaudeMessagesRequestToOpenAIChatMarshalsEmptyReasoningContent(t *testing.T) {
	body := `{"model":"deepseek-v4","messages":[
		{"role":"assistant","content":[
			{"type":"text","text":"直接调用工具"},
			{"type":"tool_use","id":"toolu_1","name":"ls","input":{"path":"."}}
		]}
	]}`

	var claudeRequest dto.ClaudeRequest
	require.NoError(t, kitutil.UnmarshalJsonStr(body, &claudeRequest))

	openAIRequest, err := ClaudeMessagesRequestToOpenAIChat(claudeRequest, nil)
	require.NoError(t, err)

	encoded, err := kitutil.Marshal(openAIRequest)
	require.NoError(t, err)
	assert.Contains(t, string(encoded), `"reasoning_content":""`)
}

// 不含工具调用的既有流量必须字节级不变，避免给严格上游多送一个未知字段。
func TestClaudeMessagesRequestToOpenAIChatOmitsReasoningContentWithoutToolCalls(t *testing.T) {
	body := `{"model":"deepseek-v4","messages":[
		{"role":"assistant","content":[
			{"type":"thinking","thinking":"推理过程"},
			{"type":"text","text":"直接回答"}
		]}
	]}`

	var claudeRequest dto.ClaudeRequest
	require.NoError(t, kitutil.UnmarshalJsonStr(body, &claudeRequest))

	openAIRequest, err := ClaudeMessagesRequestToOpenAIChat(claudeRequest, nil)
	require.NoError(t, err)

	encoded, err := kitutil.Marshal(openAIRequest)
	require.NoError(t, err)
	assert.False(t, strings.Contains(string(encoded), "reasoning_content"))
}
