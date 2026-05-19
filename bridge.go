package main

/*
#include <stdint.h>
#include <stdlib.h>
#include <string.h>

// Result carrying optional data bytes and optional error string.
typedef struct {
    char* data;
    int   data_len;
    char* error;
    int   error_len;
} CAsyncResult;

// A single KV pair.
typedef struct {
    char* key;
    int   key_len;
    char* value;
    int   value_len;
} CKVPair;

// Result carrying an array of KV pairs.
typedef struct {
    CKVPair* pairs;
    int      count;
    char*    error;
    int      error_len;
} CAsyncKVResult;

// Callback function types (callback/ctx passed as size_t / uintptr_t).
typedef void (*tikv_go_callback)(size_t ctx, CAsyncResult* result);
typedef void (*tikv_go_kv_callback)(size_t ctx, CAsyncKVResult* result);

// Helpers so Go can invoke C function pointers.
static inline void call_callback(size_t cb, size_t ctx, CAsyncResult* result) {
    ((tikv_go_callback)cb)(ctx, result);
}
static inline void call_kv_callback(size_t cb, size_t ctx, CAsyncKVResult* result) {
    ((tikv_go_kv_callback)cb)(ctx, result);
}
*/
import "C"

import (
	"bytes"
	"context"
	"runtime/cgo"
	"unsafe"

	tikverr "github.com/tikv/client-go/v2/error"
	"github.com/tikv/client-go/v2/txnkv"
)

func main() {}

// ---------------------------------------------------------------------------
// Helper: allocate a CAsyncResult on the C heap and populate it.
// Caller owns the returned memory; must call tikv_go_free_async_result.
// ---------------------------------------------------------------------------

func makeErrorResult(msg string) *C.CAsyncResult {
	r := (*C.CAsyncResult)(C.malloc(C.size_t(unsafe.Sizeof(C.CAsyncResult{}))))
	r.data = nil
	r.data_len = 0
	if msg == "" {
		r.error = nil
		r.error_len = 0
	} else {
		r.error = C.CString(msg)
		r.error_len = C.int(len(msg))
	}
	return r
}

func makeDataResult(data []byte) *C.CAsyncResult {
	r := (*C.CAsyncResult)(C.malloc(C.size_t(unsafe.Sizeof(C.CAsyncResult{}))))
	r.error = nil
	r.error_len = 0
	if len(data) == 0 {
		r.data = nil
		r.data_len = 0
	} else {
		r.data = (*C.char)(C.CBytes(data))
		r.data_len = C.int(len(data))
	}
	return r
}

func makeOKResult() *C.CAsyncResult {
	return makeDataResult(nil)
}

func makeKVErrorResult(msg string) *C.CAsyncKVResult {
	r := (*C.CAsyncKVResult)(C.malloc(C.size_t(unsafe.Sizeof(C.CAsyncKVResult{}))))
	r.pairs = nil
	r.count = 0
	if msg == "" {
		r.error = nil
		r.error_len = 0
	} else {
		r.error = C.CString(msg)
		r.error_len = C.int(len(msg))
	}
	return r
}

// ---------------------------------------------------------------------------
// Client management (synchronous – only called once during startup/shutdown)
// ---------------------------------------------------------------------------

// tikv_go_client_new creates a new txnkv.Client.
// addrs is a C array of C strings; count is its length.
// Returns handle (uint64) on success; on error, handle==0 and error is set via
// the returned CAsyncResult (caller frees with tikv_go_free_async_result).
//
//export tikv_go_client_new
func tikv_go_client_new(addrs **C.char, count C.int, out_error **C.char, out_error_len *C.int) C.uint64_t {
	n := int(count)
	pdAddrs := make([]string, n)
	for i := 0; i < n; i++ {
		p := (**C.char)(unsafe.Pointer(uintptr(unsafe.Pointer(addrs)) + uintptr(i)*unsafe.Sizeof(*addrs)))
		pdAddrs[i] = C.GoString(*p)
	}

	client, err := txnkv.NewClient(pdAddrs)
	if err != nil {
		msg := err.Error()
		*out_error = C.CString(msg)
		*out_error_len = C.int(len(msg))
		return 0
	}
	*out_error = nil
	*out_error_len = 0
	h := cgo.NewHandle(client)
	return C.uint64_t(h)
}

// tikv_go_client_destroy closes and frees a client previously created with tikv_go_client_new.
//
//export tikv_go_client_destroy
func tikv_go_client_destroy(client_handle C.uint64_t) {
	if client_handle == 0 {
		return
	}
	h := cgo.Handle(client_handle)
	client := h.Value().(*txnkv.Client)
	_ = client.Close()
	h.Delete()
}

