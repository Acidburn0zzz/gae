// Copyright 2015 The Chromium Authors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// HEAVILY adapted from github.com/golang/appengine/datastore

package rawdatastore

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/luci/gae/service/blobstore"
	"github.com/luci/luci-go/common/errors"
)

// Entities with more than this many indexed properties will not be saved.
const maxIndexedProperties = 20000

type structTag struct {
	name           string
	idxSetting     IndexSetting
	isSlice        bool
	substructCodec *structCodec
	convert        bool
	metaVal        interface{}
	canSet         bool
}

type structCodec struct {
	byMeta   map[string]int
	byName   map[string]int
	byIndex  []structTag
	hasSlice bool
	problem  error
}

type structPLS struct {
	o reflect.Value
	c *structCodec
}

var _ PropertyLoadSaver = (*structPLS)(nil)

// typeMismatchReason returns a string explaining why the property p could not
// be stored in an entity field of type v.Type().
func typeMismatchReason(val interface{}, v reflect.Value) string {
	entityType := reflect.TypeOf(val)
	return fmt.Sprintf("type mismatch: %s versus %v", entityType, v.Type())
}

func (p *structPLS) Load(propMap PropertyMap) error {
	if err := p.Problem(); err != nil {
		return err
	}

	convFailures := errors.MultiError(nil)

	t := reflect.Type(nil)
	for name, props := range propMap {
		multiple := len(props) > 1
		for i, prop := range props {
			if reason := loadInner(p.c, p.o, i, name, prop, multiple); reason != "" {
				if t == nil {
					t = p.o.Type()
				}
				convFailures = append(convFailures, &ErrFieldMismatch{
					StructType: t,
					FieldName:  name,
					Reason:     reason,
				})
			}
		}
	}

	if len(convFailures) > 0 {
		return convFailures
	}

	return nil
}

