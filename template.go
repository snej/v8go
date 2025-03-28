// Copyright 2021 Roger Chapman and the v8go contributors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package v8go

/*
#include <stdlib.h>
#include "v8go.h"
static void TemplateSetValueGo(TemplatePtr ptr,
								_GoString_ name,
								ValuePtr val_ptr,
								int attributes) {
	return TemplateSetValue(ptr, _GoStringPtr(name), _GoStringLen(name), val_ptr, attributes);}
static void TemplateSetTemplateGo(TemplatePtr ptr,
	   							_GoString_ name,
	   							TemplatePtr obj_ptr,
	   							int attributes) {
	return TemplateSetTemplate(ptr, _GoStringPtr(name), _GoStringLen(name),
							   obj_ptr, attributes); }
*/
import "C"
import (
	"errors"
	"fmt"
	"runtime"
)

type template struct {
	ptr C.TemplatePtr
	iso *Isolate
}

// Set adds a property to each instance created by this template.
// The property must be defined either as a primitive value, or a template.
// If the value passed is a Go supported primitive (string, int32, uint32, int64, uint64, float64, big.Int)
// then a value will be created and set as the value property.
func (t *template) Set(name string, val interface{}, attributes ...PropertyAttribute) error {
	var attrs PropertyAttribute
	for _, a := range attributes {
		attrs |= a
	}

	switch v := val.(type) {
	case *ObjectTemplate:
		C.TemplateSetTemplateGo(t.ptr, name, v.ptr, C.int(attrs))
		runtime.KeepAlive(v)
	case *FunctionTemplate:
		C.TemplateSetTemplateGo(t.ptr, name, v.ptr, C.int(attrs))
		runtime.KeepAlive(v)
	case *Value:
		if v.IsObject() || v.IsExternal() {
			return errors.New("v8go: unsupported property: value type must be a primitive or use a template")
		}
		C.TemplateSetValueGo(t.ptr, name, v.valuePtr(), C.int(attrs))
	default:
		newVal, err := NewValue(t.iso, v)
		if err != nil {
			return fmt.Errorf("v8go: unsupported property type `%T`, must be a type supported by NewValue(), or *v8go.ObjectTemplate or *v8go.FunctionTemplate", v)
		}
		C.TemplateSetValueGo(t.ptr, name, newVal.valuePtr(), C.int(attrs))
	}
	runtime.KeepAlive(t)

	return nil
}

func (t *template) finalizer() {
	// Using v8::PersistentBase::Reset() wouldn't be thread-safe to do from
	// this finalizer goroutine so just free the wrapper and let the template
	// itself get cleaned up when the isolate is disposed.
	C.TemplateFreeWrapper(t.ptr)
	t.ptr = nil
}
