package cli

import (
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/github"
	"github.com/stevegeek/agent-factory/internal/gitx"
	"github.com/stevegeek/agent-factory/internal/jobproto"
	"github.com/stevegeek/agent-factory/internal/podman"
	"github.com/stevegeek/agent-factory/internal/scionread"
)

// The per-call timeouts keep Python's values. Each lives as a
// constant in the adapter that runs the call; this one table pins them all
// to Python's values, so a change to any of them is a change to this test.
func TestPerCallTimeoutsArePythons(t *testing.T) {
	for _, tc := range []struct {
		name string
		got  time.Duration
		want time.Duration
	}{
		{"scion (scionread.Timeout)", scionread.Timeout, 600 * time.Second},
		{"gate podman (podman.GateTimeout)", podman.GateTimeout, 1800 * time.Second},
		{"ssh start/status/remove/list (jobproto.Timeout)", jobproto.Timeout(jobproto.VerbStart), 120 * time.Second},
		{"ssh status", jobproto.Timeout(jobproto.VerbStatus), 120 * time.Second},
		{"ssh collect", jobproto.Timeout(jobproto.VerbCollect), 300 * time.Second},
		{"ssh tail", jobproto.Timeout(jobproto.VerbTail), 20 * time.Second},
		{"refresh_base fetch (gitx.FetchTimeout)", gitx.FetchTimeout, 120 * time.Second},
		{"push (gitx.PushTimeout)", gitx.PushTimeout, 300 * time.Second},
		{"GitHub HTTP (github.Timeout)", github.Timeout, 30 * time.Second},
		{"image build (podman.BuildTimeout)", podman.BuildTimeout, 6000 * time.Second},
		{"smoke (podman.SmokeTimeout)", podman.SmokeTimeout, 2400 * time.Second},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %v, Python's is %v", tc.name, tc.got, tc.want)
		}
	}
}
