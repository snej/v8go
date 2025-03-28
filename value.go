// Copyright 2019 Roger Chapman and the v8go contributors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package v8go

/*
#include <stdlib.h>
#include "v8go.h"
static RtnValue NewValueGoString(ContextPtr ctx, _GoString_ str) {
	return NewValueString(ctx, _GoStringPtr(str), _GoStringLen(str)); }
*/
import "C"
import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"strconv"
	"unsafe"
)

// Value represents all Javascript values and objects
type Value struct {
	ref C.ValueRef // C struct containing index into context's value table, plus scope ID
	ctx *Context
}

// Valuer is an interface that reperesents anything that extends from a Value
// eg. Object, Array, Date etc
type Valuer interface {
	value() *Value
}

func (v *Value) value() *Value {
	return v
}

// Undefined returns the `undefined` JS value
func Undefined(iso *Isolate) *Value {
	return iso.undefined
}

// Null returns the `null` JS value
func Null(iso *Isolate) *Value {
	return iso.null
}

func (val *Value) valuePtr() C.ValuePtr {
	if ptr := val.ctx.ptr; ptr != nil {
		return C.ValuePtr{ptr, val.ref}
	} else {
		panic("Attempt to use a v8go.Value after its Context was closed")
	}
}

// NewValue will create a primitive value; see Context.NewValue for details.
// The Value is not associated with any particular
// Context and will remain in memory until the Isolate is closed.
func NewValue(iso *Isolate, val interface{}) (*Value, error) {
	if iso == nil {
		return nil, errors.New("v8go: failed to create new Value: Isolate cannot be <nil>")
	}
	return iso.internalContext.NewValue(val)
}

// NewValue will create a primitive value. The Value is associated with this Context;
// it becomes invalid and must not be used after this Context or its Isolate is closed.
//
// Go types recognized are: bool, int, uint, int32, uint32, int64, uint64, *big.Int,
// float32, float64, json.Number, string, *v8.Value, *v8.Object.
//
// If given an integer outside the range ±2^53, or a big.Int, it will create a BigInt.
//
// As a convenience, if passed a *v8.Value it returns the same Value,
// and if passed a *v8.Object it returns the object's Value.
func (c *Context) NewValue(val interface{}) (*Value, error) {
	ctxPtr := c.ptr
	var ref C.ValueRef
	var err error

	switch v := val.(type) {
	case bool:
		if v {
			return c.iso.trueVal, nil
		} else {
			return c.iso.falseVal, nil
		}
	case string:
		return valueResult(c, C.NewValueGoString(c.ptr, v))
	case int32:
		ref = C.NewValueInteger(ctxPtr, C.int(v))
	case uint32:
		ref = C.NewValueIntegerFromUnsigned(ctxPtr, C.uint(v))
	case int64:
		ref = newValueFromInt64(ctxPtr, v)
	case uint64:
		ref = newValueFromUint64(ctxPtr, v)
	case int:
		ref = newValueFromInt64(ctxPtr, int64(v))
	case uint:
		ref = newValueFromUint64(ctxPtr, uint64(v))
	case float32:
		ref = C.NewValueNumber(ctxPtr, C.double(v))
	case float64:
		ref = C.NewValueNumber(ctxPtr, C.double(v))
	case *big.Int:
		ref, err = newValueFromBigInt(ctxPtr, v)
	case json.Number:
		ref, err = newValueFromJSONNumber(ctxPtr, v)
	case *Value:
		return v, nil
	case *Object:
		return v.Value, nil
	case *Array:
		return v.Value, nil
	default:
		err = ErrUnsupportedValueType
	}

	if err != nil {
		return nil, err
	}
	return &Value{ref, c}, nil
}

var ErrUnsupportedValueType = fmt.Errorf("v8go: unsupported value type")

const kMaxFloat64SafeInt = 1<<53 - 1
const kMinFloat64SafeInt = -kMaxFloat64SafeInt

