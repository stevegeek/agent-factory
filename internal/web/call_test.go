package web

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type logLines struct{ got []string }

func (l *logLines) Printf(format string, args ...any) {
	l.got = append(l.got, fmt.Sprintf(format, args...))
}

// a panic in a service call reaches the caller as an error with no
// detail, and the log has the panic with its stack.
func TestACallPanicIsLoggedWithItsStack(t *testing.T) {
	log := &logLines{}
	s := &Server{log: log}
	r := httptest.NewRequest("POST", "/t/T0001/answer", nil)
	err := s.call(r, time.Second, func(context.Context) error { panic("boom") })
	if err == nil || strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v", err)
	}
	if len(log.got) != 1 || !strings.HasPrefix(log.got[0], "panic in POST /t/T0001/answer: boom\n") ||
		!strings.Contains(log.got[0], "goroutine ") || !strings.Contains(log.got[0], "call_test.go") {
		t.Fatalf("log %q", log.got)
	}
}
