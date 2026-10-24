package backup

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

var fakeCreds = Credentials{AccessKeyID: "AKIDTEST", SecretAccessKey: "test-secret"}

// fakeS3 is an S3 endpoint in memory for one bucket. Every request must be
// signed with fakeCreds: the server signs the same request again (over the
// headers the client signed) and compares, and checks the payload hash
// against the body.
// It is reached in-process, as the client's RoundTripper: no socket.
type fakeS3 struct {
	t       *testing.T
	bucket  string
	mu      sync.Mutex
	objects map[string][]byte
	uploads map[string]map[int][]byte
	log     []string
	page    int             // keys per LIST page
	fail    map[string]int  // "METHOD key" -> status to answer instead
	corrupt map[string]bool // keys whose GET answers different bytes
}

func newFakeS3(t *testing.T) *fakeS3 {
	return &fakeS3{t: t, bucket: "bkt", objects: map[string][]byte{}, uploads: map[string]map[int][]byte{},
		page: 2, fail: map[string]int{}, corrupt: map[string]bool{}}
}

func (f *fakeS3) client() *S3 {
	return &S3{Endpoint: "https://acct.r2.example", Bucket: f.bucket, Region: "auto", Creds: fakeCreds,
		HTTP: &http.Client{Transport: f}}
}

// RoundTrip serves the request as the server would see it.
func (f *fakeS3) RoundTrip(r *http.Request) (*http.Response, error) {
	sr := r.Clone(r.Context())
	sr.RequestURI = r.URL.RequestURI()
	if sr.Host == "" {
		sr.Host = r.URL.Host
	}
	if r.Body != nil {
		b, err := io.ReadAll(r.Body)
		r.Body.Close()
		if err != nil {
			return nil, err
		}
		sr.Body = io.NopCloser(bytes.NewReader(b))
	} else {
		sr.Body = http.NoBody
	}
	rec := httptest.NewRecorder()
	f.serve(rec, sr)
	resp := rec.Result()
	resp.Request = r
	return resp, nil
}

