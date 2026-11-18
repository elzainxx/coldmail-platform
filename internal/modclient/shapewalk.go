package modclient

// ShapeKind names what a Shape accepts at one position. It exists so that a tool that reasons ABOUT a Shape (the contract drift checker, which has to know where a map is and
// which keys are optional) can read it without reaching into its fields: the walk below is read-only and returns copies.
type ShapeKind uint8

const (
	// ShapeUnchecked is the zero Shape: it checks nothing.
	ShapeUnchecked ShapeKind = iota
	// ShapeAny is any JSON value (null only with OrNull).
	ShapeAny
	ShapeString
	// ShapeNumber is any JSON number.
	ShapeNumber
	// ShapeInteger is a JSON number written as an integer that fits in 64 bits.
	ShapeInteger
	ShapeBool
	// ShapeObject is an object with the listed Fields.
	ShapeObject
	// ShapeMap is an object with any keys whose every value has the shape Elem.
	ShapeMap
	// ShapeArray is an array whose every element has the shape Elem.
	ShapeArray
)

func (k ShapeKind) String() string {
	switch k {
	case ShapeUnchecked:
		return "unchecked"
	case ShapeAny:
		return "any"
	case ShapeString:
		return "string"
	case ShapeNumber:
		return "number"
	case ShapeInteger:
		return "integer"
	case ShapeBool:
		return "boolean"
	case ShapeObject:
		return "object"
	case ShapeMap:
		return "map"
	case ShapeArray:
		return "array"
	}
	return "unknown"
}

// Kind is what the Shape accepts at its position.
func (s Shape) Kind() ShapeKind {
	switch s.kind {
	case kAny:
		return ShapeAny
	case kString:
		return ShapeString
	case kNumber:
		return ShapeNumber
	case kInteger:
		return ShapeInteger
	case kBool:
		return ShapeBool
	case kObject:
		return ShapeObject
	case kMap:
		return ShapeMap
	case kArray:
		return ShapeArray
	}
	return ShapeUnchecked
}

// Nullable reports whether the Shape also accepts JSON null (OrNull).
func (s Shape) Nullable() bool { return s.nullable }

// IsOpen reports whether the Shape is an object marked Open: a bag of module-defined keys, of which the Shape lists only the ones the platform reads. It is false for every
// other kind, whatever was called.
func (s Shape) IsOpen() bool { return s.open && s.kind == kObject }

// Fields returns the listed keys of an object Shape in the order they were given (a copy: the Shape is immutable); nil for any other kind.
func (s Shape) Fields() []Field {
	if s.kind != kObject || len(s.fields) == 0 {
		return nil
	}
	return append([]Field(nil), s.fields...)
}

// Elem returns the shape of every element of an array Shape or every value of a map Shape; false for any other kind.
func (s Shape) Elem() (Shape, bool) {
	if s.elem == nil {
		return Shape{}, false
	}
	return *s.elem, true
}
