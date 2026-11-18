package modclient

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

// A Shape says what a response must look like for the adapter that reads it: which keys are required, of which JSON types, which may be absent or null, and what the nested
// objects and array elements look like. It is checked on the decoded document BEFORE anything is copied into the caller's value, so a missing key or a wrong type is a
// Contract error that names the route and the key, never a zero field that reads as an answer (a missing `complete` that decodes to false is exactly the kind of invented
// outcome this package exists to prevent).
//
// A key the Shape does not list is tolerated: the modules' API changes are additive, and an added key must not break the platform. The zero Shape checks nothing.
//
//	modclient.Object(
//	    modclient.Req("job_id", modclient.Str),
//	    modclient.Req("counts", modclient.Object(modclient.Req("total", modclient.Int))),
//	    modclient.Opt("provider_message_id", modclient.Str),          // omitted until set
//	    modclient.Req("reason", modclient.Str.OrNull()),              // always present, null when there is nothing to say
//	    modclient.Req("results", modclient.ArrayOf(modclient.Object(modclient.Req("state", modclient.Str)))),
//	)
type Shape struct {
	kind     shapeKind
	fields   []Field
	elem     *Shape
	nullable bool
	open     bool
}

type shapeKind uint8

const (
	kUnchecked shapeKind = iota // the zero Shape
	kAny                        // present, any JSON value (null only with OrNull)
	kString
	kNumber
	kInteger
	kBool
	kObject
	kMap
	kArray
)

// Field is one key of an object Shape.
type Field struct {
	Name     string
	Shape    Shape
	Required bool
}

// The scalar shapes. Int is a JSON number written as an integer that fits in 64 bits (a module's counts and prices; `1.0` and `1e3` are not integers here). Num is any JSON
// number. Any is any JSON value but null.
var (
	Str  = Shape{kind: kString}
	Num  = Shape{kind: kNumber}
	Int  = Shape{kind: kInteger}
	Bool = Shape{kind: kBool}
	Any  = Shape{kind: kAny}
)

// Object is a JSON object with the listed keys. Other keys are tolerated.
func Object(fields ...Field) Shape {
	return Shape{kind: kObject, fields: append([]Field(nil), fields...)}
}

// MapOf is a JSON object with any keys whose every value has the shape elem (module readiness `checks`, describe `vocabularies`).
func MapOf(elem Shape) Shape { return Shape{kind: kMap, elem: &elem} }

// ArrayOf is a JSON array whose every element has the shape elem.
func ArrayOf(elem Shape) Shape { return Shape{kind: kArray, elem: &elem} }

// OrNull returns the shape that also accepts JSON null.
func (s Shape) OrNull() Shape {
	s.nullable = true
	return s
}

// Open returns the shape marked as an open object: a bag of keys the module defines and fills with whichever it produced for this response (module 1's `signals` carries the keys
// the pipeline layers that ran for a row produced, so which keys a row has varies from row to row). Decoding is exactly what it was (an object shape has always tolerated keys it
// does not list, and still checks the ones it lists); the mark is for tools that compare responses, which must not call the presence of a key the Shape does not list on one side
// only a difference. It means something on an Object shape and is ignored on any other (a MapOf already has free keys).
func (s Shape) Open() Shape {
	s.open = true
	return s
}

// Req is a key that must be present (with a value of the shape; null only if the shape says OrNull).
func Req(name string, s Shape) Field { return Field{Name: name, Shape: s, Required: true} }

// Opt is a key that may be absent. When present it must have the shape (null only if the shape says OrNull): "omitted until set" and "null when unset" are different
// conventions in the modules (module 2's single report omits, its campaign items carry null) and the Shape keeps them apart.
func Opt(name string, s Shape) Field { return Field{Name: name, Shape: s} }

func (k shapeKind) String() string {
	switch k {
	case kAny:
		return "any value"
	case kString:
		return "string"
	case kNumber:
		return "number"
	case kInteger:
		return "integer"
	case kBool:
		return "boolean"
	case kObject, kMap:
		return "object"
	case kArray:
		return "array"
	}
	return "value"
}

// violation is one thing wrong with a document: the key path and a sentence without any value from the document.
type violation struct {
	path string
	why  string
}

const maxViolations = 10

// jsonType names the type of a decoded value. It never renders the value.
func jsonType(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case string:
		return "string"
	case json.Number:
		return "number"
	case bool:
		return "boolean"
	case map[string]any:
		return "object"
	case []any:
		return "array"
	}
	return "value"
}