// ---------------------------------------------------------------------------
// Transaction begin (synchronous – lightweight, just a TSO round-trip)
// ---------------------------------------------------------------------------

// tikv_go_txn_begin opens a new optimistic transaction.
// isolation: 0 = ReadCommitted, 1 = SnapshotIsolation (default SI).
// Returns txn handle on success (non-zero), 0 on error.
//
//export tikv_go_txn_begin
func tikv_go_txn_begin(client_handle C.uint64_t, isolation C.int, out_error **C.char, out_error_len *C.int) C.uint64_t {
	client := cgo.Handle(client_handle).Value().(*txnkv.Client)
	txn, err := client.Begin()
	if err != nil {
		msg := err.Error()
		*out_error = C.CString(msg)
		*out_error_len = C.int(len(msg))
		return 0
	}
    txn.SetEnable1PC(true)
    txn.SetEnableAsyncCommit(true)

	*out_error = nil
	*out_error_len = 0
	h := cgo.NewHandle(txn)
	return C.uint64_t(h)
}

// tikv_go_txn_id returns the start timestamp of the transaction (its logical ID).
//
//export tikv_go_txn_id
func tikv_go_txn_id(txn_handle C.uint64_t) C.uint64_t {
	txn := cgo.Handle(txn_handle).Value().(*txnkv.KVTxn)
	return C.uint64_t(txn.StartTS())
}

// tikv_go_txn_destroy frees the cgo handle for a transaction.
// Must be called after commit/rollback to avoid handle leaks.
//
//export tikv_go_txn_destroy
func tikv_go_txn_destroy(txn_handle C.uint64_t) {
	if txn_handle == 0 {
		return
	}
	cgo.Handle(txn_handle).Delete()
}

// ---------------------------------------------------------------------------
// Async operations
// ---------------------------------------------------------------------------

// tikv_go_txn_get_async fetches the value for key asynchronously.
// On completion, calls callback(ctx, result). result must be freed via
// tikv_go_free_async_result. result.data is nil when key is not found (with no error).
//
//export tikv_go_txn_get_async
func tikv_go_txn_get_async(txn_handle C.uint64_t, key *C.char, key_len C.int,
	callback C.size_t, ctx C.size_t) {
	goKey := C.GoBytes(unsafe.Pointer(key), key_len)
	txn := cgo.Handle(txn_handle).Value().(*txnkv.KVTxn)

	go func() {
		val, err := txn.Get(context.Background(), goKey)
		var result *C.CAsyncResult
		if err != nil {
			if tikverr.IsErrNotFound(err) {
				result = makeOKResult() // data==nil means not found, no error
			} else {
				result = makeErrorResult(err.Error())
			}
		} else {
			result = makeDataResult(val)
		}
		C.call_callback(callback, ctx, result)
	}()
}

// tikv_go_txn_put_async writes key=value into the transaction buffer asynchronously.
// Since txn.Set is an in-memory operation, the goroutine completes immediately but
// the async interface keeps it consistent with other operations.
//
//export tikv_go_txn_put_async
func tikv_go_txn_put_async(txn_handle C.uint64_t, key *C.char, key_len C.int,
	val *C.char, val_len C.int, callback C.size_t, ctx C.size_t) {
	goKey := C.GoBytes(unsafe.Pointer(key), key_len)
	goVal := C.GoBytes(unsafe.Pointer(val), val_len)
	txn := cgo.Handle(txn_handle).Value().(*txnkv.KVTxn)

	go func() {
		var result *C.CAsyncResult
		if err := txn.Set(goKey, goVal); err != nil {
			result = makeErrorResult(err.Error())
		} else {
			result = makeOKResult()
		}
		C.call_callback(callback, ctx, result)
	}()
}

// tikv_go_txn_delete_async marks the key for deletion in the transaction buffer.
//
//export tikv_go_txn_delete_async
func tikv_go_txn_delete_async(txn_handle C.uint64_t, key *C.char, key_len C.int,
	callback C.size_t, ctx C.size_t) {
	goKey := C.GoBytes(unsafe.Pointer(key), key_len)
	txn := cgo.Handle(txn_handle).Value().(*txnkv.KVTxn)

	go func() {
		var result *C.CAsyncResult
		if err := txn.Delete(goKey); err != nil {
			result = makeErrorResult(err.Error())
		} else {
			result = makeOKResult()
		}
		C.call_callback(callback, ctx, result)
	}()
}

