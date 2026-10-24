package backup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// The S3 API, as Cloudflare R2 speaks it, signed with SigV4:
// PUT (one request under MultipartThreshold, else a multipart upload),
// GET, LIST (v2) and DELETE. Path-style URLs: <endpoint>/<bucket>/<key>.

// Timeouts for one request: the small ones, and one part or object.
const (
	RequestTimeout     = 60 * time.Second
	TransferTimeout    = 30 * time.Minute
	MultipartThreshold = 100 << 20 // a single PUT below this
	PartSize           = 64 << 20
	maxErrBody         = 4 << 10
)

// S3 is one bucket on an S3-compatible endpoint.
type S3 struct {
	Endpoint string // https://<account>.r2.cloudflarestorage.com
	Bucket   string
	Region   string // "auto" for R2
	Creds    Credentials
	HTTP     *http.Client
	Now      func() time.Time
	// Threshold and Part are the multipart sizes; zero is
	// MultipartThreshold and PartSize.
	Threshold, Part int64
}

func (c *S3) threshold() int64 {
	if c.Threshold > 0 {
		return c.Threshold
	}
	return MultipartThreshold
}

func (c *S3) partSize() int64 {
	if c.Part > 0 {
		return c.Part
	}
	return PartSize
}

// Object is one listed object.
type Object struct {
	Key  string
	Size int64
}

// S3Error is a refusal from the endpoint.
type S3Error struct {
	Op, Key string
	Status  int
	Code    string
	Message string
}

func (e *S3Error) Error() string {
	msg := e.Code
	if e.Message != "" {
		msg += ": " + e.Message
	}
	return fmt.Sprintf("s3 %s %s: HTTP %d %s", e.Op, e.Key, e.Status, strings.TrimSpace(msg))
}

func (c *S3) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *S3) client() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{}
}

func (c *S3) region() string {
	if c.Region == "" {
		return "auto"
	}
	return c.Region
}

// url is the object's (or with key "", the bucket's) URL.
func (c *S3) url(key string, q url.Values) (*url.URL, error) {
	u, err := url.Parse(strings.TrimRight(c.Endpoint, "/"))
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("s3: endpoint %q is not a URL", c.Endpoint)
	}
	p := "/" + c.Bucket
	if key != "" {
		p += "/" + key
	}
	u.Path = p
	u.RawPath = ""
	u.RawQuery = q.Encode()
	return u, nil
}

// do sends one signed request; body is nil or a reader of payloadHash.
func (c *S3) do(ctx context.Context, timeout time.Duration, method, key string, q url.Values, hdr http.Header,
	body io.Reader, size int64, payloadHash string) (*http.Response, context.CancelFunc, error) {
	u, err := c.url(key, q)
	if err != nil {
		return nil, nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		cancel()
		return nil, nil, fmt.Errorf("s3: %w", err)
	}
	if body != nil {
		req.ContentLength = size
	}
	for k, vs := range hdr {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	if payloadHash == "" {
		payloadHash = EmptySHA256
	}
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	sign(req, c.Creds, c.region(), "s3", payloadHash, c.now())
	resp, err := c.client().Do(req)
	if err != nil {
		cancel()
		return nil, nil, fmt.Errorf("s3 %s %s: %w", method, key, err)
	}
	return resp, cancel, nil
}

// refused reads an error answer into an S3Error.
func refused(op, key string, resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrBody))
	var x struct {
		Code    string `xml:"Code"`
		Message string `xml:"Message"`
	}
	_ = xml.Unmarshal(b, &x)
	return &S3Error{Op: op, Key: key, Status: resp.StatusCode, Code: x.Code, Message: x.Message}
}

// simple runs a request whose answer body is small, and returns it.
func (c *S3) simple(ctx context.Context, op, method, key string, q url.Values, hdr http.Header, body []byte, want ...int) ([]byte, http.Header, error) {
	var r io.Reader
	hash := EmptySHA256
	if body != nil {
		r = bytes.NewReader(body)
		hash = hexSHA256(body)
	}
	resp, cancel, err := c.do(ctx, RequestTimeout, method, key, q, hdr, r, int64(len(body)), hash)
	if err != nil {
		return nil, nil, err
	}
	defer cancel()
	defer resp.Body.Close()
	ok := resp.StatusCode/100 == 2
	if len(want) > 0 {
		ok = false
		for _, w := range want {
			ok = ok || resp.StatusCode == w
		}
	}
	if !ok {
		return nil, nil, refused(op, key, resp)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, nil, fmt.Errorf("s3 %s %s: %w", op, key, err)
	}
	return b, resp.Header, nil
}

// PutFile uploads a local file as key; sum is its hex SHA-256.
func (c *S3) PutFile(ctx context.Context, key, path, sum string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("s3 put %s: %w", key, err)
	}
	if fi.Size() >= c.threshold() {
		return c.putMultipart(ctx, key, path, fi.Size())
	}
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("s3 put %s: %w", key, err)
	}
	defer f.Close()
	resp, cancel, err := c.do(ctx, TransferTimeout, http.MethodPut, key, nil, nil, f, fi.Size(), sum)
	if err != nil {
		return err
	}
	defer cancel()
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return refused("put", key, resp)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// PutBytes uploads a small object.
func (c *S3) PutBytes(ctx context.Context, key string, body []byte) error {
	_, _, err := c.simple(ctx, "put", http.MethodPut, key, nil, nil, body)
	return err
}