func loadInner(codec *structCodec, structValue reflect.Value, index int, name string, p Property, requireSlice bool) string {
	var v reflect.Value
	// Traverse a struct's struct-typed fields.
	for {
		fieldIndex, ok := codec.byName[name]
		if !ok {
			return "no such struct field"
		}
		v = structValue.Field(fieldIndex)

		st := codec.byIndex[fieldIndex]
		if st.substructCodec == nil {
			break
		}

		if v.Kind() == reflect.Slice {
			for v.Len() <= index {
				v.Set(reflect.Append(v, reflect.New(v.Type().Elem()).Elem()))
			}
			structValue = v.Index(index)
			requireSlice = false
		} else {
			structValue = v
		}
		// Strip the "I." from "I.X".
		name = name[len(st.name):]
		codec = st.substructCodec
	}

	doConversion := func(v reflect.Value) (string, bool) {
		a := v.Addr()
		if conv, ok := a.Interface().(PropertyConverter); ok {
			err := conv.FromProperty(p)
			if err != nil {
				return err.Error(), true
			}
			return "", true
		}
		return "", false
	}

	if ret, ok := doConversion(v); ok {
		return ret
	}

	var slice reflect.Value
	if v.Kind() == reflect.Slice && v.Type().Elem().Kind() != reflect.Uint8 {
		slice = v
		v = reflect.New(v.Type().Elem()).Elem()
	} else if requireSlice {
		return "multiple-valued property requires a slice field type"
	}

	pVal := p.Value()

	if ret, ok := doConversion(v); ok {
		if ret != "" {
			return ret
		}
	} else {
		knd := v.Kind()
		if v.Type().Implements(typeOfKey) {
			knd = reflect.Interface
		}
		switch knd {
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			x, ok := pVal.(int64)
			if !ok && pVal != nil {
				return typeMismatchReason(pVal, v)
			}
			if v.OverflowInt(x) {
				return fmt.Sprintf("value %v overflows struct field of type %v", x, v.Type())
			}
			v.SetInt(x)
		case reflect.Bool:
			x, ok := pVal.(bool)
			if !ok && pVal != nil {
				return typeMismatchReason(pVal, v)
			}
			v.SetBool(x)
		case reflect.String:
			switch x := pVal.(type) {
			case blobstore.Key:
				v.SetString(string(x))
			case string:
				v.SetString(x)
			default:
				if pVal != nil {
					return typeMismatchReason(pVal, v)
				}
			}
		case reflect.Float32, reflect.Float64:
			x, ok := pVal.(float64)
			if !ok && pVal != nil {
				return typeMismatchReason(pVal, v)
			}
			if v.OverflowFloat(x) {
				return fmt.Sprintf("value %v overflows struct field of type %v", x, v.Type())
			}
			v.SetFloat(x)
		case reflect.Interface:
			x, ok := pVal.(Key)
			if !ok && pVal != nil {
				return typeMismatchReason(pVal, v)
			}
			if x != nil {
				v.Set(reflect.ValueOf(x))
			}
		case reflect.Struct:
			switch v.Type() {
			case typeOfTime:
				x, ok := pVal.(time.Time)
				if !ok && pVal != nil {
					return typeMismatchReason(pVal, v)
				}
				v.Set(reflect.ValueOf(x))
			case typeOfGeoPoint:
				x, ok := pVal.(GeoPoint)
				if !ok && pVal != nil {
					return typeMismatchReason(pVal, v)
				}
				v.Set(reflect.ValueOf(x))
			default:
				panic(fmt.Errorf("helper: impossible: %s", typeMismatchReason(pVal, v)))
			}
		case reflect.Slice:
			switch x := pVal.(type) {
			case []byte:
				v.SetBytes(x)
			case ByteString:
				v.SetBytes([]byte(x))
			default:
				panic(fmt.Errorf("helper: impossible: %s", typeMismatchReason(pVal, v)))
			}
		default:
			panic(fmt.Errorf("helper: impossible: %s", typeMismatchReason(pVal, v)))
		}
	}
	if slice.IsValid() {
		slice.Set(reflect.Append(slice, v))
	}
	return ""
}

func (p *structPLS) Save(withMeta bool) (PropertyMap, error) {
	size := len(p.c.byName)
	if withMeta {
		size += len(p.c.byMeta)
	}
	ret := make(PropertyMap, size)
	if _, err := p.save(ret, "", ShouldIndex); err != nil {
		return nil, err
	}
	if withMeta {
		for k := range p.c.byMeta {
			val, err := p.GetMeta(k)
			if err != nil {
				return nil, err // TODO(riannucci): should these be ignored?
			}
			p := Property{}
			if err = p.SetValue(val, NoIndex); err != nil {
				return nil, err
			}
			ret["$"+k] = []Property{p}
		}
	}
	return ret, nil
}

func (p *structPLS) save(propMap PropertyMap, prefix string, is IndexSetting) (idxCount int, err error) {
	if err = p.Problem(); err != nil {
		return
	}

	saveProp := func(name string, si IndexSetting, v reflect.Value, st *structTag) (err error) {
		if st.substructCodec != nil {
			count, err := (&structPLS{v, st.substructCodec}).save(propMap, name, si)
			if err == nil {
				idxCount += count
				if idxCount > maxIndexedProperties {
					err = errors.New("gae: too many indexed properties")
				}
			}
			return err
		}

		prop := Property{}
		if st.convert {
			prop, err = v.Addr().Interface().(PropertyConverter).ToProperty()
		} else {
			err = prop.SetValue(v.Interface(), si)
		}
		if err != nil {
			return err
		}
		propMap[name] = append(propMap[name], prop)
		if prop.IndexSetting() == ShouldIndex {
			idxCount++
			if idxCount > maxIndexedProperties {
				return errors.New("gae: too many indexed properties")
			}
		}
		return nil
	}

	for i, st := range p.c.byIndex {
		if st.name == "-" {
			continue
		}
		name := st.name
		if prefix != "" {
			name = prefix + name
		}
		v := p.o.Field(i)
		is1 := is
		if st.idxSetting == NoIndex {
			is1 = NoIndex
		}
		if st.isSlice {
			for j := 0; j < v.Len(); j++ {
				if err = saveProp(name, is1, v.Index(j), &st); err != nil {
					return
				}
			}
		} else {
			if err = saveProp(name, is1, v, &st); err != nil {
				return
			}
		}
	}
	return
}