func newValueFromInt64(ctxPtr C.ContextPtr, v int64) C.ValueRef {
	if v >= kMinFloat64SafeInt && v <= kMaxFloat64SafeInt {
		return C.NewValueNumber(ctxPtr, C.double(v))
	} else {
		return C.NewValueBigInt(ctxPtr, C.int64_t(v))
	}
}

func newValueFromUint64(ctxPtr C.ContextPtr, v uint64) C.ValueRef {
	if v <= kMaxFloat64SafeInt {
		return C.NewValueNumber(ctxPtr, C.double(v))
	} else {
		return C.NewValueBigIntFromUnsigned(ctxPtr, C.uint64_t(v))
	}
}

func newValueFromBigInt(ctxPtr C.ContextPtr, v *big.Int) (C.ValueRef, error) {
	if v.IsInt64() {
		return C.NewValueBigInt(ctxPtr, C.int64_t(v.Int64())), nil
	}

	if v.IsUint64() {
		return C.NewValueBigIntFromUnsigned(ctxPtr, C.uint64_t(v.Uint64())), nil
	}

	var sign, count int
	if v.Sign() == -1 {
		sign = 1
	}
	bits := v.Bits()
	count = len(bits)

	words := make([]C.uint64_t, count, count)
	for idx, word := range bits {
		words[idx] = C.uint64_t(word)
	}

	rtn := C.NewValueBigIntFromWords(ctxPtr, C.int(sign), C.int(count), &words[0])
	if rtn.error.msg != nil {
		return C.ValueRef{}, newJSError(rtn.error)
	}
	return rtn.value, nil
}

func newValueFromJSONNumber(ctxPtr C.ContextPtr, val json.Number) (C.ValueRef, error) {
	if i, err := val.Int64(); err == nil {
		return newValueFromInt64(ctxPtr, i), nil
	} else if numErr, ok := err.(*strconv.NumError); ok && numErr.Err == strconv.ErrRange {
		// if int conversion failed because it's too large, try a big.Int, which will be
		// converted to a JS bigint:
		ibig := new(big.Int)
		if ibig, ok := ibig.SetString(string(val), 10); ok {
			return newValueFromBigInt(ctxPtr, ibig)
		}
	}
	if f, err := val.Float64(); err == nil {
		return C.NewValueNumber(ctxPtr, C.double(f)), nil
	} else {
		return C.ValueRef{}, err
	}
}

// Format implements the fmt.Formatter interface to provide a custom formatter
// primarily to output the detail string (for debugging) with `%+v` verb.
func (v *Value) Format(s fmt.State, verb rune) {
	switch verb {
	case 'v':
		if s.Flag('+') {
			io.WriteString(s, v.DetailString())
			return
		}
		fallthrough
	case 's':
		io.WriteString(s, v.String())
	case 'q':
		fmt.Fprintf(s, "%q", v.String())
	}
}

// ArrayIndex attempts to converts a string to an array index. Returns ok false if conversion fails.
func (v *Value) ArrayIndex() (idx uint32, ok bool) {
	arrayIdx := C.ValueToArrayIndex(v.valuePtr())
	defer C.free(unsafe.Pointer(arrayIdx))
	if arrayIdx == nil {
		return 0, false
	}
	return uint32(*arrayIdx), true
}

// BigInt perform the equivalent of `BigInt(value)` in JS.
func (v *Value) BigInt() *big.Int {
	if v == nil {
		return nil
	}
	bint := C.ValueToBigInt(v.valuePtr())
	defer C.free(unsafe.Pointer(bint.word_array))
	if bint.word_array == nil {
		return nil
	}
	words := (*[1 << 30]big.Word)(unsafe.Pointer(bint.word_array))[:bint.word_count:bint.word_count]

	abs := make([]big.Word, len(words))
	copy(abs, words)

	b := &big.Int{}
	b.SetBits(abs)

	if bint.sign_bit == 1 {
		b.Neg(b)
	}

	return b
}

