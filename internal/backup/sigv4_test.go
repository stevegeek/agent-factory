package backup

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// SigV4 against AWS's published examples: get-vanilla and
// get-vanilla-query-order-key-case from the Signature Version 4 test suite,
// and the GET Object and GET Bucket (list) examples of the S3 API reference
// ("Authenticating Requests: Using the Authorization Header").
func TestSigV4AgainstAWSVectors(t *testing.T) {
	suite := Credentials{AccessKeyID: "AKIDEXAMPLE", SecretAccessKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"}
	s3 := Credentials{AccessKeyID: "AKIAIOSFODNN7EXAMPLE", SecretAccessKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"}
	suiteAt := time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC)
	s3At := time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name, url, service, region string
		hdr                        map[string]string
		creds                      Credentials
		at                         time.Time
		want                       string
	}{
		{"get-vanilla", "https://example.amazonaws.com/", "service", "us-east-1", nil, suite, suiteAt,
			"AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, SignedHeaders=host;x-amz-date, " +
				"Signature=5fa00fa31553b73ebf1942676e86291e8372ff2a2260956d9b8aae1d763fbf31"},
		{"get-vanilla-query-order-key-case", "https://example.amazonaws.com/?Param2=value2&Param1=value1", "service", "us-east-1", nil, suite, suiteAt,
			"AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, SignedHeaders=host;x-amz-date, " +
				"Signature=b97d918cfa904a5beff61c982a1b6f458b799221646efd99d3219ec94cdf2500"},
		{"s3 GET object", "https://examplebucket.s3.amazonaws.com/test.txt", "s3", "us-east-1",
			map[string]string{"Range": "bytes=0-9", "X-Amz-Content-Sha256": EmptySHA256}, s3, s3At,
			"AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request, " +
				"SignedHeaders=host;range;x-amz-content-sha256;x-amz-date, " +
				"Signature=f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41"},
		{"s3 GET bucket (list)", "https://examplebucket.s3.amazonaws.com/?max-keys=2&prefix=J", "s3", "us-east-1",
			map[string]string{"X-Amz-Content-Sha256": EmptySHA256}, s3, s3At,
			"AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request, " +
				"SignedHeaders=host;x-amz-content-sha256;x-amz-date, " +
				"Signature=34b48302e7b5fa45bde8084f4b7868a86f0a534bc59db6670ed5711ef69dc6f7"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, c.url, nil)
			if err != nil {
				t.Fatal(err)
			}
			for k, v := range c.hdr {
				req.Header.Set(k, v)
			}
			sign(req, c.creds, c.region, c.service, EmptySHA256, c.at)
			if got := req.Header.Get("Authorization"); got != c.want {
				t.Errorf("Authorization\n got %s\nwant %s", got, c.want)
			}
		})
	}
}

func TestURIEncode(t *testing.T) {
	for in, want := range map[string]string{
		"factory-box-20261018T043000Z.tar.gz.fbk": "factory-box-20261018T043000Z.tar.gz.fbk",
		"a b+c/d~e": "a%20b%2Bc%2Fd~e",
		"é":         "%C3%A9",
	} {
		if got := uriEncode(in); got != want {
			t.Errorf("uriEncode(%q) = %q, want %q", in, got, want)
		}
	}
	req, _ := http.NewRequest(http.MethodPut, "https://h.example/bucket/a%20b/c+d", nil)
	if got := canonicalPath(req.URL); !strings.HasSuffix(got, "/a%20b/c%2Bd") {
		t.Errorf("canonical path %q", got)
	}
}