// tikv_go_txn_batch_get_async fetches multiple keys asynchronously.
// keys is a C array of (char*) pointers; lens is a C array of int lengths.
//
//export tikv_go_txn_batch_get_async
func tikv_go_txn_batch_get_async(txn_handle C.uint64_t, keys **C.char, lens *C.int, count C.int,
	callback C.size_t, ctx C.size_t) {
	n := int(count)
	goKeys := make([][]byte, n)
	for i := 0; i < n; i++ {
		kp := (**C.char)(unsafe.Pointer(uintptr(unsafe.Pointer(keys)) + uintptr(i)*unsafe.Sizeof(*keys)))
		lp := (*C.int)(unsafe.Pointer(uintptr(unsafe.Pointer(lens)) + uintptr(i)*unsafe.Sizeof(*lens)))
		goKeys[i] = C.GoBytes(unsafe.Pointer(*kp), *lp)
	}
	txn := cgo.Handle(txn_handle).Value().(*txnkv.KVTxn)

	go func() {
		resultMap, err := txn.BatchGet(context.Background(), goKeys)
		if err != nil {
			C.call_kv_callback(callback, ctx, makeKVErrorResult(err.Error()))
			return
		}
		// Build C KV array; ordering follows goKeys order (missing keys are skipped).
		pairs := make([]C.CKVPair, 0, len(resultMap))
		for _, k := range goKeys {
			val, ok := resultMap[string(k)]
			if !ok {
				continue
			}
			var p C.CKVPair
			p.key = (*C.char)(C.CBytes(k))
			p.key_len = C.int(len(k))
			if len(val) > 0 {
				p.value = (*C.char)(C.CBytes(val))
				p.value_len = C.int(len(val))
			} else {
				p.value = nil
				p.value_len = 0
			}
			pairs = append(pairs, p)
		}

		r := (*C.CAsyncKVResult)(C.malloc(C.size_t(unsafe.Sizeof(C.CAsyncKVResult{}))))
		r.error = nil
		r.error_len = 0
		if len(pairs) == 0 {
			r.pairs = nil
			r.count = 0
		} else {
			sz := C.size_t(len(pairs)) * C.size_t(unsafe.Sizeof(C.CKVPair{}))
			r.pairs = (*C.CKVPair)(C.malloc(sz))
			for i, p := range pairs {
				dst := (*C.CKVPair)(unsafe.Pointer(uintptr(unsafe.Pointer(r.pairs)) + uintptr(i)*unsafe.Sizeof(C.CKVPair{})))
				*dst = p
			}
			r.count = C.int(len(pairs))
		}
		C.call_kv_callback(callback, ctx, r)
	}()
}

// tikv_go_txn_scan_async scans keys in [start, end] (both inclusive) up to limit entries.
// upperBound for Go Iter is exclusive, so we use a byte-incremented end+1 as upperBound
// and then filter out keys > end.
//
//export tikv_go_txn_scan_async
func tikv_go_txn_scan_async(txn_handle C.uint64_t,
	start *C.char, slen C.int,
	end *C.char, elen C.int,
	limit C.uint64_t,
	callback C.size_t, ctx C.size_t) {
	goStart := C.GoBytes(unsafe.Pointer(start), slen)
	goEnd := C.GoBytes(unsafe.Pointer(end), elen)
	goLimit := uint64(limit)
	txn := cgo.Handle(txn_handle).Value().(*txnkv.KVTxn)

	go func() {
		// Go Iter upperBound is exclusive; compute end+1 for inclusive scan.
		upperBound := incrementBytes(goEnd)

		iter, err := txn.Iter(goStart, upperBound)
		if err != nil {
			C.call_kv_callback(callback, ctx, makeKVErrorResult(err.Error()))
			return
		}
		defer iter.Close()

		var pairs []C.CKVPair
		var count uint64
		for iter.Valid() {
			k := iter.Key()
			// Double-check inclusive upper bound.
			if bytes.Compare(k, goEnd) > 0 {
				break
			}
			v := iter.Value()

			var p C.CKVPair
			p.key = (*C.char)(C.CBytes(k))
			p.key_len = C.int(len(k))
			if len(v) > 0 {
				p.value = (*C.char)(C.CBytes(v))
				p.value_len = C.int(len(v))
			} else {
				p.value = nil
				p.value_len = 0
			}
			pairs = append(pairs, p)
			count++
			if goLimit > 0 && count >= goLimit {
				break
			}
			if err = iter.Next(); err != nil {
				freeCKVPairs(pairs)
				C.call_kv_callback(callback, ctx, makeKVErrorResult(err.Error()))
				return
			}
		}

		r := (*C.CAsyncKVResult)(C.malloc(C.size_t(unsafe.Sizeof(C.CAsyncKVResult{}))))
		r.error = nil
		r.error_len = 0
		if len(pairs) == 0 {
			r.pairs = nil
			r.count = 0
		} else {
			sz := C.size_t(len(pairs)) * C.size_t(unsafe.Sizeof(C.CKVPair{}))
			r.pairs = (*C.CKVPair)(C.malloc(sz))
			for i, p := range pairs {
				dst := (*C.CKVPair)(unsafe.Pointer(uintptr(unsafe.Pointer(r.pairs)) + uintptr(i)*unsafe.Sizeof(C.CKVPair{})))
				*dst = p
			}
			r.count = C.int(len(pairs))
		}
		C.call_kv_callback(callback, ctx, r)
	}()
}

