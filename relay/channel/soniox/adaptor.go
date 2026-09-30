package soniox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/relay/channel"
	"github.com/QuantumNous/new-api/relay/channel/openai"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"

	"github.com/gin-gonic/gin"
)

const (
	ChannelName = "soniox"

	defaultVoice    = "Adrian"
	defaultLanguage = "en"

	// Soniox accepts 0.7-1.3; OpenAI clients send 0.25-4.0.
	minSpeed = 0.7
	maxSpeed = 1.3

	// pcm output is requested as 16-bit mono at this rate so the response
	// duration can be derived from its byte length.
	pcmSampleRate = 24000

	transcriptionPollInterval = time.Second
	transcriptionTimeout      = 15 * time.Minute
	cleanupTimeout            = 15 * time.Second
)

var ModelList = []string{
	"stt-async-v5",
	"tts-rt-v2",
}

// Adaptor bridges the OpenAI audio API onto Soniox. Speech maps to the
// single-request TTS REST endpoint. Transcription is a multi-step async job on
// Soniox (upload, create, poll, fetch), which DoRequest drives to completion
// and hands back to DoResponse as one synthetic response.
type Adaptor struct{}

type ttsRequest struct {
	Model       string   `json:"model"`
	Language    string   `json:"language"`
	Voice       string   `json:"voice"`
	AudioFormat string   `json:"audio_format"`
	Text        string   `json:"text"`
	SampleRate  *int     `json:"sample_rate,omitempty"`
	Speed       *float64 `json:"speed,omitempty"`
}

type transcriptionContext struct {
	Text string `json:"text,omitempty"`
}

type createTranscriptionRequest struct {
	Model         string                `json:"model"`
	FileID        string                `json:"file_id"`
	LanguageHints []string              `json:"language_hints,omitempty"`
	Context       *transcriptionContext `json:"context,omitempty"`
}

type sonioxFile struct {
	ID string `json:"id"`
}

type sonioxTranscription struct {
	ID           string `json:"id"`
	Status       string `json:"status"`
	AudioDurMs   *int64 `json:"audio_duration_ms"`
	ErrorMessage string `json:"error_message"`
}

type sonioxToken struct {
	Text    string `json:"text"`
	StartMs int64  `json:"start_ms"`
	EndMs   int64  `json:"end_ms"`
}

type sonioxTranscript struct {
	Text   string        `json:"text"`
	Tokens []sonioxToken `json:"tokens"`
}

// transcriptionResult is the internal body DoRequest hands to DoResponse.
type transcriptionResult struct {
	Text       string        `json:"text"`
	Tokens     []sonioxToken `json:"tokens"`
	DurationMs int64         `json:"duration_ms"`
}

type transcriptionWord struct {
	Word  string  `json:"word"`
	Start float64 `json:"start"`
	End   float64 `json:"end"`
}

type transcriptionUsage struct {
	Type    string  `json:"type"`
	Seconds float64 `json:"seconds"`
}

func (a *Adaptor) Init(info *relaycommon.RelayInfo) {}

func (a *Adaptor) GetRequestURL(info *relaycommon.RelayInfo) (string, error) {
	if info.RelayMode != relayconstant.RelayModeAudioSpeech {
		return "", fmt.Errorf("unsupported relay mode: %d", info.RelayMode)
	}
	baseURL := info.ChannelBaseUrl
	if baseURL == "" {
		baseURL = constant.GetChannelBaseURL(constant.ChannelTypeSoniox)
	}
	// TTS is served from a sibling host of the API domain (api.soniox.com ->
	// tts-rt.soniox.com, and the same for the regional api.<region>.soniox.com).
	parsed, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil {
		return "", fmt.Errorf("invalid soniox base url: %w", err)
	}
	if host, ok := strings.CutPrefix(parsed.Host, "api."); ok {
		parsed.Host = "tts-rt." + host
	}
	return parsed.String() + "/tts", nil
}

func (a *Adaptor) SetupRequestHeader(c *gin.Context, req *http.Header, info *relaycommon.RelayInfo) error {
	req.Set("Content-Type", "application/json")
	req.Set("Authorization", "Bearer "+info.ApiKey)
	return nil
}

func (a *Adaptor) ConvertAudioRequest(c *gin.Context, info *relaycommon.RelayInfo, request dto.AudioRequest) (io.Reader, error) {
	switch info.RelayMode {
	case relayconstant.RelayModeAudioSpeech:
		return convertSpeechRequest(info, request)
	case relayconstant.RelayModeAudioTranscription:
		switch request.ResponseFormat {
		case "", "json", "text", "verbose_json":
			// The file itself is streamed to Soniox by DoRequest.
			return bytes.NewReader(nil), nil
		default:
			return nil, errors.New("soniox transcription supports response_format json, text or verbose_json")
		}
	default:
		return nil, errors.New("soniox does not support audio translation")
	}
}