func (p *structPLS) GetMeta(key string) (interface{}, error) {
	if err := p.Problem(); err != nil {
		return nil, err
	}
	idx, ok := p.c.byMeta[key]
	if !ok {
		return nil, ErrMetaFieldUnset
	}
	st := p.c.byIndex[idx]
	val := st.metaVal
	f := p.o.Field(idx)
	if st.canSet {
		if !reflect.DeepEqual(reflect.Zero(f.Type()).Interface(), f.Interface()) {
			val = f.Interface()
			if bf, ok := val.(Toggle); ok {
				val = bf == On // true if On, otherwise false
			}
		}
	}
	return val, nil
}

func (p *structPLS) SetMeta(key string, val interface{}) (err error) {
	if err = p.Problem(); err != nil {
		return
	}
	idx, ok := p.c.byMeta[key]
	if !ok {
		return ErrMetaFieldUnset
	}
	if !p.c.byIndex[idx].canSet {
		return fmt.Errorf("gae/helper: cannot set meta %q: unexported field", key)
	}
	// setting a BoolField
	if b, ok := val.(bool); ok {
		if b {
			val = On
		} else {
			val = Off
		}
	}
	p.o.Field(idx).Set(reflect.ValueOf(val))
	return nil
}

func (p *structPLS) Problem() error { return p.c.problem }

var (
	// The RWMutex is chosen intentionally, as the majority of access to the
	// structCodecs map will be in parallel and will be to read an existing codec.
	// There's no reason to serialize goroutines on every
	// gae.RawDatastore.{Get,Put}{,Multi} call.
	structCodecsMutex sync.RWMutex
	structCodecs      = map[reflect.Type]*structCodec{}
)

// validPropertyName returns whether name consists of one or more valid Go
// identifiers joined by ".".
func validPropertyName(name string) bool {
	if name == "" {
		return false
	}
	for _, s := range strings.Split(name, ".") {
		if s == "" {
			return false
		}
		first := true
		for _, c := range s {
			if first {
				first = false
				if c != '_' && !unicode.IsLetter(c) {
					return false
				}
			} else {
				if c != '_' && !unicode.IsLetter(c) && !unicode.IsDigit(c) {
					return false
				}
			}
		}
	}
	return true
}

var (
	errRecursiveStruct = fmt.Errorf("(internal): struct type is recursively defined")
)

