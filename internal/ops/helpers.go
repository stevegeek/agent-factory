package ops

import (
	"errors"
	"io/fs"
	"time"

	"github.com/stevegeek/agent-factory/internal/clock"
	"github.com/stevegeek/agent-factory/internal/jsonx"
)

func stampOf(t time.Time) string { return clock.Stamp(t) }

func isNotExist(err error) bool { return errors.Is(err, fs.ErrNotExist) }

// lookup is key in an object value: its value and whether the key is there.
func lookup(v jsonx.Value, key string) (jsonx.Value, bool) {
	o, ok := v.Object()
	if !ok {
		return jsonx.Null(), false
	}
	return o.Lookup(key)
}