func (c *S3) putMultipart(ctx context.Context, key, path string, size int64) (err error) {
	b, _, err := c.simple(ctx, "create-multipart", http.MethodPost, key, url.Values{"uploads": {""}}, nil, nil)
	if err != nil {
		return err
	}
	var start struct {
		UploadID string `xml:"UploadId"`
	}
	if err := xml.Unmarshal(b, &start); err != nil || start.UploadID == "" {
		return fmt.Errorf("s3 create-multipart %s: no UploadId in the answer", key)
	}
	defer func() {
		if err != nil {
			// a failed upload leaves no parts behind to pay for
			_, _, _ = c.simple(context.WithoutCancel(ctx), "abort-multipart", http.MethodDelete, key,
				url.Values{"uploadId": {start.UploadID}}, nil, nil)
		}
	}()
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("s3 put %s: %w", key, err)
	}
	defer f.Close()
	type part struct {
		XMLName    xml.Name `xml:"Part"`
		PartNumber int      `xml:"PartNumber"`
		ETag       string   `xml:"ETag"`
	}
	var parts []part
	partLen := c.partSize()
	buf := make([]byte, partLen)
	for n := 1; int64(n-1)*partLen < size; n++ {
		m, rerr := io.ReadFull(f, buf)
		if rerr != nil && rerr != io.ErrUnexpectedEOF {
			return fmt.Errorf("s3 put %s: %w", key, rerr)
		}
		chunk := buf[:m]
		q := url.Values{"partNumber": {strconv.Itoa(n)}, "uploadId": {start.UploadID}}
		resp, cancel, derr := c.do(ctx, TransferTimeout, http.MethodPut, key, q, nil, bytes.NewReader(chunk), int64(m), hexSHA256(chunk))
		if derr != nil {
			return derr
		}
		if resp.StatusCode/100 != 2 {
			e := refused("upload-part", key, resp)
			resp.Body.Close()
			cancel()
			return e
		}
		etag := resp.Header.Get("ETag")
		resp.Body.Close()
		cancel()
		parts = append(parts, part{PartNumber: n, ETag: etag})
	}
	body, err := xml.Marshal(struct {
		XMLName xml.Name `xml:"CompleteMultipartUpload"`
		Parts   []part
	}{Parts: parts})
	if err != nil {
		return fmt.Errorf("s3 complete-multipart %s: %w", key, err)
	}
	b, _, err = c.simple(ctx, "complete-multipart", http.MethodPost, key, url.Values{"uploadId": {start.UploadID}}, nil, body)
	if err != nil {
		return err
	}
	// S3 may answer 200 with an error document
	if bytes.Contains(b, []byte("<Error>")) {
		return fmt.Errorf("s3 complete-multipart %s: %s", key, strings.TrimSpace(string(b)))
	}
	return nil
}

// Get streams an object into w and returns how many bytes it wrote.
func (c *S3) Get(ctx context.Context, key string, w io.Writer) (int64, error) {
	resp, cancel, err := c.do(ctx, TransferTimeout, http.MethodGet, key, nil, nil, nil, 0, "")
	if err != nil {
		return 0, err
	}
	defer cancel()
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, refused("get", key, resp)
	}
	n, err := io.Copy(w, resp.Body)
	if err != nil {
		return n, fmt.Errorf("s3 get %s: %w", key, err)
	}
	return n, nil
}

// GetSmall is a small object's bytes.
func (c *S3) GetSmall(ctx context.Context, key string) ([]byte, error) {
	b, _, err := c.simple(ctx, "get", http.MethodGet, key, nil, nil, nil, http.StatusOK)
	return b, err
}

// List is every object under prefix (ListObjectsV2, page by page).
func (c *S3) List(ctx context.Context, prefix string) ([]Object, error) {
	var out []Object
	token := ""
	for {
		q := url.Values{"list-type": {"2"}, "prefix": {prefix}}
		if token != "" {
			q.Set("continuation-token", token)
		}
		b, _, err := c.simple(ctx, "list", http.MethodGet, "", q, nil, nil, http.StatusOK)
		if err != nil {
			return nil, err
		}
		var page struct {
			Contents []struct {
				Key  string `xml:"Key"`
				Size int64  `xml:"Size"`
			} `xml:"Contents"`
			IsTruncated bool   `xml:"IsTruncated"`
			Next        string `xml:"NextContinuationToken"`
		}
		if err := xml.Unmarshal(b, &page); err != nil {
			return nil, fmt.Errorf("s3 list %s: %w", prefix, err)
		}
		for _, o := range page.Contents {
			out = append(out, Object{Key: o.Key, Size: o.Size})
		}
		if !page.IsTruncated || page.Next == "" {
			return out, nil
		}
		token = page.Next
	}
}

// Delete removes an object (a missing one is no error, as in S3).
func (c *S3) Delete(ctx context.Context, key string) error {
	_, _, err := c.simple(ctx, "delete", http.MethodDelete, key, nil, nil, nil)
	return err
}

// fileSHA256 is a file's hex SHA-256 and size.
func fileSHA256(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// errNotConfigured is a [backup] table without an endpoint or bucket.
var errNotConfigured = errors.New("backup: [backup] needs endpoint and bucket")
