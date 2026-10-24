package mailbox

import (
	"fmt"
	"path/filepath"

	"github.com/stevegeek/agent-factory/internal/fsx"
	"github.com/stevegeek/agent-factory/internal/jobproto"
	"github.com/stevegeek/agent-factory/internal/vitals"
)

// PostVitals stores what the host sent as its vitals, once vitals.Parse
// takes it, in mailbox/<host>/vitals.json: stamped with this side's clock
// (the host's own moves to Reported), so its age never rests on the
// host's clock. It records that the host called in.
func (b Box) PostVitals(host string, data []byte) error {
	d, err := b.hostDir(host)
	if err != nil {
		return err
	}
	if len(data) > jobproto.MaxVitals {
		return refuse("vitals over %d bytes", jobproto.MaxVitals)
	}
	v, err := vitals.Parse(data)
	if err != nil {
		return refuse("%v", err)
	}
	if err := b.seen(host); err != nil {
		return err
	}
	out, err := v.Received(b.now()).Encode()
	if err != nil {
		return err
	}
	return fsx.WriteAtomic(filepath.Join(d, jobproto.MailVitalsFile), out, 0o600)
}

// Vitals is the host's last post, never read through a link. None yet
// matches fs.ErrNotExist.
func (b Box) Vitals(host string) (vitals.Vitals, error) {
	d, err := b.hostDir(host)
	if err != nil {
		return vitals.Vitals{}, err
	}
	data, err := fsx.ReadNoFollow(filepath.Join(d, jobproto.MailVitalsFile), vitals.MaxBytes)
	if err != nil {
		return vitals.Vitals{}, err
	}
	v, err := vitals.Parse(data)
	if err != nil {
		return vitals.Vitals{}, fmt.Errorf("%s %s: %w", host, jobproto.MailVitalsFile, err)
	}
	return v, nil
}
