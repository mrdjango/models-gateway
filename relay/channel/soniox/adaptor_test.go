package soniox

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/QuantumNous/new-api/common"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relaykit/dto"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConvertSpeechRequest(t *testing.T) {
	speed := 4.0
	info := &relaycommon.RelayInfo{
		RelayMode:   relayconstant.RelayModeAudioSpeech,
		ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: "tts-rt-v2"},
	}

	tests := []struct {
		name    string
		request dto.AudioRequest
		want    string
		wantErr bool
	}{
		{
			name:    "defaults",
			request: dto.AudioRequest{Input: "hi"},
			want:    `{"model":"tts-rt-v2","language":"en","voice":"Adrian","audio_format":"mp3","text":"hi"}`,
		},
		{
			name: "pcm pins the sample rate and speed is clamped",
			request: dto.AudioRequest{
				Input: "hi", Voice: "Daniel", ResponseFormat: "pcm", Speed: &speed,
				Language: []byte(`"fa"`),
			},
			want: `{"model":"tts-rt-v2","language":"fa","voice":"Daniel","audio_format":"pcm_s16le","text":"hi","sample_rate":24000,"speed":1.3}`,
		},
		{
			name:    "unknown format is rejected",
			request: dto.AudioRequest{Input: "hi", ResponseFormat: "ogg"},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body, err := convertSpeechRequest(info, tt.request)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			got, err := io.ReadAll(body)
			require.NoError(t, err)
			assert.JSONEq(t, tt.want, string(got))
		})
	}
}

func TestGetRequestURLUsesTTSHost(t *testing.T) {
	tests := []struct{ base, want string }{
		{"", "https://tts-rt.soniox.com/tts"},
		{"https://api.soniox.com/", "https://tts-rt.soniox.com/tts"},
		{"https://api.eu.soniox.com", "https://tts-rt.eu.soniox.com/tts"},
		{"https://proxy.example.com", "https://proxy.example.com/tts"},
	}
	for _, tt := range tests {
		info := &relaycommon.RelayInfo{
			RelayMode:   relayconstant.RelayModeAudioSpeech,
			ChannelMeta: &relaycommon.ChannelMeta{ChannelBaseUrl: tt.base},
		}
		got, err := (&Adaptor{}).GetRequestURL(info)
		require.NoError(t, err)
		assert.Equal(t, tt.want, got, tt.base)
	}
}

func TestTokensToWords(t *testing.T) {
	words := tokensToWords([]sonioxToken{
		{Text: "H", StartMs: 210, EndMs: 270},
		{Text: "ello", StartMs: 390, EndMs: 450},
		{Text: " world", StartMs: 510, EndMs: 900},
		{Text: ".", StartMs: 900, EndMs: 930},
	})
	assert.Equal(t, []transcriptionWord{
		{Word: "Hello", Start: 0.21, End: 0.45},
		{Word: "world.", Start: 0.51, End: 0.93},
	}, words)
}

// fakeSoniox serves the async STT flow and records what it saw.
type fakeSoniox struct {
	mu      sync.Mutex
	polls   int
	calls   []string
	auth    string
	fileBuf []byte
	create  createTranscriptionRequest
}

func (f *fakeSoniox) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	f.auth = r.Header.Get("Authorization")
	switch r.Method + " " + r.URL.Path {
	case "POST /v1/files":
		file, _, err := r.FormFile("file")
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.fileBuf, _ = io.ReadAll(file)
		_, _ = w.Write([]byte(`{"id":"file-1"}`))
	case "POST /v1/transcriptions":
		_ = common.DecodeJson(r.Body, &f.create)
		_, _ = w.Write([]byte(`{"id":"tr-1","status":"queued"}`))
	case "GET /v1/transcriptions/tr-1":
		f.polls++
		if f.polls < 2 {
			_, _ = w.Write([]byte(`{"id":"tr-1","status":"processing"}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"tr-1","status":"completed","audio_duration_ms":61000}`))
	case "GET /v1/transcriptions/tr-1/transcript":
		_, _ = w.Write([]byte(`{"id":"tr-1","text":"Hello world","tokens":[{"text":"Hello","start_ms":0,"end_ms":400},{"text":" world","start_ms":500,"end_ms":900}]}`))
	case "DELETE /v1/transcriptions/tr-1", "DELETE /v1/files/file-1":
		w.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(w, r)
	}
}

