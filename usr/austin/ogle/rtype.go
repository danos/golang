// Copyright 2009 The Go Authors.  All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ogle

import (
	"eval";
	"fmt";
	"log";
	"ptrace";
)

const debugParseRemoteType = false

// A remoteType is the local representation of a type in a remote process.
type remoteType struct {
	eval.Type;
	// The size of values of this type in bytes.
	size int;
	// The field alignment of this type.  Only used for
	// manually-constructed types.
	fieldAlign int;
	// The maker function to turn a remote address of a value of
	// this type into an interpreter Value.
	mk maker;
}

var manualTypes = make(map[Arch] map[eval.Type] *remoteType)

// newManualType constructs a remote type from an interpreter Type
// using the size and alignment properties of the given architecture.
// Most types are parsed directly out of the remote process, but to do
// so we need to layout the structures that describe those types ourselves.
func newManualType(t eval.Type, arch Arch) *remoteType {
	if nt, ok := t.(*eval.NamedType); ok {
		t = nt.Def;
	}

	// Get the type map for this architecture
	typeMap, ok := manualTypes[arch];
	if typeMap == nil {
		typeMap = make(map[eval.Type] *remoteType);
		manualTypes[arch] = typeMap;

		// Construct basic types for this architecture
		basicType := func(t eval.Type, mk maker, size int, fieldAlign int) {
			t = t.(*eval.NamedType).Def;
			if fieldAlign == 0 {
				fieldAlign = size;
			}
			typeMap[t] = &remoteType{t, size, fieldAlign, mk};
		};
		basicType(eval.Uint8Type,   mkUint8,   1, 0);
		basicType(eval.Uint32Type,  mkUint32,  4, 0);
		basicType(eval.UintptrType, mkUintptr, arch.PtrSize(), 0);
		basicType(eval.Int32Type,   mkInt32,   4, 0);
		basicType(eval.IntType,     mkInt,     arch.IntSize(), 0);
		basicType(eval.StringType,  mkString,  arch.PtrSize() + arch.IntSize(), arch.PtrSize());
	}

	if rt, ok := typeMap[t]; ok {
		return rt;
	}

	var rt *remoteType;
	switch t := t.(type) {
	case *eval.PtrType:
		var elem *remoteType;
		mk := func(r remote) eval.Value {
			return remotePtr{r, elem};
		};
		rt = &remoteType{t, arch.PtrSize(), arch.PtrSize(), mk};
		// Construct the element type after registering the
		// type to break cycles.
		typeMap[t] = rt;
		elem = newManualType(t.Elem, arch);

	case *eval.ArrayType:
		elem := newManualType(t.Elem, arch);
		mk := func(r remote) eval.Value {
			return remoteArray{r, t.Len, elem};
		};
		rt = &remoteType{t, elem.size*int(t.Len), elem.fieldAlign, mk};

	case *eval.SliceType:
		elem := newManualType(t.Elem, arch);
		mk := func(r remote) eval.Value {
			return remoteSlice{r, elem};
		};
		rt = &remoteType{t, arch.PtrSize() + 2*arch.IntSize(), arch.PtrSize(), mk};

	case *eval.StructType:
		layout := make([]remoteStructField, len(t.Elems));
		offset := 0;
		fieldAlign := 0;
		for i, f := range t.Elems {
			elem := newManualType(f.Type, arch);
			if fieldAlign == 0 {
				fieldAlign = elem.fieldAlign;
			}
			offset = arch.Align(offset, elem.fieldAlign);
			layout[i].offset = offset;
			layout[i].fieldType = elem;
			offset += elem.size;
		}
		mk := func(r remote) eval.Value {
			return remoteStruct{r, layout};
		};
		rt = &remoteType{t, offset, fieldAlign, mk};

	default:
		log.Crashf("cannot manually construct type %T", t);
	}

	typeMap[t] = rt;
	return rt;
}

var prtIndent = "";