// Boolean perform the equivalent of `Boolean(value)` in JS. This can never fail.
func (v *Value) Boolean() bool {
	return C.ValueToBoolean(v.valuePtr()) != 0
}

// DetailString provide a string representation of this value usable for debugging.
func (v *Value) DetailString() string {
	rtn := C.ValueToDetailString(v.valuePtr())
	if rtn.data == nil {
		if rtn.error.msg != nil {
			err := newJSError(rtn.error)
			panic(err) // TODO: Return a fallback value
		}
		return ""
	}
	defer C.free(unsafe.Pointer(rtn.data))
	return C.GoStringN(rtn.data, rtn.length)
}

// Int32 perform the equivalent of `Number(value)` in JS and convert the result to a
// signed 32-bit integer by performing the steps in https://tc39.es/ecma262/#sec-toint32.
func (v *Value) Int32() int32 {
	return int32(C.ValueToInt32(v.valuePtr()))
}

// Integer perform the equivalent of `Number(value)` in JS and convert the result to an integer.
// Negative values are rounded up, positive values are rounded down. NaN is converted to 0.
// Infinite values yield undefined results.
func (v *Value) Integer() int64 {
	return int64(C.ValueToInteger(v.valuePtr()))
}

// Number perform the equivalent of `Number(value)` in JS.
func (v *Value) Number() float64 {
	return float64(C.ValueToNumber(v.valuePtr()))
}

// Object perform the equivalent of Object(value) in JS.
// To just cast this value as an Object use AsObject() instead.
func (v *Value) Object() *Object {
	rtn := C.ValueToObject(v.valuePtr())
	obj, err := objectResult(v.ctx, rtn)
	if err != nil {
		panic(err) // TODO: Return error
	}
	return obj
}

// String perform the equivalent of `String(value)` in JS. Primitive values
// are returned as-is, objects will return `[object Object]` and functions will
// print their definition.
func (v *Value) String() string {
	// It's OK to use the Isolate's shared buffer because we already require that client code can
	// only access an Isolate, and Values derived from it, on a single goroutine at a time.
	buffer := v.ctx.iso.stringBuffer
	bufPtr := unsafe.Pointer(&buffer[0])
	s := C.ValueToString(v.valuePtr(), bufPtr, C.int(len(buffer)))
	if unsafe.Pointer(s.data) == bufPtr {
		return string(buffer[0:s.length])
	} else {
		// Result was too big for buffer, so the C++ code malloc-ed its own
		defer C.free(unsafe.Pointer(s.data))
		return C.GoStringN(s.data, C.int(s.length))
	}
}

// Uint32 perform the equivalent of `Number(value)` in JS and convert the result to an
// unsigned 32-bit integer by performing the steps in https://tc39.es/ecma262/#sec-touint32.
func (v *Value) Uint32() uint32 {
	return uint32(C.ValueToUint32(v.valuePtr()))
}

// SameValue returns true if the other value is the same value.
// This is equivalent to `Object.is(v, other)` in JS.
func (v *Value) SameValue(other *Value) bool {
	return C.ValueSameValue(v.valuePtr(), other.valuePtr()) != 0
}

// Enumeration returned by Value.GetType to distinguish between common types of Values.
type ValueType int8

const (
	OtherType ValueType = iota
	UndefinedType
	NullType
	TrueType
	FalseType
	NumberType
	BigIntType
	StringType
	SymbolType
	FunctionType
	ObjectType
)

// GetType returns an enumeration of the most common value types.
func (v *Value) GetType() ValueType {
	return ValueType(C.ValueGetType(v.valuePtr()))
}

// IsUndefined returns true if this value is the undefined value. See ECMA-262 4.3.10.
func (v *Value) IsUndefined() bool {
	return C.ValueIsUndefined(v.valuePtr()) != 0
}

// IsNull returns true if this value is the null value. See ECMA-262 4.3.11.
func (v *Value) IsNull() bool {
	return C.ValueIsNull(v.valuePtr()) != 0
}