// tikv_go_txn_commit_async commits the transaction asynchronously.
//
//export tikv_go_txn_commit_async
func tikv_go_txn_commit_async(txn_handle C.uint64_t, callback C.size_t, ctx C.size_t) {
	txn := cgo.Handle(txn_handle).Value().(*txnkv.KVTxn)

	go func() {
		var result *C.CAsyncResult
		if err := txn.Commit(context.Background()); err != nil {
			result = makeErrorResult(err.Error())
		} else {
			result = makeOKResult()
		}
		C.call_callback(callback, ctx, result)
	}()
}

// tikv_go_txn_rollback_async rolls back the transaction asynchronously.
//
//export tikv_go_txn_rollback_async
func tikv_go_txn_rollback_async(txn_handle C.uint64_t, callback C.size_t, ctx C.size_t) {
	txn := cgo.Handle(txn_handle).Value().(*txnkv.KVTxn)

	go func() {
		var result *C.CAsyncResult
		if err := txn.Rollback(); err != nil {
			result = makeErrorResult(err.Error())
		} else {
			result = makeOKResult()
		}
		C.call_callback(callback, ctx, result)
	}()
}

// ---------------------------------------------------------------------------
// Memory management
// ---------------------------------------------------------------------------

// tikv_go_free_async_result frees a CAsyncResult allocated by the bridge.
//
//export tikv_go_free_async_result
func tikv_go_free_async_result(r *C.CAsyncResult) {
	if r == nil {
		return
	}
	if r.data != nil {
		C.free(unsafe.Pointer(r.data))
	}
	if r.error != nil {
		C.free(unsafe.Pointer(r.error))
	}
	C.free(unsafe.Pointer(r))
}

// tikv_go_free_kv_result frees a CAsyncKVResult allocated by the bridge.
//
//export tikv_go_free_kv_result
func tikv_go_free_kv_result(r *C.CAsyncKVResult) {
	if r == nil {
		return
	}
	n := int(r.count)
	for i := 0; i < n; i++ {
		p := (*C.CKVPair)(unsafe.Pointer(uintptr(unsafe.Pointer(r.pairs)) + uintptr(i)*unsafe.Sizeof(C.CKVPair{})))
		if p.key != nil {
			C.free(unsafe.Pointer(p.key))
		}
		if p.value != nil {
			C.free(unsafe.Pointer(p.value))
		}
	}
	if r.pairs != nil {
		C.free(unsafe.Pointer(r.pairs))
	}
	if r.error != nil {
		C.free(unsafe.Pointer(r.error))
	}
	C.free(unsafe.Pointer(r))
}

// tikv_go_free_string frees a C string allocated by the bridge (e.g. error strings from
// tikv_go_client_new / tikv_go_txn_begin).
//
//export tikv_go_free_string
func tikv_go_free_string(s *C.char) {
	if s != nil {
		C.free(unsafe.Pointer(s))
	}
}

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

// incrementBytes returns a byte slice that is one greater than b (for use as
// exclusive upper bound in Go's Iter, simulating an inclusive end key).
func incrementBytes(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}
	result := make([]byte, len(b))
	copy(result, b)
	for i := len(result) - 1; i >= 0; i-- {
		result[i]++
		if result[i] != 0 {
			return result
		}
	}
	// Overflow: all bytes were 0xff → unbounded upper end.
	return nil
}

// freeCKVPairs releases C memory inside a Go slice of CKVPair. Used on error paths.
func freeCKVPairs(pairs []C.CKVPair) {
	for _, p := range pairs {
		if p.key != nil {
			C.free(unsafe.Pointer(p.key))
		}
		if p.value != nil {
			C.free(unsafe.Pointer(p.value))
		}
	}
}

