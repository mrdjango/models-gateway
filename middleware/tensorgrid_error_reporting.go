package middleware

// TensorGrid fork: panic reporting to GlitchTip (Sentry-compatible), project models-gateway
// at https://glitchtip.tensorgrid.space. Off unless SENTRY_DSN is set. Kept in its own file
// so upstream syncs only meet the hooks in main.go and recover.go.
//
// Privacy: SendDefaultPII stays off, so sentry-go drops Authorization, Cookie and
// X-Forwarded-For; request bodies (prompts) are never read, and query strings are removed
// because some clients pass API keys there.

import (
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/getsentry/sentry-go"
	sentrygin "github.com/getsentry/sentry-go/gin"
	"github.com/gin-gonic/gin"
)

var errorReportingEnabled bool

// InitErrorReporting starts the Sentry client when SENTRY_DSN is set.
func InitErrorReporting() {
	dsn := strings.TrimSpace(os.Getenv("SENTRY_DSN"))
	if dsn == "" {
		return
	}
	sampleRate, err := strconv.ParseFloat(os.Getenv("SENTRY_TRACES_SAMPLE_RATE"), 64)
	if err != nil {
		sampleRate = 0
	}
	err = sentry.Init(sentry.ClientOptions{
		Dsn:              dsn,
		Environment:      os.Getenv("SENTRY_ENVIRONMENT"),
		Release:          common.Version,
		ServerName:       "models-gateway",
		SendDefaultPII:   false,
		AttachStacktrace: true,
		EnableTracing:    sampleRate > 0,
		TracesSampleRate: sampleRate,
		BeforeSend: func(event *sentry.Event, _ *sentry.EventHint) *sentry.Event {
			scrubReportedRequest(event.Request)
			// A panic with a non-error value (panic("...")) arrives as a message whose stack
			// is in Threads, which GlitchTip does not display; report it as an exception.
			if len(event.Exception) == 0 && len(event.Threads) > 0 && event.Threads[0].Stacktrace != nil {
				event.Exception = []sentry.Exception{{Type: "panic", Value: event.Message, Stacktrace: event.Threads[0].Stacktrace}}
				event.Threads = nil
			}
			return event
		},
	})
	if err != nil {
		common.SysError("error reporting disabled: " + err.Error())
		return
	}
	errorReportingEnabled = true
	common.SysLog("error reporting enabled")
}

// FlushErrorReporting sends queued events before the process exits.
func FlushErrorReporting() {
	if errorReportingEnabled {
		sentry.Flush(2 * time.Second)
	}
}

// ErrorReporting captures panics that reach the top of the handler chain and re-panics so
// gin.CustomRecovery still writes the response. Register it right after that recovery.
func ErrorReporting() gin.HandlerFunc {
	if !errorReportingEnabled {
		return func(c *gin.Context) { c.Next() }
	}
	return sentrygin.New(sentrygin.Options{Repanic: true, Timeout: 2 * time.Second})
}

// ReportRelayPanic records a panic that RelayPanicRecover swallowed before it could reach
// ErrorReporting.
func ReportRelayPanic(c *gin.Context, recovered any) {
	if !errorReportingEnabled {
		return
	}
	hub := sentrygin.GetHubFromContext(c)
	if hub == nil {
		hub = sentry.CurrentHub().Clone()
	}
	hub.RecoverWithContext(c.Request.Context(), recovered)
}

// reportedRequestHeaders is the only request metadata kept on an event. Everything else
// (Authorization, X-Api-Key, provider keys in custom headers, cookies) is dropped.
var reportedRequestHeaders = []string{"Content-Type", "User-Agent", "Host", "X-Request-Id"}

// scrubReportedRequest removes what may carry credentials or prompts from the request
// attached to an event: query strings (some clients send ?key=), headers outside
// reportedRequestHeaders, cookies and any body.
func scrubReportedRequest(req *sentry.Request) {
	if req == nil {
		return
	}
	kept := make(map[string]string, len(reportedRequestHeaders))
	for _, name := range reportedRequestHeaders {
		if v, ok := req.Headers[name]; ok {
			kept[name] = v
		}
	}
	req.Headers = kept
	req.Env = nil
	req.QueryString = ""
	req.Cookies = ""
	req.Data = ""
	if u, err := url.Parse(req.URL); err == nil {
		u.RawQuery = ""
		u.Fragment = ""
		req.URL = u.String()
	}
}