// IsNullOrUndefined returns true if this value is either the null or the undefined value.
// See ECMA-262 4.3.11. and 4.3.12
// This is equivalent to `value == null` in JS.
func (v *Value) IsNullOrUndefined() bool {
	return C.ValueIsNullOrUndefined(v.valuePtr()) != 0
}

// IsTrue returns true if this value is true.
// This is not the same as `BooleanValue()`. The latter performs a conversion to boolean,
// i.e. the result of `Boolean(value)` in JS, whereas this checks `value === true`.
func (v *Value) IsTrue() bool {
	return C.ValueIsTrue(v.valuePtr()) != 0
}

// IsFalse returns true if this value is false.
// This is not the same as `!BooleanValue()`. The latter performs a conversion to boolean,
// i.e. the result of `!Boolean(value)` in JS, whereas this checks `value === false`.
func (v *Value) IsFalse() bool {
	return C.ValueIsFalse(v.valuePtr()) != 0
}

// IsName returns true if this value is a symbol or a string.
// This is equivalent to `typeof value === 'string' || typeof value === 'symbol'` in JS.
func (v *Value) IsName() bool {
	return C.ValueIsName(v.valuePtr()) != 0
}

// IsString returns true if this value is an instance of the String type. See ECMA-262 8.4.
// This is equivalent to `typeof value === 'string'` in JS.
func (v *Value) IsString() bool {
	return C.ValueIsString(v.valuePtr()) != 0
}

// IsSymbol returns true if this value is a symbol.
// This is equivalent to `typeof value === 'symbol'` in JS.
func (v *Value) IsSymbol() bool {
	return C.ValueIsSymbol(v.valuePtr()) != 0
}

// IsFunction returns true if this value is a function.
// This is equivalent to `typeof value === 'function'` in JS.
func (v *Value) IsFunction() bool {
	return C.ValueIsFunction(v.valuePtr()) != 0
}

// IsObject returns true if this value is an object.
func (v *Value) IsObject() bool {
	return v.ctx != nil && C.ValueIsObject(v.valuePtr()) != 0
}

// IsBigInt returns true if this value is a bigint.
// This is equivalent to `typeof value === 'bigint'` in JS.
func (v *Value) IsBigInt() bool {
	return C.ValueIsBigInt(v.valuePtr()) != 0
}

// IsBoolean returns true if this value is boolean.
// This is equivalent to `typeof value === 'boolean'` in JS.
func (v *Value) IsBoolean() bool {
	return C.ValueIsBoolean(v.valuePtr()) != 0
}

// IsNumber returns true if this value is a number.
// This is equivalent to `typeof value === 'number'` in JS.
func (v *Value) IsNumber() bool {
	return C.ValueIsNumber(v.valuePtr()) != 0
}

// IsExternal returns true if this value is an `External` object.
func (v *Value) IsExternal() bool {
	// TODO(rogchap): requires test case
	return v.ctx != nil && C.ValueIsExternal(v.valuePtr()) != 0
}

// IsInt32 returns true if this value is a 32-bit signed integer.
func (v *Value) IsInt32() bool {
	return C.ValueIsInt32(v.valuePtr()) != 0
}

// IsUint32 returns true if this value is a 32-bit unsigned integer.
func (v *Value) IsUint32() bool {
	return C.ValueIsUint32(v.valuePtr()) != 0
}

// IsDate returns true if this value is a `Date`.
func (v *Value) IsDate() bool {
	return C.ValueIsDate(v.valuePtr()) != 0
}

// IsArgumentsObject returns true if this value is an Arguments object.
func (v *Value) IsArgumentsObject() bool {
	return C.ValueIsArgumentsObject(v.valuePtr()) != 0
}

// IsBigIntObject returns true if this value is a BigInt object.
func (v *Value) IsBigIntObject() bool {
	return C.ValueIsBigIntObject(v.valuePtr()) != 0
}

// IsNumberObject returns true if this value is a `Number` object.
func (v *Value) IsNumberObject() bool {
	return C.ValueIsNumberObject(v.valuePtr()) != 0
}

