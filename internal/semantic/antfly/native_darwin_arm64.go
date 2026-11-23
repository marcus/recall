//go:build cgo

package antfly

/*
#include <dlfcn.h>
#include <stdint.h>
#include <stddef.h>
#include <stdlib.h>

// The small ABI-3 subset used here matches the v0.3.2 Apache-2.0 C header.
// No native header, linker search path, Python, or Go module is needed.
typedef struct antfly_db antfly_db;
typedef struct { const uint8_t *ptr; size_t len; } af_slice;
typedef struct { uint8_t *ptr; size_t len; } af_buffer;
static void *af_library;
static uint32_t (*af_abi)(void);
static const char *(*af_detail)(void);
static const char *(*af_error_name)(int);
static int (*af_create)(const char *, antfly_db **);
static int (*af_open)(const char *, antfly_db **);
static void (*af_close)(antfly_db *);
static int (*af_add)(antfly_db *, af_slice);
static int (*af_batch)(antfly_db *, af_slice, af_buffer *);
static int (*af_search)(antfly_db *, af_slice, af_buffer *);
static int (*af_indexes)(antfly_db *, af_buffer *);
static int (*af_drain)(antfly_db *);
static void (*af_free)(af_buffer *);

static const char *af_load(const char *path) {
 af_library = dlopen(path, RTLD_NOW | RTLD_LOCAL);
 if (!af_library) return dlerror();
 #define AF_SYMBOL(target, name) do { *(void **)(&(target)) = dlsym(af_library, name); if (!(target)) return name; } while (0)
 AF_SYMBOL(af_abi, "antfly_abi_version");
 AF_SYMBOL(af_detail, "antfly_last_error_detail");
 AF_SYMBOL(af_error_name, "antfly_error_code_name");
 AF_SYMBOL(af_create, "antfly_lite_create_hosted");
 AF_SYMBOL(af_open, "antfly_lite_open_readonly");
 AF_SYMBOL(af_close, "antfly_db_close");
 AF_SYMBOL(af_add, "antfly_db_add_index_json");
 AF_SYMBOL(af_batch, "antfly_db_batch_json");
 AF_SYMBOL(af_search, "antfly_db_search_json");
 AF_SYMBOL(af_indexes, "antfly_db_list_indexes_json");
 AF_SYMBOL(af_drain, "antfly_db_run_until_idle");
 AF_SYMBOL(af_free, "antfly_buffer_free");
 #undef AF_SYMBOL
 if (af_abi() != 3) return "expected Antfly ABI 3";
 return NULL;
}
static int af_open_db(const char *path, int create, antfly_db **db) {
 return create ? af_create(path, db) : af_open(path, db);
}
static int af_call(antfly_db *db, int op, const void *input, size_t len, af_buffer *out) {
 af_slice slice = { input, len };
 switch (op) {
 case 0: return af_add(db, slice);
 case 1: return af_batch(db, slice, out);
 case 2: return af_drain(db);
 case 3: return af_indexes(db, out);
 case 4: return af_search(db, slice, out);
 default: return -1;
 }
}
static const char *af_last_detail(void) { return af_detail(); }
static const char *af_code_name(int code) { return af_error_name(code); }
static void af_close_db(antfly_db *db) { af_close(db); }
static void af_free_buffer(af_buffer *buf) { af_free(buf); }
*/
import "C"

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"unsafe"
)

// Pin the same darwin-arm64 v0.3.2 embedded artifact as the local proof.
const librarySHA256 = "1144d7f0d28978b4d75ab83a0c5a5d0c10c01246d64b594e5458112788b7e08b"

var nativeRuntime struct {
	sync.Once
	err error
}

// The successful library stays loaded for process lifetime. Closing a database
// releases its resources; unloading the process-wide runtime is unsafe while
// another native handle or runtime thread might still exist.
func loadNative() error {
	nativeRuntime.Do(func() { nativeRuntime.err = loadLibrary() })
	return nativeRuntime.err
}
func loadLibrary() error {
	path := os.Getenv("ANTFLY_LIBRARY")
	if path == "" {
		return fmt.Errorf("%w: set ANTFLY_LIBRARY to the pinned v0.3.2 libantfly.dylib (see package README)", ErrUnavailable)
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("%w: library path: %w", ErrUnavailable, err)
	}
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	hash := sha256.New()
	_, hashErr := io.Copy(hash, file)
	closeErr := file.Close()
	if hashErr != nil || closeErr != nil {
		return fmt.Errorf("%w: hash library: %w", ErrUnavailable, errors.Join(hashErr, closeErr))
	}
	if got := fmt.Sprintf("%x", hash.Sum(nil)); got != librarySHA256 {
		return fmt.Errorf("%w: libantfly SHA-256 %s, want %s (v0.3.2 darwin-arm64)", ErrUnavailable, got, librarySHA256)
	}
	cPath := C.CString(path)
	defer C.free(unsafe.Pointer(cPath))
	if detail := C.af_load(cPath); detail != nil {
		return fmt.Errorf("%w: load library: %s", ErrUnavailable, C.GoString(detail))
	}
	return nil
}

type nativeDB struct{ handle *C.antfly_db }

func openNative(path string, create bool) (database, error) {
	// Never let a build accidentally reopen or overwrite a published index.
	if create {
		if _, err := os.Lstat(path); err == nil {
			return nil, fmt.Errorf("antfly: destination already exists: %s", path)
		} else if !os.IsNotExist(err) {
			return nil, fmt.Errorf("antfly: destination: %w", err)
		}
	}
	if err := loadNative(); err != nil {
		return nil, err
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	cPath := C.CString(path)
	defer C.free(unsafe.Pointer(cPath))
	var handle *C.antfly_db
	mode := C.int(0)
	if create {
		mode = 1
	}
	if err := nativeError(C.af_open_db(cPath, mode, &handle)); err != nil { //nolint:gocritic // cgo inserts a pointer check with duplicate operands.
		return nil, fmt.Errorf("antfly: open: %w", err)
	}
	return &nativeDB{handle: handle}, nil
}
func (db *nativeDB) call(op string, input []byte) ([]byte, error) {
	var operation C.int
	switch op {
	case "add_index":
		operation = 0
	case "batch":
		operation = 1
	case "drain":
		operation = 2
	case "indexes":
		operation = 3
	case "search":
		operation = 4
	default:
		return nil, fmt.Errorf("unknown native operation %q", op)
	}
	var ptr unsafe.Pointer
	if len(input) > 0 {
		ptr = C.CBytes(input)
		defer C.free(ptr)
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	var out C.af_buffer
	code := C.af_call(db.handle, operation, ptr, C.size_t(len(input)), &out) //nolint:gocritic // cgo inserts a pointer check with duplicate operands.
	err := nativeError(code)                                                 // Native diagnostics are thread-local; read immediately.
	defer C.af_free_buffer(&out)                                             //nolint:gocritic // cgo inserts a pointer check with duplicate operands.
	if err != nil {
		return nil, err
	}
	if out.len == 0 {
		return nil, nil
	}
	return append([]byte(nil), unsafe.Slice((*byte)(unsafe.Pointer(out.ptr)), int(out.len))...), nil
}
func nativeError(code C.int) error {
	if code == 0 {
		return nil
	}
	return fmt.Errorf("%s (%d): %s", C.GoString(C.af_code_name(code)), int(code), C.GoString(C.af_last_detail()))
}
func (db *nativeDB) close() error {
	if db.handle != nil {
		C.af_close_db(db.handle)
		db.handle = nil
	}
	return nil
}