func convertSpeechRequest(info *relaycommon.RelayInfo, request dto.AudioRequest) (io.Reader, error) {
	audioFormat, sampleRate, err := sonioxAudioFormat(request.ResponseFormat)
	if err != nil {
		return nil, err
	}

	language := defaultLanguage
	if len(request.Language) > 0 {
		if err := common.Unmarshal(request.Language, &language); err != nil || language == "" {
			return nil, errors.New("language must be a language code string")
		}
	}

	voice := request.Voice
	if voice == "" {
		voice = defaultVoice
	}

	body := ttsRequest{
		Model:       info.UpstreamModelName,
		Language:    language,
		Voice:       voice,
		AudioFormat: audioFormat,
		Text:        request.Input,
		SampleRate:  sampleRate,
	}
	if request.Speed != nil {
		speed := min(max(*request.Speed, minSpeed), maxSpeed)
		body.Speed = &speed
	}

	jsonData, err := common.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("error marshalling soniox tts request: %w", err)
	}
	return bytes.NewReader(jsonData), nil
}

// sonioxAudioFormat maps an OpenAI response_format to a Soniox audio_format
// and the sample rate that must accompany it.
func sonioxAudioFormat(responseFormat string) (string, *int, error) {
	switch responseFormat {
	case "", "mp3":
		return "mp3", nil, nil
	case "opus", "aac", "flac", "wav":
		return responseFormat, nil, nil
	case "pcm":
		rate := pcmSampleRate
		return "pcm_s16le", &rate, nil
	default:
		return "", nil, fmt.Errorf("unsupported response_format %q", responseFormat)
	}
}

func audioContentType(responseFormat string) string {
	switch responseFormat {
	case "wav":
		return "audio/wav"
	case "opus":
		return "audio/opus"
	case "aac":
		return "audio/aac"
	case "flac":
		return "audio/flac"
	case "pcm":
		return "audio/pcm"
	default:
		return "audio/mpeg"
	}
}

func (a *Adaptor) DoRequest(c *gin.Context, info *relaycommon.RelayInfo, requestBody io.Reader) (any, error) {
	if info.RelayMode == relayconstant.RelayModeAudioTranscription {
		resp, err := transcribe(c, info)
		if err != nil {
			return nil, err
		}
		return resp, nil
	}
	return channel.DoApiRequest(a, c, info, requestBody)
}

func (a *Adaptor) DoResponse(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo) (usage any, err *types.NewAPIError) {
	switch info.RelayMode {
	case relayconstant.RelayModeAudioSpeech:
		audioReq, ok := info.Request.(*dto.AudioRequest)
		if !ok {
			return nil, types.NewError(errors.New("invalid request type"), types.ErrorCodeInvalidRequest, types.ErrOptionWithSkipRetry())
		}
		// Soniox labels every format application/octet-stream.
		resp.Header.Set("Content-Type", audioContentType(audioReq.ResponseFormat))
		return openai.OpenaiTTSHandler(c, resp, info), nil
	case relayconstant.RelayModeAudioTranscription:
		return transcriptionResponse(c, resp, info)
	default:
		return nil, types.NewOpenAIError(errors.New("soniox only supports audio speech and transcription"), types.ErrorCodeInvalidRequest, http.StatusBadRequest, types.ErrOptionWithSkipRetry())
	}
}

func transcriptionResponse(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo) (any, *types.NewAPIError) {
	defer service.CloseResponseBodyGracefully(resp)
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeReadResponseBodyFailed, http.StatusInternalServerError)
	}
	var result transcriptionResult
	if err := common.Unmarshal(body, &result); err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
	}

	// Bill on the duration Soniox measured, at 1000 tokens per minute like the
	// other transcription channels. The value is upstream-controlled, so the
	// conversion saturates.
	seconds := float64(result.DurationMs) / 1000
	usage := &dto.Usage{}
	if seconds > 0 {
		usage.PromptTokens = common.QuotaRound(math.Ceil(seconds) / 60.0 * 1000)
	} else {
		usage.PromptTokens = info.GetEstimatePromptTokens()
	}
	usage.TotalTokens = usage.PromptTokens
	usage.PromptTokensDetails.AudioTokens = usage.PromptTokens

	responseFormat := ""
	if audioReq, ok := info.Request.(*dto.AudioRequest); ok {
		responseFormat = audioReq.ResponseFormat
	}
	switch responseFormat {
	case "text":
		c.Data(http.StatusOK, "text/plain; charset=utf-8", []byte(result.Text))
	case "verbose_json":
		c.JSON(http.StatusOK, gin.H{
			"task":     "transcribe",
			"duration": seconds,
			"text":     result.Text,
			"words":    tokensToWords(result.Tokens),
			"usage":    transcriptionUsage{Type: "duration", Seconds: seconds},
		})
	default:
		c.JSON(http.StatusOK, gin.H{
			"text":  result.Text,
			"usage": transcriptionUsage{Type: "duration", Seconds: seconds},
		})
	}
	return usage, nil
}

