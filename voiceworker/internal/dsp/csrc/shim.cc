/*
 * The worker's whole interface onto the audio DSP, compiled into dsp.wasm by scripts/build-dsp-wasm.sh
 * (ADR 0036). One instance holds one call's chain: two AudioProcessing objects, one doing AEC3 and its
 * high-pass filter and nothing else, one doing AGC2's adaptive digital gain and nothing else, and one
 * RNNoise state.
 *
 * The components are exported one by one, and the order they run in is internal/dsp's, in Go: AEC, then
 * RNNoise, then AGC. RNNoise sits between two calls into the APM because the APM's own chain has no place
 * for it.
 *
 * Every call works in place on 10 ms of 48 kHz mono s16, the buffer dsp_alloc gave.
 */
#include <math.h>
#include <stdint.h>
#include <stdlib.h>

#include "modules/audio_processing/include/audio_processing.h"
#include "rnnoise.h"

#define EXPORT(name) extern "C" __attribute__((export_name(#name)))

namespace {

constexpr int kRate = 48000;
constexpr int kChunk = kRate / 100;

struct Chain {
	webrtc::AudioProcessing *aec;
	webrtc::AudioProcessing *agc;
	DenoiseState *ns;
	float in[kChunk], out[kChunk];
};

webrtc::AudioProcessing *apm(bool aec) {
	webrtc::AudioProcessing::Config c;
	c.pipeline.multi_channel_render = false;
	c.pipeline.multi_channel_capture = false;
	c.high_pass_filter.enabled = aec;
	c.echo_canceller.enabled = aec;
	c.echo_canceller.mobile_mode = false;
	c.noise_suppression.enabled = false;
	c.gain_controller1.enabled = false;
	c.gain_controller2.enabled = !aec;
	c.gain_controller2.adaptive_digital.enabled = !aec;
	// It would set the OS microphone's volume, which the worker never touches.
	c.gain_controller2.input_volume_controller.enabled = false;
	rtc::scoped_refptr<webrtc::AudioProcessing> p = webrtc::AudioProcessingBuilder().Create();
	if (!p) return nullptr;
	p->ApplyConfig(c);
	return p.release();
}

const webrtc::StreamConfig kStream(kRate, 1);

}  // namespace

EXPORT(dsp_alloc) void *dsp_alloc(int n) { return malloc((size_t)n); }

// dsp_new makes a call's chain, or returns 0.
EXPORT(dsp_new) Chain *dsp_new(void) {
	Chain *c = static_cast<Chain *>(calloc(1, sizeof(Chain)));
	if (!c) return nullptr;
	c->aec = apm(true);
	c->agc = apm(false);
	c->ns = rnnoise_create(nullptr);
	if (!c->aec || !c->agc || !c->ns) return nullptr;
	return c;
}

// dsp_render takes 10 ms of what the speakers played. It returns the APM's error code, 0 for none.
EXPORT(dsp_render) int dsp_render(Chain *c, int16_t *buf) {
	return c->aec->ProcessReverseStream(buf, kStream, kStream, buf);
}

// dsp_aec runs the high-pass filter and AEC3 on 10 ms of the microphone. The stream delay is never set:
// AEC3 then starts from its default delay and estimates the rest, where a hint of 0 would start it at
// its minimum (render_delay_buffer.cc).
EXPORT(dsp_aec) int dsp_aec(Chain *c, int16_t *buf) {
	return c->aec->ProcessStream(buf, kStream, kStream, buf);
}

// dsp_ns runs RNNoise on 10 ms and returns its voice probability, from 0 to 1. RNNoise reads and writes
// floats at the scale of s16; the result is rounded to nearest, ties to even, and saturated, and a NaN,
// which no correct RNNoise makes, is silence rather than a conversion whose result C leaves undefined.
EXPORT(dsp_ns) float dsp_ns(Chain *c, int16_t *buf) {
	for (int i = 0; i < kChunk; i++) c->in[i] = buf[i];
	float p = rnnoise_process_frame(c->ns, c->out, c->in);
	for (int i = 0; i < kChunk; i++) {
		float v = nearbyintf(c->out[i]);
		buf[i] = v != v ? 0 : (int16_t)(v > 32767.f ? 32767.f : v < -32768.f ? -32768.f : v);
	}
	return p;
}

// dsp_agc runs AGC2's adaptive digital gain on 10 ms.
EXPORT(dsp_agc) int dsp_agc(Chain *c, int16_t *buf) {
	return c->agc->ProcessStream(buf, kStream, kStream, buf);
}

// dsp_erle is AEC3's echo return loss enhancement in dB, or -1 while it has none.
EXPORT(dsp_erle) float dsp_erle(Chain *c) {
	webrtc::AudioProcessingStats s = c->aec->GetStatistics();
	return s.echo_return_loss_enhancement ? (float)*s.echo_return_loss_enhancement : -1.f;
}
