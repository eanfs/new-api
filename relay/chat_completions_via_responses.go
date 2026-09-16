package relay

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/relay/channel"
	openaichannel "github.com/QuantumNous/new-api/relay/channel/openai"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"

	"github.com/gin-gonic/gin"
	"github.com/samber/lo"
)

func applySystemPromptIfNeeded(c *gin.Context, info *relaycommon.RelayInfo, request *dto.GeneralOpenAIRequest) {
	if info == nil || request == nil {
		return
	}
	if info.ChannelSetting.SystemPrompt == "" {
		return
	}

	systemRole := request.GetSystemRoleName()

	// A Kimi K3 dynamic tool loading message ({"role":"system","tools":[...]})
	// declares tools rather than a system prompt and must never receive content.
	containSystemPrompt := false
	for _, message := range request.Messages {
		if message.Role == systemRole && len(message.Tools) == 0 {
			containSystemPrompt = true
			break
		}
	}
	if !containSystemPrompt {
		systemMessage := dto.Message{
			Role:    systemRole,
			Content: info.ChannelSetting.SystemPrompt,
		}
		request.Messages = append([]dto.Message{systemMessage}, request.Messages...)
		return
	}

	if !info.ChannelSetting.SystemPromptOverride {
		return
	}

	common.SetContextKey(c, constant.ContextKeySystemPromptOverride, true)
	for i, message := range request.Messages {
		if message.Role != systemRole || len(message.Tools) > 0 {
			continue
		}
		if message.IsStringContent() {
			request.Messages[i].SetStringContent(info.ChannelSetting.SystemPrompt + "\n" + message.StringContent())
			return
		}
		contents := message.ParseContent()
		contents = append([]dto.MediaContent{
			{
				Type: dto.ContentTypeText,
				Text: info.ChannelSetting.SystemPrompt,
			},
		}, contents...)
		request.Messages[i].Content = contents
		return
	}
}

func textRequestViaResponses(c *gin.Context, info *relaycommon.RelayInfo, adaptor channel.Adaptor, request any) (*dto.Usage, *types.NewAPIError) {
	paramOverrideApplied := false
	if chatRequest, ok := request.(*dto.GeneralOpenAIRequest); ok {
		chatJSON, err := common.Marshal(chatRequest)
		if err != nil {
			return nil, types.NewError(err, types.ErrorCodeConvertRequestFailed, types.ErrOptionWithSkipRetry())
		}

		chatJSON, err = relaycommon.RemoveDisabledFields(chatJSON, info.ChannelOtherSettings, info.ChannelSetting.PassThroughBodyEnabled)
		if err != nil {
			return nil, types.NewError(err, types.ErrorCodeConvertRequestFailed, types.ErrOptionWithSkipRetry())
		}

		if len(info.ParamOverride) > 0 {
			chatJSON, err = relaycommon.ApplyParamOverrideWithRelayInfo(chatJSON, info)
			if err != nil {
				return nil, newAPIErrorFromParamOverride(err)
			}
			paramOverrideApplied = true
		}

		var overriddenChatReq dto.GeneralOpenAIRequest
		if err := common.Unmarshal(chatJSON, &overriddenChatReq); err != nil {
			return nil, types.NewError(err, types.ErrorCodeChannelParamOverrideInvalid, types.ErrOptionWithSkipRetry())
		}
		request = &overriddenChatReq
	}

	result, err := service.ConvertRequest(c, info, types.RelayFormatOpenAIResponses, request)
	if err != nil {
		return nil, types.NewErrorWithStatusCode(err, types.ErrorCodeInvalidRequest, http.StatusBadRequest, types.ErrOptionWithSkipRetry())
	}
	responsesReq, ok := result.Value.(*dto.OpenAIResponsesRequest)
	if !ok {
		return nil, types.NewError(fmt.Errorf("expected OpenAI responses request, got %T", result.Value), types.ErrorCodeConvertRequestFailed, types.ErrOptionWithSkipRetry())
	}
	return relayResponsesRequest(c, info, adaptor, responsesReq, paramOverrideApplied)
}

