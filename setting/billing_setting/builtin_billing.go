package billing_setting

// Built-in token prices use actual USD per million tokens. Keep new model
// defaults here instead of splitting them across the legacy ratio tables.
var builtinBillingExpr = map[string]string{
	// https://developers.openai.com/api/docs/pricing (Standard, 2026-09-09).
	// The Images API reports image output in output_tokens, normalized to c.
	"gpt-image-2":            `tier("standard", p * 5 + cr * 1.25 + img * 8 + img_cr * 2 + c * 30)`,
	"gpt-image-2.5-sunburst": `tier("standard", p * 5 + cr * 1.25 + img * 8 + img_cr * 2 + c * 30)`,
	"gpt-image-2.5-flare":    `tier("standard", p * 5 + cr * 1.25 + img * 8 + img_cr * 2 + c * 30)`,
	// https://soniox.com/pricing (2026-09-30). The audio relay counts 1000
	// tokens per audio minute (60000 per hour). Async STT is $0.10/hour of
	// input audio, so p is $0.10 / 60000 per token, in USD per million tokens.
	// TTS input p is counted in characters (~0.3 Soniox text tokens each at
	// $4.00/M) and output c is the generated audio, priced so an hour of
	// speech totals the published ~$0.70.
	"stt-async-v5": `tier("standard", p * 1.6667)`,
	"tts-rt-v2":    `tier("standard", p * 1.2 + c * 10.62)`,
	// https://developers.openai.com/api/docs/models/gpt-6-astra
	// Standard pricing; the long-context rates apply to the whole request.
	// Do not infer service-tier discounts from incoming request parameters:
	// channels filter service_tier by default, so it may not reach the upstream.
	"gpt-6-astra": `len <= 272000 ? tier("standard", p * 10 + c * 50 + cr * 1 + cc * 12.5) : tier("long_context", p * 20 + c * 75 + cr * 2 + cc * 25)`,
}
