package provider

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"testing"
	"time"
)

// goldenSig is one signature produced by botocore (the reference AWS SDK for
// Python) for a Bedrock Converse request, with the exact timestamp it used.
// Regenerate with the script described in testdata/README.md.
type goldenSig struct {
	Name          string `json:"name"`
	Model         string `json:"model"`
	Token         string `json:"token"`
	Body          string `json:"body"`
	Authorization string `json:"authorization"`
	AmzDate       string `json:"amzdate"`
}

// The hand-written signer must agree with botocore byte for byte. This is the
// test that matters: a signer can be internally consistent and still be wrong,
// and the failure mode in production is an opaque 403 on exactly the model ids
// that contain ":" or "/" — the double-encoding trap.
func TestSigV4MatchesBotocore(t *testing.T) {
	raw, err := os.ReadFile("testdata/sigv4_botocore.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []goldenSig
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) < 4 {
		t.Fatalf("expected the full golden set, got %d cases", len(cases))
	}
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			when, err := time.Parse(amzDateLayout, c.AmzDate)
			if err != nil {
				t.Fatal(err)
			}
			endpoint := "https://bedrock-runtime.us-east-1.amazonaws.com/model/" + uriEncode(c.Model) + "/converse"
			req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader([]byte(c.Body)))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/json")
			signRequest(req, []byte(c.Body), sigV4Credentials{
				AccessKeyID:     "AKIDEXAMPLE",
				SecretAccessKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY",
				SessionToken:    c.Token,
			}, "us-east-1", "bedrock", when)

			if got := req.Header.Get("Authorization"); got != c.Authorization {
				t.Errorf("Authorization differs from botocore\n got: %s\nwant: %s", got, c.Authorization)
			}
			if got := req.Header.Get("X-Amz-Date"); got != c.AmzDate {
				t.Errorf("X-Amz-Date = %q, want %q", got, c.AmzDate)
			}
			if (req.Header.Get("X-Amz-Security-Token") != "") != (c.Token != "") {
				t.Errorf("X-Amz-Security-Token presence wrong for token %q", c.Token)
			}
		})
	}
}

// AWS's published worked example (Signature Version 4 documentation, IAM
// ListUsers), covering the canonical-query path that Bedrock itself never uses.
func TestSigV4MatchesAWSPublishedExample(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "https://iam.amazonaws.com/?Action=ListUsers&Version=2010-05-08", nil)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	when := time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC)
	signRequest(req, nil, sigV4Credentials{
		AccessKeyID:     "AKIDEXAMPLE",
		SecretAccessKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY",
	}, "us-east-1", "iam", when)

	const want = "5d672d79c15b13162d9279b0855cfba6789a8edb4c82c400e06b5924a6f2b5d7"
	if got := req.Header.Get("Authorization"); !bytes.Contains([]byte(got), []byte("Signature="+want)) {
		t.Errorf("signature != AWS's documented %s\n got: %s", want, got)
	}
}

// uriEncode must escape what url.PathEscape leaves alone.
func TestURIEncodeEscapesColonAndSlash(t *testing.T) {
	cases := map[string]string{
		"v2:0":                "v2%3A0",
		"a/b":                 "a%2Fb",
		"safe-._~AZaz09":      "safe-._~AZaz09",
		"arn:aws:bedrock:x/y": "arn%3Aaws%3Abedrock%3Ax%2Fy",
		"a+b=c@d&e":           "a%2Bb%3Dc%40d%26e",
	}
	for in, want := range cases {
		if got := uriEncode(in); got != want {
			t.Errorf("uriEncode(%q) = %q, want %q", in, got, want)
		}
	}
}
