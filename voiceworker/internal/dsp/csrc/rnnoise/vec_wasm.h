/* WebAssembly SIMD128 kernels for RNNoise.

   Each function computes exactly what vec.h's plain-C version computes, bit for bit:
   - the float kernels do, per lane, the same multiplies and adds in the same order (wasm has no
     fused multiply-add, so nothing is contracted on either side);
   - the int8 kernels quantize the input with the plain-C loop itself, then accumulate in int32, where
     plain C accumulates in float. Every partial sum is an integer below 2^24 for RNNoise's layers, so
     float is exact there and the two agree;
   - min and max are pmin and pmax, which are the ternaries MIN32 and MAX32 are, NaN and -0 included.
   Only standard SIMD128, never relaxed-simd, whose results may differ between hosts.

   Norite's (M30), modelled on vec_neon.h and selected by the two lines scripts/dsp-wasm/rnnoise-vec-wasm.patch
   adds to vec.h. lpcnet_exp2, tanh_approx, sigmoid_approx and softmax are RNNoise's own, copied from vec.h's
   plain-C branch, which this file replaces whole as vec_neon.h does: their copyright and licence are
   RNNoise's (BSD-3-Clause), shipped in internal/notices/embedded/rnnoise. scripts/build-dsp-wasm.sh builds
   the module once more with DISABLE_WASM_SIMD, and internal/dsp's tests require the two to agree byte for
   byte. */

#ifndef VEC_WASM_H
#define VEC_WASM_H

#include <wasm_simd128.h>
#include "os_support.h"

#define MAX_INPUTS (2048)

#define SCALE (128.f*127.f)
#define SCALE_1 (1.f/128.f/127.f)

#ifndef LPCNET_TEST
/* Unchanged from vec.h. Only softmax reaches it, and RNNoise's model has no softmax layer. */
static inline float lpcnet_exp2(float x)
{
   int integer;
   float frac;
   union {
      float f;
      opus_uint32 i;
   } res;
   integer = floor(x);
   if (integer < -50)
      return 0;
   frac = x-integer;
   res.f = 0.99992522f + frac * (0.69583354f
           + frac * (0.22606716f + 0.078024523f*frac));
   res.i = (res.i + (integer<<23)) & 0x7fffffff;
   return res.f;
}
#define lpcnet_exp(x) lpcnet_exp2((x)*1.44269504f)

#define fmadd(a, b, c) ((a)*(b)+(c))
static OPUS_INLINE float tanh_approx(float x)
{
    const float N0 = 952.52801514f;
    const float N1 = 96.39235687f;
    const float N2 = 0.60863042f;
    const float D0 = 952.72399902f;
    const float D1 = 413.36801147f;
    const float D2 = 11.88600922f;
    float X2, num, den;
    X2 = x*x;
    num = fmadd(fmadd(N2, X2, N1), X2, N0);
    den = fmadd(fmadd(D2, X2, D1), X2, D0);
    num = num*x/den;
    return MAX32(-1.f, MIN32(1.f, num));
}

static inline float sigmoid_approx(float x)
{
   return .5f + .5f*tanh_approx(.5f*x);
}

/* tanh_approx on four lanes: the same operations in the same order. */
static inline v128_t tanh4_approx(v128_t x)
{
    const v128_t N0 = wasm_f32x4_splat(952.52801514f);
    const v128_t N1 = wasm_f32x4_splat(96.39235687f);
    const v128_t N2 = wasm_f32x4_splat(0.60863042f);
    const v128_t D0 = wasm_f32x4_splat(952.72399902f);
    const v128_t D1 = wasm_f32x4_splat(413.36801147f);
    const v128_t D2 = wasm_f32x4_splat(11.88600922f);
    v128_t X2, num, den;
    X2 = wasm_f32x4_mul(x, x);
    num = wasm_f32x4_add(wasm_f32x4_mul(wasm_f32x4_add(wasm_f32x4_mul(N2, X2), N1), X2), N0);
    den = wasm_f32x4_add(wasm_f32x4_mul(wasm_f32x4_add(wasm_f32x4_mul(D2, X2), D1), X2), D0);
    num = wasm_f32x4_div(wasm_f32x4_mul(num, x), den);
    /* MIN32(1, num) is (1 < num ? 1 : num), which is pmin(num, 1); MAX32(-1, m) is pmax(m, -1). */
    num = wasm_f32x4_pmin(num, wasm_f32x4_splat(1.f));
    return wasm_f32x4_pmax(num, wasm_f32x4_splat(-1.f));
}

