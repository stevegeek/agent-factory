package jobtail

import (
	"errors"
	"io/fs"
	"path/filepath"

	"github.com/stevegeek/agent-factory/internal/fsx"
	"github.com/stevegeek/agent-factory/internal/jobproto"
)

// MailTail is the tail of a pull host's job (`transport = "pull"`): the log
// tail its last beat left in the mailbox, <dir>/<host>/<job>/beat.log,
// read without following a link and capped at jobproto.TailBytes. A job
// with no beat yet, or gone, has none: "".
func MailTail(dir, host, job string) (string, error) {
	if !jobproto.JobNameOK(host) || !jobproto.JobNameOK(job) {
		return "", errors.New("bad host or job name")
	}
	b, err := fsx.ReadNoFollow(filepath.Join(dir, host, job, jobproto.MailLog), jobproto.TailBytes)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return jobproto.DecodeText(b), nil
}
