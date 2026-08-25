package relay

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/relay/channel/codex"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newResp(contentType, body string) *http.Response {
	h := http.Header{}
	if contentType != "" {
		h.Set("Content-Type", contentType)
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     h,
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

const codexSSEBody = "event: response.created\n" +
	`data: {"type":"response.created","response":{"id":"resp_1"}}` + "\n\n"

func TestResponsesUpstreamIsStream(t *testing.T) {
	cases := []struct {
		name        string
		contentType string
		body        string
		want        bool
	}{
		{"标准 SSE 响应头", "text/event-stream", codexSSEBody, true},
		{"SSE 响应头带 charset", "text/event-stream; charset=utf-8", codexSSEBody, true},
		// Codex 的 backend-api/codex/responses 返回 SSE 却不带 Content-Type，
		// 这是线上 500 "invalid character 'e'" 的直接成因。
		{"无 Content-Type 但体是 SSE(event: 开头)", "", codexSSEBody, true},
		{"无 Content-Type 但体是 SSE(data: 开头)", "", "data: {\"type\":\"x\"}\n\n", true},
		{"无 Content-Type 且体是 JSON", "", `{"id":"resp_1","object":"response"}`, false},
		{"JSON 响应头", "application/json", `{"id":"resp_1"}`, false},
		{"空响应体", "", "", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := newResp(tc.contentType, tc.body)
			t.Cleanup(func() { require.NoError(t, resp.Body.Close()) })

			assert.Equal(t, tc.want, responsesUpstreamIsStream(resp))
		})
	}
}

// 嗅探不能吃掉响应体，否则后续 handler 会丢事件。
func TestResponsesUpstreamIsStreamPreservesBody(t *testing.T) {
	resp := newResp("", codexSSEBody)
	t.Cleanup(func() { require.NoError(t, resp.Body.Close()) })

	require.True(t, responsesUpstreamIsStream(resp))

	got, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, codexSSEBody, string(got))
}

// 嗅探替换 Body 后，原始 Closer 仍须被调用，避免连接泄漏。
func TestResponsesUpstreamIsStreamClosesOriginalBody(t *testing.T) {
	closed := false
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{},
		Body: struct {
			io.Reader
			io.Closer
		}{
			Reader: strings.NewReader(codexSSEBody),
			Closer: closerFunc(func() error { closed = true; return nil }),
		},
	}

	responsesUpstreamIsStream(resp)
	require.NoError(t, resp.Body.Close())
	assert.True(t, closed)
}

func TestChatCompletionsViaResponsesCodexUsesUpstreamStream(t *testing.T) {
	oldMode := gin.Mode()
	gin.SetMode(gin.TestMode)
	t.Cleanup(func() { gin.SetMode(oldMode) })
	oldTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = oldTimeout })

	streaming := true
	tests := []struct {
		name            string
		clientStream    *bool
		expectClientSSE bool
	}{
		{name: "buffers SSE when client omits stream"},
		{name: "streams SSE for streaming client", clientStream: &streaming, expectClientSSE: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			type upstreamRequestResult struct {
				stream *bool
				err    error
			}
			upstreamRequest := make(chan upstreamRequestResult, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var payload struct {
					Stream *bool `json:"stream"`
				}
				err := common.DecodeJson(r.Body, &payload)
				upstreamRequest <- upstreamRequestResult{stream: payload.Stream, err: err}

				w.Header()["Content-Type"] = nil
				_, _ = io.WriteString(w, strings.Join([]string{
					`event: response.created`,
					`data: {"type":"response.created","response":{"id":"resp_1","model":"gpt-5.6-luna"}}`,
					``,
					`data: {"type":"response.output_text.delta","delta":"response text"}`,
					`data: {"type":"response.done","response":{"model":"gpt-5.6-luna","status":"completed","usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}}`,
					`data: [DONE]`,
					``,
				}, "\n"))
			}))
			t.Cleanup(server.Close)

			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			c.Request.Header.Set("Content-Type", "application/json")
			c.Set(common.RequestIdKey, "codex-upstream-stream-test")

			request := &dto.GeneralOpenAIRequest{
				Model:  "gpt-5.6-luna",
				Stream: tt.clientStream,
				Messages: []dto.Message{
					{Role: "user", Content: "Hello"},
				},
			}
			info := &relaycommon.RelayInfo{
				ChannelMeta: &relaycommon.ChannelMeta{
					ChannelType:       constant.ChannelTypeCodex,
					ChannelBaseUrl:    server.URL,
					ApiKey:            `{"access_token":"test-token","account_id":"test-account"}`,
					UpstreamModelName: "gpt-5.6-luna",
				},
				OriginModelName:    "gpt-5.6-luna",
				RelayMode:          relayconstant.RelayModeChatCompletions,
				RequestURLPath:     "/v1/chat/completions",
				RelayFormat:        types.RelayFormatOpenAI,
				IsStream:           tt.expectClientSSE,
				ShouldIncludeUsage: true,
			}

			usage, newAPIError := chatCompletionsViaResponses(c, info, &codex.Adaptor{}, request)
			require.Nil(t, newAPIError)
			require.NotNil(t, usage)
			assert.Equal(t, 3, usage.TotalTokens)
			assert.Equal(t, tt.expectClientSSE, info.IsStream)

			result := <-upstreamRequest
			require.NoError(t, result.err)
			require.NotNil(t, result.stream)
			assert.True(t, *result.stream)

			responseBody := recorder.Body.String()
			assert.Contains(t, responseBody, `"content":"response text"`)
			if tt.expectClientSSE {
				assert.Equal(t, "text/event-stream", recorder.Header().Get("Content-Type"))
				assert.Contains(t, responseBody, "data:")
				assert.Contains(t, responseBody, `"object":"chat.completion.chunk"`)
				return
			}
			assert.NotContains(t, responseBody, "data:")
			assert.Contains(t, responseBody, `"object":"chat.completion"`)
		})
	}
}

type closerFunc func() error

func (f closerFunc) Close() error { return f() }