static inline void softmax(float *y, const float *x, int N)
{
    int i;
    for (i=0;i<N;i++)
        y[i] = lpcnet_exp(x[i]);
}

static inline void vec_tanh(float *y, const float *x, int N)
{
    int i;
    for (i=0;i<N-3;i+=4)
        wasm_v128_store(&y[i], tanh4_approx(wasm_v128_load(&x[i])));
    for (;i<N;i++)
        y[i] = tanh_approx(x[i]);
}

static inline void vec_sigmoid(float *y, const float *x, int N)
{
    const v128_t half = wasm_f32x4_splat(.5f);
    int i;
    for (i=0;i<N-3;i+=4)
    {
        v128_t t = tanh4_approx(wasm_f32x4_mul(half, wasm_v128_load(&x[i])));
        wasm_v128_store(&y[i], wasm_f32x4_add(half, wasm_f32x4_mul(half, t)));
    }
    for (;i<N;i++)
        y[i] = sigmoid_approx(x[i]);
}
#endif

static inline void sgemv16x1(float *out, const float *weights, int rows, int cols, int col_stride, const float *x)
{
   int i, j;
   for (i=0;i<rows;i+=16)
   {
      v128_t y0 = wasm_f32x4_splat(0), y4 = y0, y8 = y0, y12 = y0;
      for (j=0;j<cols;j++)
      {
         const float *w = &weights[j*col_stride + i];
         v128_t xj = wasm_v128_load32_splat(&x[j]);
         y0 = wasm_f32x4_add(y0, wasm_f32x4_mul(wasm_v128_load(&w[0]), xj));
         y4 = wasm_f32x4_add(y4, wasm_f32x4_mul(wasm_v128_load(&w[4]), xj));
         y8 = wasm_f32x4_add(y8, wasm_f32x4_mul(wasm_v128_load(&w[8]), xj));
         y12 = wasm_f32x4_add(y12, wasm_f32x4_mul(wasm_v128_load(&w[12]), xj));
      }
      wasm_v128_store(&out[i], y0);
      wasm_v128_store(&out[i+4], y4);
      wasm_v128_store(&out[i+8], y8);
      wasm_v128_store(&out[i+12], y12);
   }
}

static inline void sgemv8x1(float *out, const float *weights, int rows, int cols, int col_stride, const float *x)
{
   int i, j;
   for (i=0;i<rows;i+=8)
   {
      v128_t y0 = wasm_f32x4_splat(0), y4 = y0;
      for (j=0;j<cols;j++)
      {
         const float *w = &weights[j*col_stride + i];
         v128_t xj = wasm_v128_load32_splat(&x[j]);
         y0 = wasm_f32x4_add(y0, wasm_f32x4_mul(wasm_v128_load(&w[0]), xj));
         y4 = wasm_f32x4_add(y4, wasm_f32x4_mul(wasm_v128_load(&w[4]), xj));
      }
      wasm_v128_store(&out[i], y0);
      wasm_v128_store(&out[i+4], y4);
   }
}

static inline void sgemv(float *out, const float *weights, int rows, int cols, int col_stride, const float *x)
{
   if ((rows&0xf) == 0) sgemv16x1(out, weights, rows, cols, col_stride, x);
   else if ((rows&0x7) == 0) sgemv8x1(out, weights, rows, cols, col_stride, x);
   else {
      int i, j;
      for (i=0;i<rows;i++)
      {
         out[i] = 0;
         for (j=0;j<cols;j++) out[i] += weights[j*col_stride + i]*x[j];
      }
   }
}