// tokensToWords joins Soniox sub-word tokens into words; a token starting with
// whitespace begins a new word.
func tokensToWords(tokens []sonioxToken) []transcriptionWord {
	words := make([]transcriptionWord, 0, len(tokens))
	for _, token := range tokens {
		startsWord := len(words) == 0 || strings.HasPrefix(token.Text, " ")
		if !startsWord {
			last := &words[len(words)-1]
			last.Word += token.Text
			last.End = float64(token.EndMs) / 1000
			continue
		}
		words = append(words, transcriptionWord{
			Word:  strings.TrimSpace(token.Text),
			Start: float64(token.StartMs) / 1000,
			End:   float64(token.EndMs) / 1000,
		})
	}
	return words
}

// transcribe runs the Soniox async flow for one uploaded file and returns a
// synthetic 200 response carrying the transcript. A non-2xx Soniox reply is
// returned as-is so the standard relay error handling reports it.
func transcribe(c *gin.Context, info *relaycommon.RelayInfo) (*http.Response, error) {
	form, err := common.ParseMultipartFormReusable(c)
	if err != nil {
		return nil, fmt.Errorf("error parsing multipart form: %w", err)
	}
	fileHeaders := form.File["file"]
	if len(fileHeaders) == 0 {
		return nil, errors.New("file is required")
	}
	client, err := service.GetHttpClientWithProxySettings(info.ChannelSetting.Proxy, info.ChannelSetting)
	if err != nil {
		return nil, fmt.Errorf("new proxy http client failed: %w", err)
	}

	baseURL := strings.TrimRight(info.ChannelBaseUrl, "/")
	if baseURL == "" {
		baseURL = constant.GetChannelBaseURL(constant.ChannelTypeSoniox)
	}
	call := func(ctx context.Context, method, path, contentType string, body io.Reader) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, method, baseURL+path, body)
		if err != nil {
			return nil, err
		}
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		req.Header.Set("Authorization", "Bearer "+info.ApiKey)
		return client.Do(req)
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), transcriptionTimeout)
	defer cancel()

	// Upload. The file is streamed so large recordings are not buffered twice.
	file, err := fileHeaders[0].Open()
	if err != nil {
		return nil, fmt.Errorf("error opening audio file: %w", err)
	}
	defer file.Close()
	pipeReader, pipeWriter := io.Pipe()
	formWriter := multipart.NewWriter(pipeWriter)
	go func() {
		part, err := formWriter.CreateFormFile("file", fileHeaders[0].Filename)
		if err == nil {
			_, err = io.Copy(part, file)
		}
		if err == nil {
			err = formWriter.Close()
		}
		pipeWriter.CloseWithError(err)
	}()
	uploadResp, err := call(ctx, http.MethodPost, "/v1/files", formWriter.FormDataContentType(), pipeReader)
	if err != nil {
		return nil, fmt.Errorf("soniox file upload failed: %w", err)
	}
	var uploaded sonioxFile
	if failed, err := decodeUpstream(uploadResp, &uploaded); failed != nil || err != nil {
		return failed, err
	}
	if uploaded.ID == "" {
		return nil, errors.New("soniox file upload returned no file id")
	}
	defer deleteUpstream(call, "/v1/files/"+uploaded.ID)

	createBody, err := common.Marshal(buildCreateTranscription(info, form, uploaded.ID))
	if err != nil {
		return nil, fmt.Errorf("error marshalling soniox transcription request: %w", err)
	}
	createResp, err := call(ctx, http.MethodPost, "/v1/transcriptions", "application/json", bytes.NewReader(createBody))
	if err != nil {
		return nil, fmt.Errorf("soniox create transcription failed: %w", err)
	}
	var created sonioxTranscription
	if failed, err := decodeUpstream(createResp, &created); failed != nil || err != nil {
		return failed, err
	}
	if created.ID == "" {
		return nil, errors.New("soniox create transcription returned no id")
	}
	defer deleteUpstream(call, "/v1/transcriptions/"+created.ID)

	var finished sonioxTranscription
	for finished.Status != "completed" {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("soniox transcription did not finish: %w", ctx.Err())
		case <-time.After(transcriptionPollInterval):
		}
		statusResp, err := call(ctx, http.MethodGet, "/v1/transcriptions/"+created.ID, "", nil)
		if err != nil {
			return nil, fmt.Errorf("soniox transcription status failed: %w", err)
		}
		finished = sonioxTranscription{}
		if failed, err := decodeUpstream(statusResp, &finished); failed != nil || err != nil {
			return failed, err
		}
		if finished.Status == "error" {
			return nil, fmt.Errorf("soniox transcription failed: %s", finished.ErrorMessage)
		}
	}

	transcriptResp, err := call(ctx, http.MethodGet, "/v1/transcriptions/"+created.ID+"/transcript", "", nil)
	if err != nil {
		return nil, fmt.Errorf("soniox transcript fetch failed: %w", err)
	}
	var transcript sonioxTranscript
	if failed, err := decodeUpstream(transcriptResp, &transcript); failed != nil || err != nil {
		return failed, err
	}

	result := transcriptionResult{Text: transcript.Text, Tokens: transcript.Tokens}
	if finished.AudioDurMs != nil {
		result.DurationMs = *finished.AudioDurMs
	}
	resultBody, err := common.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("error marshalling transcription result: %w", err)
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(resultBody)),
	}, nil
}