func newTranscriptionContext(t *testing.T, fields map[string]string) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	part, err := form.CreateFormFile("file", "call.wav")
	require.NoError(t, err)
	_, err = part.Write([]byte("RIFFdata"))
	require.NoError(t, err)
	for k, v := range fields {
		require.NoError(t, form.WriteField(k, v))
	}
	require.NoError(t, form.Close())

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", &body)
	c.Request.Header.Set("Content-Type", form.FormDataContentType())
	return c, recorder
}

func TestTranscribeRunsAsyncFlowAndBillsMeasuredDuration(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := &fakeSoniox{}
	server := httptest.NewServer(upstream)
	defer server.Close()

	c, recorder := newTranscriptionContext(t, map[string]string{"language": "en", "prompt": "Acme Corp", "response_format": "verbose_json"})
	audioReq := &dto.AudioRequest{Model: "stt-async-v5", ResponseFormat: "verbose_json"}
	info := &relaycommon.RelayInfo{
		RelayMode: relayconstant.RelayModeAudioTranscription,
		Request:   audioReq,
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelBaseUrl:    server.URL,
			ApiKey:            "test-key",
			UpstreamModelName: "stt-async-v5",
		},
	}

	adaptor := &Adaptor{}
	_, err := adaptor.ConvertAudioRequest(c, info, *audioReq)
	require.NoError(t, err)
	respAny, err := adaptor.DoRequest(c, info, nil)
	require.NoError(t, err)
	usage, apiErr := adaptor.DoResponse(c, respAny.(*http.Response), info)
	require.Nil(t, apiErr)

	assert.Equal(t, "Bearer test-key", upstream.auth)
	assert.Equal(t, []byte("RIFFdata"), upstream.fileBuf)
	assert.Equal(t, "stt-async-v5", upstream.create.Model)
	assert.Equal(t, "file-1", upstream.create.FileID)
	assert.Equal(t, []string{"en"}, upstream.create.LanguageHints)
	require.NotNil(t, upstream.create.Context)
	assert.Equal(t, "Acme Corp", upstream.create.Context.Text)
	// Uploaded audio and the finished job are both removed from Soniox.
	assert.Contains(t, upstream.calls, "DELETE /v1/files/file-1")
	assert.Contains(t, upstream.calls, "DELETE /v1/transcriptions/tr-1")

	assert.JSONEq(t,
		`{"task":"transcribe","duration":61,"text":"Hello world","words":[{"word":"Hello","start":0,"end":0.4},{"word":"world","start":0.5,"end":0.9}],"usage":{"type":"duration","seconds":61}}`,
		recorder.Body.String())

	// 61s -> 61/60 minutes at 1000 tokens per minute (1016.67 rounds to 1017), all audio.
	billed, ok := usage.(*dto.Usage)
	require.True(t, ok)
	assert.Equal(t, 1017, billed.PromptTokens)
	assert.Equal(t, 1017, billed.PromptTokensDetails.AudioTokens)
}

func TestTranscribeSurfacesUpstreamErrorResponse(t *testing.T) {
	gin.SetMode(gin.TestMode)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error_type":"unauthenticated","error_message":"bad key"}`))
	}))
	defer server.Close()

	c, _ := newTranscriptionContext(t, nil)
	info := &relaycommon.RelayInfo{
		RelayMode:   relayconstant.RelayModeAudioTranscription,
		ChannelMeta: &relaycommon.ChannelMeta{ChannelBaseUrl: server.URL, ApiKey: "bad"},
	}
	respAny, err := (&Adaptor{}).DoRequest(c, info, nil)
	require.NoError(t, err)
	assert.Equal(t, http.StatusUnauthorized, respAny.(*http.Response).StatusCode)
}

func TestConvertAudioRequestRejectsUnsupportedModes(t *testing.T) {
	info := &relaycommon.RelayInfo{RelayMode: relayconstant.RelayModeAudioTranscription}
	_, err := (&Adaptor{}).ConvertAudioRequest(nil, info, dto.AudioRequest{ResponseFormat: "srt"})
	require.Error(t, err)
	info.RelayMode = relayconstant.RelayModeAudioTranslation
	_, err = (&Adaptor{}).ConvertAudioRequest(nil, info, dto.AudioRequest{})
	require.Error(t, err)
}
