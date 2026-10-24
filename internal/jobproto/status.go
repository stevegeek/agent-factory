package jobproto

import (
	"fmt"

	"github.com/stevegeek/agent-factory/internal/jsonx"
)

// ParseStatus reads a status answer, json.loads(out)["state"].
// A state that is not a string is "" (neither running nor done: the tick
// treats the job as gone, as Python's comparisons do); an answer that is
// not an object holding "state" is an error.
func ParseStatus(out []byte) (string, error) {
	v, err := jsonx.Parse(out)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrStatus, err)
	}
	o, ok := v.Object()
	if !ok {
		return "", ErrStatus
	}
	st, ok := o.Lookup("state")
	if !ok {
		return "", ErrStatus
	}
	s, _ := st.Str()
	return s, nil
}