func getStructCodecLocked(t reflect.Type) (c *structCodec) {
	if c, ok := structCodecs[t]; ok {
		return c
	}

	me := func(fmtStr string, args ...interface{}) error {
		return fmt.Errorf(fmtStr, args...)
	}

	c = &structCodec{
		byIndex: make([]structTag, t.NumField()),
		byName:  make(map[string]int, t.NumField()),
		byMeta:  make(map[string]int, t.NumField()),
		problem: errRecursiveStruct, // we'll clear this later if it's not recursive
	}
	defer func() {
		// If the codec has a problem, free up the indexes
		if c.problem != nil {
			c.byIndex = nil
			c.byName = nil
			c.byMeta = nil
		}
	}()
	structCodecs[t] = c

	for i := range c.byIndex {
		st := &c.byIndex[i]
		f := t.Field(i)
		name := f.Tag.Get("gae")
		opts := ""
		if i := strings.Index(name, ","); i != -1 {
			name, opts = name[:i], name[i+1:]
		}
		st.canSet = f.PkgPath == "" // blank == exported
		switch {
		case name == "":
			if !f.Anonymous {
				name = f.Name
			}
		case name[0] == '$':
			name = name[1:]
			if _, ok := c.byMeta[name]; ok {
				c.problem = me("meta field %q set multiple times", "$"+name)
				return
			}
			c.byMeta[name] = i
			mv, err := convertMeta(opts, f.Type)
			if err != nil {
				c.problem = me("meta field %q has bad type: %s", "$"+name, err)
				return
			}
			st.metaVal = mv
			fallthrough
		case name == "-":
			st.name = "-"
			continue
		default:
			if !validPropertyName(name) {
				c.problem = me("struct tag has invalid property name: %q", name)
				return
			}
		}
		if !st.canSet {
			st.name = "-"
			continue
		}

		substructType := reflect.Type(nil)
		ft := f.Type
		if reflect.PtrTo(ft).Implements(typeOfPropertyConverter) {
			st.convert = true
		} else {
			switch f.Type.Kind() {
			case reflect.Struct:
				if ft != typeOfTime && ft != typeOfGeoPoint {
					substructType = ft
				}
			case reflect.Slice:
				if reflect.PtrTo(ft.Elem()).Implements(typeOfPropertyConverter) {
					st.convert = true
				} else if ft.Elem().Kind() == reflect.Struct {
					substructType = ft.Elem()
				}
				st.isSlice = ft.Elem().Kind() != reflect.Uint8
				c.hasSlice = c.hasSlice || st.isSlice
			case reflect.Interface:
				if ft != typeOfKey {
					c.problem = me("field %q has non-concrete interface type %s",
						f.Name, f.Type)
					return
				}
			}
		}

		if substructType != nil {
			sub := getStructCodecLocked(substructType)
			if sub.problem != nil {
				if sub.problem == errRecursiveStruct {
					c.problem = me("field %q is recursively defined", f.Name)
				} else {
					c.problem = me("field %q has problem: %s", f.Name, sub.problem)
				}
				return
			}
			st.substructCodec = sub
			if st.isSlice && sub.hasSlice {
				c.problem = me(
					"flattening nested structs leads to a slice of slices: field %q",
					f.Name)
				return
			}
			c.hasSlice = c.hasSlice || sub.hasSlice
			if name != "" {
				name += "."
			}
			for relName := range sub.byName {
				absName := name + relName
				if _, ok := c.byName[absName]; ok {
					c.problem = me("struct tag has repeated property name: %q", absName)
					return
				}
				c.byName[absName] = i
			}
		} else {
			if !st.convert { // check the underlying static type of the field
				t := ft
				if st.isSlice {
					t = t.Elem()
				}
				v := reflect.New(t).Elem().Interface()
				v, _ = UpconvertUnderlyingType(v, t)
				if _, err := PropertyTypeOf(v, false); err != nil {
					c.problem = me("field %q has invalid type: %s", name, ft)
					return
				}
			}

			if _, ok := c.byName[name]; ok {
				c.problem = me("struct tag has repeated property name: %q", name)
				return
			}
			c.byName[name] = i
		}
		st.name = name
		if opts == "noindex" {
			st.idxSetting = NoIndex
		}
	}
	if c.problem == errRecursiveStruct {
		c.problem = nil
	}
	return
}

func convertMeta(val string, t reflect.Type) (interface{}, error) {
	switch t {
	case typeOfString:
		return val, nil
	case typeOfInt64:
		if val == "" {
			return int64(0), nil
		}
		return strconv.ParseInt(val, 10, 64)
	case typeOfToggle:
		switch val {
		case "on", "On", "true":
			return true, nil
		case "off", "Off", "false":
			return false, nil
		}
		return nil, fmt.Errorf("Toggle field has bad/missing default, got %q", val)
	}
	return nil, fmt.Errorf("helper: meta field with bad type/value %s/%q", t, val)
}