// safeKey renders a key the MODULE chose (a map key such as a readiness check name) for an error message: the safe alphabet only, bounded.
func safeKey(k string) string {
	if safeWord(k, 40) {
		return k
	}
	b := make([]byte, 0, 40)
	for i := 0; i < len(k) && len(b) < 40; i++ {
		c := k[i]
		if safeWord(string(c), 1) {
			b = append(b, c)
		} else {
			b = append(b, '?')
		}
	}
	if len(b) == 0 {
		return "?"
	}
	return string(b)
}

func join(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

func (s Shape) check(path string, v any, out *[]violation) {
	if len(*out) >= maxViolations || s.kind == kUnchecked {
		return
	}
	at := path
	if at == "" {
		at = "(body)"
	}
	if v == nil {
		if !s.nullable {
			*out = append(*out, violation{at, "is null, want " + s.kind.String()})
		}
		return
	}
	wrong := func() { *out = append(*out, violation{at, "is a " + jsonType(v) + ", want " + s.kind.String()}) }
	switch s.kind {
	case kAny:
	case kString:
		if _, ok := v.(string); !ok {
			wrong()
		}
	case kNumber:
		if _, ok := v.(json.Number); !ok {
			wrong()
		}
	case kInteger:
		n, ok := v.(json.Number)
		if !ok {
			wrong()
			return
		}
		if _, err := strconv.ParseInt(n.String(), 10, 64); err != nil {
			*out = append(*out, violation{at, "is a number that is not an integer, want integer"})
		}
	case kBool:
		if _, ok := v.(bool); !ok {
			wrong()
		}
	case kObject:
		m, ok := v.(map[string]any)
		if !ok {
			wrong()
			return
		}
		for _, f := range s.fields {
			val, present := m[f.Name]
			if !present {
				if f.Required {
					*out = append(*out, violation{join(path, f.Name), "is missing"})
				}
				continue
			}
			f.Shape.check(join(path, f.Name), val, out)
		}
	case kMap:
		m, ok := v.(map[string]any)
		if !ok {
			wrong()
			return
		}
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			s.elem.check(join(path, safeKey(k)), m[k], out)
		}
	case kArray:
		a, ok := v.([]any)
		if !ok {
			wrong()
			return
		}
		for i, e := range a {
			s.elem.check(path+"["+strconv.Itoa(i)+"]", e, out)
		}
	}
}

// Decode checks resp's body against shape and, only if it conforms, unmarshals it into v (a non-nil pointer, or nil to check without copying). It returns a Contract error naming
// the route and the key for: an empty body, a body that is not one complete JSON document (truncated, trailing data), a document of the wrong top-level type, a required key
// that is missing, a key of the wrong type, a null where none is allowed, and a value that does not fit v.
//
// ON ANY ERROR v IS NOT TOUCHED: the document is unmarshalled into a fresh value and copied over v only after every check passed, so a caller that ignores the error still holds
// what it started with, never half an answer. Keys the shape does not list are ignored.
func Decode(resp *Response, v any, shape Shape) error {
	if resp == nil {
		return &Error{Kind: Invalid, Message: "Decode was given no response"}
	}
	contract := func(key, msg string) error {
		return &Error{Kind: Contract, Module: resp.module, Route: resp.route, Status: resp.Status, RequestID: resp.RequestID, Key: key, Message: msg, Sent: true, Attempts: resp.Attempts}
	}
	if len(bytes.TrimSpace(resp.Body)) == 0 {
		return contract("", "the response body is empty, want a JSON document")
	}
	dec := json.NewDecoder(bytes.NewReader(resp.Body))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return contract("", "the response body ends before the JSON document does (truncated)")
		}
		return contract("", "the response body is not JSON")
	}
	if _, err := dec.Token(); err != io.EOF {
		return contract("", "the response body has data after the JSON document")
	}
	var vs []violation
	shape.check("", doc, &vs)
	if len(vs) > 0 {
		parts := make([]string, 0, 5)
		for i, x := range vs {
			if i == 5 {
				parts = append(parts, fmt.Sprintf("and %d more", len(vs)-5))
				break
			}
			parts = append(parts, strconv.Quote(x.path)+" "+x.why)
		}
		return contract(vs[0].path, "the response breaks the shape the adapter relies on: "+strings.Join(parts, "; "))
	}
	if v == nil {
		return nil
	}
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Pointer || rv.IsNil() {
		return &Error{Kind: Invalid, Module: resp.module, Route: resp.route, Message: "Decode needs a non-nil pointer or nil"}
	}
	fresh := reflect.New(rv.Elem().Type())
	if err := json.Unmarshal(resp.Body, fresh.Interface()); err != nil {
		var ute *json.UnmarshalTypeError
		if errors.As(err, &ute) {
			return contract(ute.Field, "a value does not fit the type the adapter decodes into ("+ute.Type.String()+")")
		}
		return contract("", "the response does not fit the type the adapter decodes into")
	}
	rv.Elem().Set(fresh.Elem())
	return nil
}
