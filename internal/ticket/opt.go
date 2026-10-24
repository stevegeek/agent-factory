package ticket

// Opt tells apart a key that is absent, a key that holds null, and a key
// that holds a value: Python writes "branch": null on some tickets and
// leaves keys out on others, and a save must not turn one into the other.
//
// The zero Opt is absent. On a save, an absent Opt removes its key from the
// stored object (Python's pop), a null one writes null, and a set one writes
// V.
type Opt[T any] struct {
	Set  bool // the key is there
	Null bool // and holds null
	V    T
}

// Some is an Opt holding v.
func Some[T any](v T) Opt[T] { return Opt[T]{Set: true, V: v} }

// Nil is an Opt holding null.
func Nil[T any]() Opt[T] { return Opt[T]{Set: true, Null: true} }

// Get is the value and whether there is one (set and not null).
func (o Opt[T]) Get() (T, bool) {
	if o.Set && !o.Null {
		return o.V, true
	}
	var zero T
	return zero, false
}

// Present reports a value: set and not null.
func (o Opt[T]) Present() bool { return o.Set && !o.Null }

// Or is the value, or def when there is none.
func (o Opt[T]) Or(def T) T {
	if v, ok := o.Get(); ok {
		return v
	}
	return def
}

// optional marks Opt for the codec.
func (Opt[T]) optional() {}

type optional interface{ optional() }