static inline void sparse_sgemv8x4(float *out, const float *w, const int *idx, int rows, const float *x)
{
   int i, j;
   for (i=0;i<rows;i+=8)
   {
      int cols = *idx++;
      v128_t y0 = wasm_f32x4_splat(0), y4 = y0;
      for (j=0;j<cols;j++)
      {
         int pos = (*idx++);
         int k;
         /* plain C adds column 0's products to every row, then column 1's, and so on. */
         for (k=0;k<4;k++)
         {
            v128_t xj = wasm_v128_load32_splat(&x[pos+k]);
            y0 = wasm_f32x4_add(y0, wasm_f32x4_mul(wasm_v128_load(&w[8*k]), xj));
            y4 = wasm_f32x4_add(y4, wasm_f32x4_mul(wasm_v128_load(&w[8*k+4]), xj));
         }
         w += 32;
      }
      wasm_v128_store(&out[i], y0);
      wasm_v128_store(&out[i+4], y4);
   }
}

/* x is four int8 inputs splatted across 16 bytes. Rows r..r+3 of a block take 16 weights, four per row,
   so extending each half to int16 and taking dot products against (x0 x1 x2 x3 x0 x1 x2 x3) gives the
   pairwise sums of two rows per half; the pairs are added once, after the loop. */
static inline void dot8x4(v128_t *lo0, v128_t *hi0, v128_t *lo1, v128_t *hi1, const opus_int8 *w, v128_t xs)
{
   v128_t x16 = wasm_i16x8_extend_low_i8x16(xs);
   v128_t w0 = wasm_v128_load(w);
   v128_t w1 = wasm_v128_load(w+16);
   *lo0 = wasm_i32x4_add(*lo0, wasm_i32x4_dot_i16x8(wasm_i16x8_extend_low_i8x16(w0), x16));
   *hi0 = wasm_i32x4_add(*hi0, wasm_i32x4_dot_i16x8(wasm_i16x8_extend_high_i8x16(w0), x16));
   *lo1 = wasm_i32x4_add(*lo1, wasm_i32x4_dot_i16x8(wasm_i16x8_extend_low_i8x16(w1), x16));
   *hi1 = wasm_i32x4_add(*hi1, wasm_i32x4_dot_i16x8(wasm_i16x8_extend_high_i8x16(w1), x16));
}

static inline v128_t pairs(v128_t lo, v128_t hi)
{
   return wasm_i32x4_add(wasm_i32x4_shuffle(lo, hi, 0, 2, 4, 6), wasm_i32x4_shuffle(lo, hi, 1, 3, 5, 7));
}

static inline void store8(float *out, const float *scale, v128_t lo0, v128_t hi0, v128_t lo1, v128_t hi1)
{
   wasm_v128_store(out, wasm_f32x4_mul(wasm_f32x4_convert_i32x4(pairs(lo0, hi0)), wasm_v128_load(scale)));
   wasm_v128_store(out+4, wasm_f32x4_mul(wasm_f32x4_convert_i32x4(pairs(lo1, hi1)), wasm_v128_load(scale+4)));
}

static inline void sparse_cgemv8x4(float *out, const opus_int8 *w, const int *idx, const float *scale, int rows, int cols, const float *_x)
{
   int i, j;
   opus_int8 x[MAX_INPUTS];
   for (i=0;i<cols;i++) x[i] = (int)floor(.5+127*_x[i]);
   for (i=0;i<rows;i+=8)
   {
      int colblocks = *idx++;
      v128_t lo0 = wasm_i32x4_splat(0), hi0 = lo0, lo1 = lo0, hi1 = lo0;
      for (j=0;j<colblocks;j++)
      {
         int pos = (*idx++);
         dot8x4(&lo0, &hi0, &lo1, &hi1, w, wasm_v128_load32_splat(&x[pos]));
         w += 32;
      }
      store8(&out[i], &scale[i], lo0, hi0, lo1, hi1);
   }
}

static inline void cgemv8x4(float *out, const opus_int8 *w, const float *scale, int rows, int cols, const float *_x)
{
   int i, j;
   opus_int8 x[MAX_INPUTS];
   for (i=0;i<cols;i++) x[i] = (int)floor(.5+127*_x[i]);
   for (i=0;i<rows;i+=8)
   {
      v128_t lo0 = wasm_i32x4_splat(0), hi0 = lo0, lo1 = lo0, hi1 = lo0;
      for (j=0;j<cols;j+=4)
      {
         dot8x4(&lo0, &hi0, &lo1, &hi1, w, wasm_v128_load32_splat(&x[j]));
         w += 32;
      }
      store8(&out[i], &scale[i], lo0, hi0, lo1, hi1);
   }
}

#endif /*VEC_WASM_H*/