func relayResponsesRequest(c *gin.Context, info *relaycommon.RelayInfo, adaptor channel.Adaptor, responsesReq *dto.OpenAIResponsesRequest, paramOverrideApplied bool) (*dto.Usage, *types.NewAPIError) {
	savedRelayMode := info.RelayMode
	savedRequestURLPath := info.RequestURLPath
	defer func() {
		info.RelayMode = savedRelayMode
		info.RequestURLPath = savedRequestURLPath
	}()

	info.RelayMode = relayconstant.RelayModeResponses
	info.RequestURLPath = "/v1/responses"

	// Codex 上游（chatgpt.com/backend-api/codex/responses）只接受流式请求，
	// 非流式会被直接拒为 400 "Stream must be set to true"。客户端要非流式时，
	// 强制上游走流式，响应再由下方 buffered handler 聚合成完整 JSON 返回给客户端。
	// info.IsStream 保持不变，以免把 SSE 直接透传给期望 JSON 的客户端。
	if info.ChannelType == constant.ChannelTypeCodex && !lo.FromPtrOr(responsesReq.Stream, false) {
		responsesReq.Stream = lo.ToPtr(true)
	}

	convertedRequest, err := adaptor.ConvertOpenAIResponsesRequest(c, info, *responsesReq)
	if err != nil {
		return nil, newConvertRequestFailedError(c, info, err)
	}
	relaycommon.AppendRequestConversionFromRequest(info, convertedRequest)

	jsonData, err := common.Marshal(convertedRequest)
	if err != nil {
		return nil, types.NewError(err, types.ErrorCodeConvertRequestFailed, types.ErrOptionWithSkipRetry())
	}

	jsonData, err = relaycommon.RemoveDisabledFields(jsonData, info.ChannelOtherSettings, info.ChannelSetting.PassThroughBodyEnabled)
	if err != nil {
		return nil, types.NewError(err, types.ErrorCodeConvertRequestFailed, types.ErrOptionWithSkipRetry())
	}
	if !paramOverrideApplied && len(info.ParamOverride) > 0 {
		jsonData, err = relaycommon.ApplyParamOverrideWithRelayInfo(jsonData, info)
		if err != nil {
			return nil, newAPIErrorFromParamOverride(err)
		}
	}

	body, closer, err := relaycommon.NewOutboundJSONBody(jsonData)
	if err != nil {
		return nil, types.NewError(err, types.ErrorCodeConvertRequestFailed, types.ErrOptionWithSkipRetry())
	}
	defer closer.Close()
	jsonData = nil
	var requestBody io.Reader = body

	var httpResp *http.Response
	resp, err := adaptor.DoRequest(c, info, requestBody)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeDoRequestFailed, http.StatusInternalServerError)
	}
	if resp == nil {
		return nil, types.NewOpenAIError(nil, types.ErrorCodeBadResponse, http.StatusInternalServerError)
	}

	statusCodeMappingStr := c.GetString("status_code_mapping")

	httpResp = resp.(*http.Response)
	clientStream := info.IsStream
	if httpResp.StatusCode != http.StatusOK {
		newApiErr := service.RelayErrorHandler(c.Request.Context(), httpResp, false)
		service.ResetStatusCode(newApiErr, statusCodeMappingStr)
		return nil, newApiErr
	}
	upstreamStream := responsesUpstreamIsStream(httpResp)
	info.IsStream = clientStream || upstreamStream

	if upstreamStream && clientStream {
		usage, newApiErr := openaichannel.OaiResponsesToChatStreamHandler(c, info, httpResp)
		if newApiErr != nil {
			service.ResetStatusCode(newApiErr, statusCodeMappingStr)
			return nil, newApiErr
		}
		return usage, nil
	}
	if upstreamStream {
		info.IsStream = false
		usage, newApiErr := openaichannel.OaiResponsesToChatBufferedStreamHandler(c, info, httpResp)
		if newApiErr != nil {
			service.ResetStatusCode(newApiErr, statusCodeMappingStr)
			return nil, newApiErr
		}
		return usage, nil
	}

	usage, newApiErr := openaichannel.OaiResponsesToChatHandler(c, info, httpResp)
	if newApiErr != nil {
		service.ResetStatusCode(newApiErr, statusCodeMappingStr)
		return nil, newApiErr
	}
	return usage, nil
}

func isResponsesEventStreamContentType(contentType string) bool {
	return strings.Contains(strings.ToLower(contentType), "text/event-stream")
}

// sseBodySniffLen 覆盖最长的 SSE 行前缀 "event:"。
const sseBodySniffLen = 6

type peekedBody struct {
	io.Reader
	io.Closer
}

// responsesUpstreamIsStream 判断上游 Responses 响应是否为 SSE 事件流。
//
// Content-Type 不可信：Codex 的 chatgpt.com/backend-api/codex/responses 返回 SSE
// 时不带该响应头，仅凭响应头判断会把事件流当成 JSON 解析，首字符 'e'（event:）
// 直接触发 "invalid character 'e' looking for beginning of value"。
// 因此响应头缺失或不匹配时，改为嗅探响应体首字节；被读取的字节会放回 Body，
// 后续 handler 仍能拿到完整响应。
func responsesUpstreamIsStream(resp *http.Response) bool {
	if resp == nil {
		return false
	}
	if isResponsesEventStreamContentType(resp.Header.Get("Content-Type")) {
		return true
	}
	if resp.Body == nil {
		return false
	}

	original := resp.Body
	buffered := bufio.NewReader(original)
	resp.Body = peekedBody{Reader: buffered, Closer: original}

	prefix, err := buffered.Peek(sseBodySniffLen)
	if len(prefix) == 0 && err != nil {
		return false
	}
	return bytes.HasPrefix(prefix, []byte("data:")) || bytes.HasPrefix(prefix, []byte("event:"))
}
