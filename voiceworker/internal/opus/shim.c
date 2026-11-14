//go:build norite_opusnative && cgo

/*
 * The worker's whole interface onto libopus, compiled into opus.wasm by scripts/build-opus-wasm.sh and, under
 * the norite_opusnative tag, into the worker natively (ADR 0036). The two implementations call these and
 * nothing else, so they make the same calls.
 *
 * Every function takes plain integers and pointers: opus_encoder_ctl is variadic, and a variadic call across
 * the WebAssembly boundary would mean laying out its arguments in linear memory by hand.
 */
#include <stdlib.h>
#include <opus.h>

#ifdef __wasm__
#define EXPORT(name) __attribute__((export_name(#name)))
#else
#define EXPORT(name)
#endif

EXPORT(shim_alloc) void *shim_alloc(int n) { return malloc((size_t)n); }

EXPORT(shim_encoder) OpusEncoder *shim_encoder(int bitrate, int complexity, int fec, int dtx, int loss) {
	int err = 0;
	OpusEncoder *e = opus_encoder_create(48000, 1, OPUS_APPLICATION_VOIP, &err);
	if (err != OPUS_OK) return 0;
	if (opus_encoder_ctl(e, OPUS_SET_BITRATE(bitrate)) != OPUS_OK ||
		opus_encoder_ctl(e, OPUS_SET_COMPLEXITY(complexity)) != OPUS_OK ||
		opus_encoder_ctl(e, OPUS_SET_INBAND_FEC(fec)) != OPUS_OK ||
		opus_encoder_ctl(e, OPUS_SET_DTX(dtx)) != OPUS_OK ||
		opus_encoder_ctl(e, OPUS_SET_PACKET_LOSS_PERC(loss)) != OPUS_OK ||
		opus_encoder_ctl(e, OPUS_SET_SIGNAL(OPUS_SIGNAL_VOICE)) != OPUS_OK) {
		opus_encoder_destroy(e);
		return 0;
	}
	return e;
}

EXPORT(shim_encoder_free) void shim_encoder_free(OpusEncoder *e) { opus_encoder_destroy(e); }

EXPORT(shim_set_bitrate) int shim_set_bitrate(OpusEncoder *e, int bitrate) {
	return opus_encoder_ctl(e, OPUS_SET_BITRATE(bitrate));
}

/* The loss the encoder sizes its in-band FEC for, which the worker follows from what its feedback measures
 * (M31). */
EXPORT(shim_set_loss) int shim_set_loss(OpusEncoder *e, int percent) {
	return opus_encoder_ctl(e, OPUS_SET_PACKET_LOSS_PERC(percent));
}

EXPORT(shim_encode) int shim_encode(OpusEncoder *e, const opus_int16 *pcm, int frame, unsigned char *out,
	int max) {
	return opus_encode(e, pcm, frame, out, max);
}

EXPORT(shim_decoder) OpusDecoder *shim_decoder(void) {
	int err = 0;
	OpusDecoder *d = opus_decoder_create(48000, 1, &err);
	return err == OPUS_OK ? d : 0;
}

EXPORT(shim_decoder_free) void shim_decoder_free(OpusDecoder *d) { opus_decoder_destroy(d); }

/* data may be 0 with len 0, which conceals a lost frame; fec asks for the frame before this packet. */
EXPORT(shim_decode) int shim_decode(OpusDecoder *d, const unsigned char *data, int len, opus_int16 *pcm,
	int frame, int fec) {
	return opus_decode(d, data, len, pcm, frame, fec);
}

EXPORT(shim_reset) int shim_reset(OpusDecoder *d) { return opus_decoder_ctl(d, OPUS_RESET_STATE); }
