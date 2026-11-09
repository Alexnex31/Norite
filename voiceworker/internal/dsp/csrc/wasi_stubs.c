/*
 * The WASI calls wasi-libc makes, answered inside dsp.wasm, so that the module imports nothing and runs
 * with no host module, as opus.wasm does (ADR 0036). internal/dsp holds it to importing nothing.
 *
 * libc keeps every wrapper in one object, __wasilibc_real.c.obj, and a single reference left undefined
 * pulls the whole object in, each wrapper's import with it. So every wrapper libc references before
 * --gc-sections runs is defined here, not only the eight that survive it (clock_time_get, fd_close,
 * fd_fdstat_get, fd_prestat_get, fd_prestat_dir_name, fd_seek, fd_write and proc_exit). A wrapper missing
 * fails the link with a duplicate symbol, which is how a new libc says it references one more.
 *
 * What the module sees: a clock that never moves, so nothing it computes depends on time; output that goes
 * nowhere; no file and no directory; and an exit that traps.
 */
#include <wasi/api.h>

__wasi_errno_t __wasi_clock_time_get(__wasi_clockid_t id, __wasi_timestamp_t precision, __wasi_timestamp_t *t)
{
	(void)id;
	(void)precision;
	*t = 0;
	return __WASI_ERRNO_SUCCESS;
}

/* Every byte is reported written, so stdio does not retry. */
__wasi_errno_t __wasi_fd_write(__wasi_fd_t fd, const __wasi_ciovec_t *iovs, size_t n, __wasi_size_t *written)
{
	__wasi_size_t w = 0;
	(void)fd;
	for (size_t i = 0; i < n; i++)
		w += iovs[i].buf_len;
	*written = w;
	return __WASI_ERRNO_SUCCESS;
}

/* The worker decides what follows a trap; the instance is discarded. */
_Noreturn void __wasi_proc_exit(__wasi_exitcode_t code)
{
	(void)code;
	__builtin_trap();
}

__wasi_errno_t __wasi_fd_close(__wasi_fd_t fd)
{
	(void)fd;
	return __WASI_ERRNO_BADF;
}

__wasi_errno_t __wasi_fd_seek(__wasi_fd_t fd, __wasi_filedelta_t off, __wasi_whence_t whence, __wasi_filesize_t *r)
{
	(void)fd;
	(void)off;
	(void)whence;
	(void)r;
	return __WASI_ERRNO_SPIPE;
}

__wasi_errno_t __wasi_fd_fdstat_get(__wasi_fd_t fd, __wasi_fdstat_t *r)
{
	(void)fd;
	(void)r;
	return __WASI_ERRNO_BADF;
}

/* No preopened directory, so libc finds no filesystem at all. */
__wasi_errno_t __wasi_fd_prestat_get(__wasi_fd_t fd, __wasi_prestat_t *r)
{
	(void)fd;
	(void)r;
	return __WASI_ERRNO_BADF;
}

__wasi_errno_t __wasi_fd_prestat_dir_name(__wasi_fd_t fd, uint8_t *path, __wasi_size_t n)
{
	(void)fd;
	(void)path;
	(void)n;
	return __WASI_ERRNO_BADF;
}

/* Referenced, and removed by --gc-sections: nothing reaches them, since there is no file to open. */

__wasi_errno_t __wasi_fd_fdstat_set_flags(__wasi_fd_t fd, __wasi_fdflags_t flags)
{
	(void)fd;
	(void)flags;
	return __WASI_ERRNO_NOSYS;
}

__wasi_errno_t __wasi_fd_read(__wasi_fd_t fd, const __wasi_iovec_t *iovs, size_t n, __wasi_size_t *r)
{
	(void)fd;
	(void)iovs;
	(void)n;
	(void)r;
	return __WASI_ERRNO_NOSYS;
}

__wasi_errno_t __wasi_fd_readdir(__wasi_fd_t fd, uint8_t *buf, __wasi_size_t n, __wasi_dircookie_t cookie,
	__wasi_size_t *r)
{
	(void)fd;
	(void)buf;
	(void)n;
	(void)cookie;
	(void)r;
	return __WASI_ERRNO_NOSYS;
}

__wasi_errno_t __wasi_fd_renumber(__wasi_fd_t fd, __wasi_fd_t to)
{
	(void)fd;
	(void)to;
	return __WASI_ERRNO_NOSYS;
}

__wasi_errno_t __wasi_path_create_directory(__wasi_fd_t fd, const char *path)
{
	(void)fd;
	(void)path;
	return __WASI_ERRNO_NOSYS;
}

__wasi_errno_t __wasi_path_filestat_get(__wasi_fd_t fd, __wasi_lookupflags_t flags, const char *path,
	__wasi_filestat_t *r)
{
	(void)fd;
	(void)flags;
	(void)path;
	(void)r;
	return __WASI_ERRNO_NOSYS;
}

__wasi_errno_t __wasi_path_filestat_set_times(__wasi_fd_t fd, __wasi_lookupflags_t flags, const char *path,
	__wasi_timestamp_t atim, __wasi_timestamp_t mtim, __wasi_fstflags_t fst)
{
	(void)fd;
	(void)flags;
	(void)path;
	(void)atim;
	(void)mtim;
	(void)fst;
	return __WASI_ERRNO_NOSYS;
}

__wasi_errno_t __wasi_path_link(__wasi_fd_t old_fd, __wasi_lookupflags_t flags, const char *old_path,
	__wasi_fd_t new_fd, const char *new_path)
{
	(void)old_fd;
	(void)flags;
	(void)old_path;
	(void)new_fd;
	(void)new_path;
	return __WASI_ERRNO_NOSYS;
}

__wasi_errno_t __wasi_path_open(__wasi_fd_t fd, __wasi_lookupflags_t dirflags, const char *path,
	__wasi_oflags_t oflags, __wasi_rights_t base, __wasi_rights_t inheriting, __wasi_fdflags_t fdflags,
	__wasi_fd_t *r)
{
	(void)fd;
	(void)dirflags;
	(void)path;
	(void)oflags;
	(void)base;
	(void)inheriting;
	(void)fdflags;
	(void)r;
	return __WASI_ERRNO_NOSYS;
}

__wasi_errno_t __wasi_path_readlink(__wasi_fd_t fd, const char *path, uint8_t *buf, __wasi_size_t n,
	__wasi_size_t *r)
{
	(void)fd;
	(void)path;
	(void)buf;
	(void)n;
	(void)r;
	return __WASI_ERRNO_NOSYS;
}

__wasi_errno_t __wasi_path_remove_directory(__wasi_fd_t fd, const char *path)
{
	(void)fd;
	(void)path;
	return __WASI_ERRNO_NOSYS;
}

__wasi_errno_t __wasi_path_rename(__wasi_fd_t fd, const char *old_path, __wasi_fd_t new_fd, const char *new_path)
{
	(void)fd;
	(void)old_path;
	(void)new_fd;
	(void)new_path;
	return __WASI_ERRNO_NOSYS;
}

__wasi_errno_t __wasi_path_symlink(const char *old_path, __wasi_fd_t fd, const char *new_path)
{
	(void)old_path;
	(void)fd;
	(void)new_path;
	return __WASI_ERRNO_NOSYS;
}

__wasi_errno_t __wasi_path_unlink_file(__wasi_fd_t fd, const char *path)
{
	(void)fd;
	(void)path;
	return __WASI_ERRNO_NOSYS;
}