func (f *fakeS3) keys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for k := range f.objects {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (f *fakeS3) checkSignature(r *http.Request, body []byte) error {
	auth := r.Header.Get("Authorization")
	_, signedPart, ok := strings.Cut(auth, "SignedHeaders=")
	if !ok {
		return fmt.Errorf("no SigV4 Authorization: %q", auth)
	}
	signed, _, _ := strings.Cut(signedPart, ",")
	if got := hexSHA256(body); r.Header.Get("X-Amz-Content-Sha256") != got {
		return fmt.Errorf("payload hash %s, body %s", r.Header.Get("X-Amz-Content-Sha256"), got)
	}
	at, err := time.Parse("20060102T150405Z", r.Header.Get("X-Amz-Date"))
	if err != nil {
		return err
	}
	again, _ := http.NewRequest(r.Method, "http://"+r.Host+r.URL.RequestURI(), nil)
	for _, h := range strings.Split(signed, ";") {
		if h != "host" && h != "x-amz-date" {
			again.Header.Set(h, r.Header.Get(h))
		}
	}
	sign(again, fakeCreds, "auto", "s3", r.Header.Get("X-Amz-Content-Sha256"), at)
	if again.Header.Get("Authorization") != auth {
		return fmt.Errorf("signature mismatch:\n got %s\nwant %s", auth, again.Header.Get("Authorization"))
	}
	return nil
}

func (f *fakeS3) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	if err := f.checkSignature(r, body); err != nil {
		f.t.Errorf("%s %s: %v", r.Method, r.URL, err)
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, "<Error><Code>SignatureDoesNotMatch</Code></Error>")
		return
	}
	p := strings.TrimPrefix(r.URL.Path, "/")
	bucket, key, _ := strings.Cut(p, "/")
	if bucket != f.bucket {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, "<Error><Code>NoSuchBucket</Code></Error>")
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log = append(f.log, r.Method+" "+key)
	if st, ok := f.fail[r.Method+" "+key]; ok {
		w.WriteHeader(st)
		fmt.Fprint(w, "<Error><Code>InternalError</Code><Message>scripted</Message></Error>")
		return
	}
	q := r.URL.Query()
	switch {
	case r.Method == http.MethodGet && key == "":
		f.list(w, q)
	case r.Method == http.MethodPost && q.Has("uploads"):
		id := "up" + strconv.Itoa(len(f.uploads)+1)
		f.uploads[id] = map[int][]byte{}
		fmt.Fprintf(w, "<InitiateMultipartUploadResult><UploadId>%s</UploadId></InitiateMultipartUploadResult>", id)
	case r.Method == http.MethodPut && q.Has("partNumber"):
		n, _ := strconv.Atoi(q.Get("partNumber"))
		f.uploads[q.Get("uploadId")][n] = body
		w.Header().Set("ETag", fmt.Sprintf("\"etag-%d\"", n))
	case r.Method == http.MethodPost && q.Has("uploadId"):
		var done struct {
			Parts []struct {
				PartNumber int    `xml:"PartNumber"`
				ETag       string `xml:"ETag"`
			} `xml:"Part"`
		}
		if err := xml.Unmarshal(body, &done); err != nil {
			f.t.Errorf("complete: %v", err)
		}
		var all []byte
		for i, p := range done.Parts {
			if p.PartNumber != i+1 || p.ETag != fmt.Sprintf("\"etag-%d\"", i+1) {
				f.t.Errorf("complete lists part %+v at %d", p, i)
			}
			all = append(all, f.uploads[q.Get("uploadId")][p.PartNumber]...)
		}
		f.objects[key] = all
		fmt.Fprint(w, "<CompleteMultipartUploadResult/>")
	case r.Method == http.MethodDelete && q.Has("uploadId"):
		delete(f.uploads, q.Get("uploadId"))
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPut:
		f.objects[key] = body
	case r.Method == http.MethodGet:
		b, ok := f.objects[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, "<Error><Code>NoSuchKey</Code></Error>")
			return
		}
		if f.corrupt[key] {
			b = append(bytes.Clone(b[:len(b)-1]), b[len(b)-1]^1)
		}
		w.Write(b)
	case r.Method == http.MethodDelete:
		delete(f.objects, key)
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (f *fakeS3) list(w http.ResponseWriter, q map[string][]string) {
	prefix := ""
	if v := q["prefix"]; len(v) > 0 {
		prefix = v[0]
	}
	var keys []string
	for k := range f.objects {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	start := 0
	if v := q["continuation-token"]; len(v) > 0 {
		start, _ = strconv.Atoi(v[0])
	}
	end := min(start+f.page, len(keys))
	var b strings.Builder
	b.WriteString("<ListBucketResult>")
	for _, k := range keys[start:end] {
		fmt.Fprintf(&b, "<Contents><Key>%s</Key><Size>%d</Size></Contents>", k, len(f.objects[k]))
	}
	if end < len(keys) {
		fmt.Fprintf(&b, "<IsTruncated>true</IsTruncated><NextContinuationToken>%d</NextContinuationToken>", end)
	} else {
		b.WriteString("<IsTruncated>false</IsTruncated>")
	}
	b.WriteString("</ListBucketResult>")
	fmt.Fprint(w, b.String())
}

// the S3 client against the fake server: PUT, LIST (several pages),
// GET, DELETE, and a refusal as an S3Error.
func TestS3PutListGetDelete(t *testing.T) {
	f := newFakeS3(t)
	c := f.client()
	ctx := t.Context()
	dir := t.TempDir()
	for i, k := range []string{"p/a", "p/b c", "p/c+d", "q/other"} {
		path := filepath.Join(dir, strconv.Itoa(i))
		os.WriteFile(path, []byte("object "+k), 0o600)
		sum, _, _ := fileSHA256(path)
		if err := c.PutFile(ctx, k, path, sum); err != nil {
			t.Fatal(err)
		}
	}
	objs, err := c.List(ctx, "p/")
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 3 || objs[1].Key != "p/b c" || objs[1].Size != int64(len("object p/b c")) {
		t.Errorf("list %+v", objs)
	}
	var buf bytes.Buffer
	if n, err := c.Get(ctx, "p/c+d", &buf); err != nil || buf.String() != "object p/c+d" || n != 12 {
		t.Errorf("get: %q %d %v", buf.String(), n, err)
	}
	if err := c.Delete(ctx, "p/a"); err != nil {
		t.Fatal(err)
	}
	if got := f.keys(); strings.Join(got, ",") != "p/b c,p/c+d,q/other" {
		t.Errorf("after delete: %v", got)
	}
	_, err = c.Get(ctx, "p/missing", io.Discard)
	if e, ok := err.(*S3Error); !ok || e.Status != 404 || e.Code != "NoSuchKey" {
		t.Errorf("missing: %v", err)
	}
}

// Above the threshold the upload goes in parts, and the parts join into
// the object; a failed part aborts the upload.
func TestS3Multipart(t *testing.T) {
	f := newFakeS3(t)
	c := f.client()
	c.Threshold, c.Part = 10, 4
	path := filepath.Join(t.TempDir(), "big")
	data := []byte("0123456789abcdefghij-")
	os.WriteFile(path, data, 0o600)
	sum, _, _ := fileSHA256(path)
	if err := c.PutFile(t.Context(), "big", path, sum); err != nil {
		t.Fatal(err)
	}
	if got := f.objects["big"]; !bytes.Equal(got, data) {
		t.Errorf("joined %q", got)
	}
	parts := 0
	for _, l := range f.log {
		if l == "PUT big" {
			parts++
		}
	}
	if parts != 6 {
		t.Errorf("%d part requests, want 6: %v", parts, f.log)
	}
	f.fail["PUT big2"] = 500
	err := c.PutFile(t.Context(), "big2", path, sum)
	if err == nil || len(f.uploads) != 1 || f.log[len(f.log)-1] != "DELETE big2" {
		t.Errorf("a failed part: %v, uploads %v, log %v", err, f.uploads, f.log)
	}
}
