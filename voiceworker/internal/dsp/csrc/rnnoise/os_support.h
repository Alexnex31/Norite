/* RNNoise 0.2's vec.h includes os_support.h in its plain-C branch, and the release does not carry one:
   nobody upstream builds that branch. It needs OPUS_CLEAR alone, as libopus defines it. */
#ifndef OS_SUPPORT_H
#define OS_SUPPORT_H
#include <string.h>
#define OPUS_CLEAR(dst, n) (memset((dst), 0, (n)*sizeof(*(dst))))
#endif