// IsStringObject returns true if this value is a `String` object.
func (v *Value) IsStringObject() bool {
	return C.ValueIsStringObject(v.valuePtr()) != 0
}

// IsSymbolObject returns true if this value is a `Symbol` object.
func (v *Value) IsSymbolObject() bool {
	return C.ValueIsSymbolObject(v.valuePtr()) != 0
}

// IsNativeError returns true if this value is a NativeError.
func (v *Value) IsNativeError() bool {
	return C.ValueIsNativeError(v.valuePtr()) != 0
}

// IsRegExp returns true if this value is a `RegExp`.
func (v *Value) IsRegExp() bool {
	return C.ValueIsRegExp(v.valuePtr()) != 0
}

// IsAsyncFunc returns true if this value is an async function.
func (v *Value) IsAsyncFunction() bool {
	return C.ValueIsAsyncFunction(v.valuePtr()) != 0
}

// Is IsGeneratorFunc returns true if this value is a Generator function.
func (v *Value) IsGeneratorFunction() bool {
	return C.ValueIsGeneratorFunction(v.valuePtr()) != 0
}

// IsGeneratorObject returns true if this value is a Generator object (iterator).
func (v *Value) IsGeneratorObject() bool {
	return C.ValueIsGeneratorObject(v.valuePtr()) != 0
}

// IsPromise returns true if this value is a `Promise`.
func (v *Value) IsPromise() bool {
	return C.ValueIsPromise(v.valuePtr()) != 0
}

// IsMap returns true if this value is a `Map`.
func (v *Value) IsMap() bool {
	return C.ValueIsMap(v.valuePtr()) != 0
}

// IsSet returns true if this value is a `Set`.
func (v *Value) IsSet() bool {
	return C.ValueIsSet(v.valuePtr()) != 0
}

// IsMapIterator returns true if this value is a `Map` Iterator.
func (v *Value) IsMapIterator() bool {
	return C.ValueIsMapIterator(v.valuePtr()) != 0
}

// IsSetIterator returns true if this value is a `Set` Iterator.
func (v *Value) IsSetIterator() bool {
	return C.ValueIsSetIterator(v.valuePtr()) != 0
}

// IsWeakMap returns true if this value is a `WeakMap`.
func (v *Value) IsWeakMap() bool {
	return C.ValueIsWeakMap(v.valuePtr()) != 0
}

// IsWeakSet returns true if this value is a `WeakSet`.
func (v *Value) IsWeakSet() bool {
	return C.ValueIsWeakSet(v.valuePtr()) != 0
}

// IsArray returns true if this value is an array.
// Note that it will return false for a `Proxy` of an array.
func (v *Value) IsArray() bool {
	return C.ValueIsArray(v.valuePtr()) != 0
}

// IsArrayBuffer returns true if this value is an `ArrayBuffer`.
func (v *Value) IsArrayBuffer() bool {
	return C.ValueIsArrayBuffer(v.valuePtr()) != 0
}

// IsArrayBufferView returns true if this value is an `ArrayBufferView`.
func (v *Value) IsArrayBufferView() bool {
	return C.ValueIsArrayBufferView(v.valuePtr()) != 0
}

// IsTypedArray returns true if this value is one of TypedArrays.
func (v *Value) IsTypedArray() bool {
	return C.ValueIsTypedArray(v.valuePtr()) != 0
}

// IsUint8Array returns true if this value is an `Uint8Array`.
func (v *Value) IsUint8Array() bool {
	return C.ValueIsUint8Array(v.valuePtr()) != 0
}

// IsUint8ClampedArray returns true if this value is an `Uint8ClampedArray`.
func (v *Value) IsUint8ClampedArray() bool {
	return C.ValueIsUint8ClampedArray(v.valuePtr()) != 0
}

// IsInt8Array returns true if this value is an `Int8Array`.
func (v *Value) IsInt8Array() bool {
	return C.ValueIsInt8Array(v.valuePtr()) != 0
}