// parseRemoteType parses a Type structure in a remote process to
// construct the corresponding interpreter type and remote type.
func parseRemoteType(rs remoteStruct) *remoteType {
	addr := rs.addr().base;
	p := rs.addr().p;

	// We deal with circular types by discovering cycles at
	// NamedTypes.  If a type cycles back to something other than
	// a named type, we're guaranteed that there will be a named
	// type somewhere in that cycle.  Thus, we continue down,
	// re-parsing types until we reach the named type in the
	// cycle.  In order to still create one remoteType per remote
	// type, we insert an empty remoteType in the type map the
	// first time we encounter the type and re-use that structure
	// the second time we encounter it.

	rt, ok := p.types[addr];
	if ok && rt.Type != nil {
		return rt;
	} else if !ok {
		rt = &remoteType{};
		p.types[addr] = rt;
	}

	if debugParseRemoteType {
		sym := p.syms.SymFromAddr(uint64(addr));
		name := "<unknown>";
		if sym != nil {
			name = sym.Common().Name;
		}
		log.Stderrf("%sParsing type at %#x (%s)", prtIndent, addr, name);
		prtIndent += " ";
		defer func() { prtIndent = prtIndent[0:len(prtIndent)-1] }();
	}

	// Get Type header
	itype := ptrace.Word(rs.Field(p.f.Type.Typ).(remoteUint).Get());
	typ := rs.Field(p.f.Type.Ptr).(remotePtr).Get().(remoteStruct);

	// Is this a named type?
	var nt *eval.NamedType;
	uncommon := typ.Field(p.f.CommonType.UncommonType).(remotePtr).Get();
	if uncommon != nil {
		name := uncommon.(remoteStruct).Field(p.f.UncommonType.Name).(remotePtr).Get();
		if name != nil {
			// TODO(austin) Declare type in appropriate remote package
			nt = eval.NewNamedType(name.(remoteString).Get());
			rt.Type = nt;
		}
	}

	// Create type
	var t eval.Type;
	var mk maker;
	switch itype {
	case p.runtime.PBoolType:
		t = eval.BoolType;
		mk = mkBool;
	case p.runtime.PUint8Type:
		t = eval.Uint8Type;
		mk = mkUint8;
	case p.runtime.PUint16Type:
		t = eval.Uint16Type;
		mk = mkUint16;
	case p.runtime.PUint32Type:
		t = eval.Uint32Type;
		mk = mkUint32;
	case p.runtime.PUint64Type:
		t = eval.Uint64Type;
		mk = mkUint64;
	case p.runtime.PUintType:
		t = eval.UintType;
		mk = mkUint;
	case p.runtime.PUintptrType:
		t = eval.UintptrType;
		mk = mkUintptr;
	case p.runtime.PInt8Type:
		t = eval.Int8Type;
		mk = mkInt8;
	case p.runtime.PInt16Type:
		t = eval.Int16Type;
		mk = mkInt16;
	case p.runtime.PInt32Type:
		t = eval.Int32Type;
		mk = mkInt32;
	case p.runtime.PInt64Type:
		t = eval.Int64Type;
		mk = mkInt64;
	case p.runtime.PIntType:
		t = eval.IntType;
		mk = mkInt;
	case p.runtime.PFloat32Type:
		t = eval.Float32Type;
		mk = mkFloat32;
	case p.runtime.PFloat64Type:
		t = eval.Float64Type;
		mk = mkFloat64;
	case p.runtime.PFloatType:
		t = eval.FloatType;
		mk = mkFloat;
	case p.runtime.PStringType:
		t = eval.StringType;
		mk = mkString;

	case p.runtime.PArrayType:
		// Cast to an ArrayType
		typ := p.runtime.ArrayType.mk(typ.addr()).(remoteStruct);
		len := int64(typ.Field(p.f.ArrayType.Len).(remoteUint).Get());
		elem := parseRemoteType(typ.Field(p.f.ArrayType.Elem).(remotePtr).Get().(remoteStruct));
		t = eval.NewArrayType(len, elem.Type);
		mk = func(r remote) eval.Value {
			return remoteArray{r, len, elem};
		};

	case p.runtime.PStructType:
		// Cast to a StructType
		typ := p.runtime.StructType.mk(typ.addr()).(remoteStruct);
		fs := typ.Field(p.f.StructType.Fields).(remoteSlice).Get();

		fields := make([]eval.StructField, fs.Len);
		layout := make([]remoteStructField, fs.Len);
		for i := range fields {
			f := fs.Base.Elem(int64(i)).(remoteStruct);
			elemrs := f.Field(p.f.StructField.Typ).(remotePtr).Get().(remoteStruct);
			elem := parseRemoteType(elemrs);
			fields[i].Type = elem.Type;
			name := f.Field(p.f.StructField.Name).(remotePtr).Get();
			if name == nil {
				fields[i].Anonymous = true;
			} else {
				fields[i].Name = name.(remoteString).Get();
			}
			layout[i].offset = int(f.Field(p.f.StructField.Offset).(remoteUint).Get());
			layout[i].fieldType = elem;
		}

		t = eval.NewStructType(fields);
		mk = func(r remote) eval.Value {
			return remoteStruct{r, layout};
		};

	case p.runtime.PPtrType:
		// Cast to a PtrType
		typ := p.runtime.PtrType.mk(typ.addr()).(remoteStruct);
		elem := parseRemoteType(typ.Field(p.f.PtrType.Elem).(remotePtr).Get().(remoteStruct));
		t = eval.NewPtrType(elem.Type);
		mk = func(r remote) eval.Value {
			return remotePtr{r, elem};
		};

	case p.runtime.PSliceType:
		// Cast to a SliceType
		typ := p.runtime.SliceType.mk(typ.addr()).(remoteStruct);
		elem := parseRemoteType(typ.Field(p.f.SliceType.Elem).(remotePtr).Get().(remoteStruct));
		t = eval.NewSliceType(elem.Type);
		mk = func(r remote) eval.Value {
			return remoteSlice{r, elem};
		};

	case p.runtime.PMapType, p.runtime.PChanType, p.runtime.PFuncType, p.runtime.PInterfaceType, p.runtime.PUnsafePointerType, p.runtime.PDotDotDotType:
		// TODO(austin)
		t = eval.UintptrType;
		mk = mkUintptr;

	default:
		sym := p.syms.SymFromAddr(uint64(itype));
		name := "<unknown symbol>";
		if sym != nil {
			name = sym.Common().Name;
		}
		err := fmt.Sprintf("runtime type at %#x has unexpected type %#x (%s)", addr, itype, name);
		eval.Abort(FormatError(err));
	}

	// Fill in the remote type
	if nt != nil {
		nt.Complete(t);
	} else {
		rt.Type = t;
	}
	rt.size = int(typ.Field(p.f.CommonType.Size).(remoteUint).Get());
	rt.mk = mk;

	return rt;
}