func buildCreateTranscription(info *relaycommon.RelayInfo, form *multipart.Form, fileID string) createTranscriptionRequest {
	req := createTranscriptionRequest{Model: info.UpstreamModelName, FileID: fileID}
	if language := strings.TrimSpace(firstFormValue(form, "language")); language != "" {
		req.LanguageHints = []string{language}
	}
	if prompt := strings.TrimSpace(firstFormValue(form, "prompt")); prompt != "" {
		req.Context = &transcriptionContext{Text: prompt}
	}
	return req
}

func firstFormValue(form *multipart.Form, key string) string {
	if values := form.Value[key]; len(values) > 0 {
		return values[0]
	}
	return ""
}

// decodeUpstream decodes a 2xx JSON body into out. For any other status it
// returns the response untouched (body unread) so the caller can surface it.
func decodeUpstream(resp *http.Response, out any) (*http.Response, error) {
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp, nil
	}
	defer service.CloseResponseBodyGracefully(resp)
	if err := common.DecodeJson(resp.Body, out); err != nil {
		return nil, fmt.Errorf("invalid soniox response: %w", err)
	}
	return nil, nil
}

// deleteUpstream removes an uploaded file or finished job so customer audio
// does not linger on Soniox. It outlives the request context on purpose, and
// failures are non-fatal.
func deleteUpstream(call func(context.Context, string, string, string, io.Reader) (*http.Response, error), path string) {
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	resp, err := call(ctx, http.MethodDelete, path, "", nil)
	if err != nil {
		common.SysError(fmt.Sprintf("soniox cleanup %s failed: %v", path, err))
		return
	}
	service.CloseResponseBodyGracefully(resp)
}

func (a *Adaptor) ConvertOpenAIRequest(*gin.Context, *relaycommon.RelayInfo, *dto.GeneralOpenAIRequest) (any, error) {
	return nil, errors.New("soniox only supports audio speech and transcription")
}

func (a *Adaptor) ConvertRerankRequest(*gin.Context, int, dto.RerankRequest) (any, error) {
	return nil, errors.New("not implemented")
}

func (a *Adaptor) ConvertEmbeddingRequest(*gin.Context, *relaycommon.RelayInfo, dto.EmbeddingRequest) (any, error) {
	return nil, errors.New("not implemented")
}

func (a *Adaptor) ConvertImageRequest(*gin.Context, *relaycommon.RelayInfo, dto.ImageRequest) (any, error) {
	return nil, errors.New("not implemented")
}

func (a *Adaptor) ConvertOpenAIResponsesRequest(*gin.Context, *relaycommon.RelayInfo, dto.OpenAIResponsesRequest) (any, error) {
	return nil, errors.New("not implemented")
}

func (a *Adaptor) ConvertClaudeRequest(*gin.Context, *relaycommon.RelayInfo, *dto.ClaudeRequest) (any, error) {
	return nil, errors.New("not implemented")
}

func (a *Adaptor) ConvertGeminiRequest(*gin.Context, *relaycommon.RelayInfo, *dto.GeminiChatRequest) (any, error) {
	return nil, errors.New("not implemented")
}

func (a *Adaptor) GetModelList() []string {
	return ModelList
}

func (a *Adaptor) GetChannelName() string {
	return ChannelName
}