// IsUint16Array returns true if this value is an `Uint16Array`.
func (v *Value) IsUint16Array() bool {
	return C.ValueIsUint16Array(v.valuePtr()) != 0
}

// IsInt16Array returns true if this value is an `Int16Array`.
func (v *Value) IsInt16Array() bool {
	return C.ValueIsInt16Array(v.valuePtr()) != 0
}

// IsUint32Array returns true if this value is an `Uint32Array`.
func (v *Value) IsUint32Array() bool {
	return C.ValueIsUint32Array(v.valuePtr()) != 0
}

// IsInt32Array returns true if this value is an `Int32Array`.
func (v *Value) IsInt32Array() bool {
	return C.ValueIsInt32Array(v.valuePtr()) != 0
}

// IsFloat32Array returns true if this value is a `Float32Array`.
func (v *Value) IsFloat32Array() bool {
	return C.ValueIsFloat32Array(v.valuePtr()) != 0
}

// IsFloat64Array returns true if this value is a `Float64Array`.
func (v *Value) IsFloat64Array() bool {
	return C.ValueIsFloat64Array(v.valuePtr()) != 0
}

// IsBigInt64Array returns true if this value is a `BigInt64Array`.
func (v *Value) IsBigInt64Array() bool {
	return C.ValueIsBigInt64Array(v.valuePtr()) != 0
}

// IsBigUint64Array returns true if this value is a BigUint64Array`.
func (v *Value) IsBigUint64Array() bool {
	return C.ValueIsBigUint64Array(v.valuePtr()) != 0
}

// IsDataView returns true if this value is a `DataView`.
func (v *Value) IsDataView() bool {
	return C.ValueIsDataView(v.valuePtr()) != 0
}

// IsSharedArrayBuffer returns true if this value is a `SharedArrayBuffer`.
func (v *Value) IsSharedArrayBuffer() bool {
	return C.ValueIsSharedArrayBuffer(v.valuePtr()) != 0
}

// IsProxy returns true if this value is a JavaScript `Proxy`.
func (v *Value) IsProxy() bool {
	return C.ValueIsProxy(v.valuePtr()) != 0
}

// IsWasmModuleObject returns true if this value is a `WasmModuleObject`.
func (v *Value) IsWasmModuleObject() bool {
	// TODO(rogchap): requires test case
	return C.ValueIsWasmModuleObject(v.valuePtr()) != 0
}

// IsModuleNamespaceObject returns true if the value is a `Module` Namespace `Object`.
func (v *Value) IsModuleNamespaceObject() bool {
	// TODO(rogchap): requires test case
	return C.ValueIsModuleNamespaceObject(v.valuePtr()) != 0
}

// AsObject will cast the value to the Object type. If the value is not an Object
// then an error is returned. Use `value.Object()` to do the JS equivalent of `Object(value)`.
func (v *Value) AsObject() (*Object, error) {
	if !v.IsObject() {
		return nil, errors.New("v8go: value is not an Object")
	}

	return &Object{v}, nil
}

// AsArray will cast the value to the Array type. If the value is not an Array
// then an error is returned.
func (v *Value) AsArray() (*Array, error) {
	if !v.IsArray() {
		return nil, errors.New("v8go: value is not an Array")
	}

	return &Array{Object{v}}, nil
}

func (v *Value) AsPromise() (*Promise, error) {
	if !v.IsPromise() {
		return nil, errors.New("v8go: value is not a Promise")
	}
	return &Promise{&Object{v}}, nil
}

func (v *Value) AsFunction() (*Function, error) {
	if !v.IsFunction() {
		return nil, errors.New("v8go: value is not a Function")
	}
	return &Function{v}, nil
}

// MarshalJSON implements the json.Marshaler interface.
func (v *Value) MarshalJSON() ([]byte, error) {
	jsonStr, err := JSONStringify(nil, v)
	if err != nil {
		return nil, err
	}
	return []byte(jsonStr), nil
}
