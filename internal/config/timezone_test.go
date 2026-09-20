package config_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	. "github.com/awsaman-ai/queryforge/internal/config"
)

// tzConfig builds a minimal config JSON with the given timezone fragment.
func tzConfig(tzFragment string) string {
	return `{"entity":"Order","model":{},` + tzFragment + `"fields":[{"name":"createdAt","type":"date"}]}`
}

// TestTimezoneResolves covers the happy path and the default.
func TestTimezoneResolves(t *testing.T) {
	cases := map[string]struct {
		fragment string
		want     string
	}{
		"omitted is UTC":  {``, "UTC"},
		"empty is UTC":    {`"timezone":"",`, "UTC"},
		"explicit UTC":    {`"timezone":"UTC",`, "UTC"},
		"IANA zone":       {`"timezone":"Asia/Kolkata",`, "Asia/Kolkata"},
		"zone with a DST": {`"timezone":"America/New_York",`, "America/New_York"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c, err := ParseConfig([]byte(tzConfig(tc.fragment)))
			if err != nil {
				t.Fatalf("ParseConfig: %v", err)
			}
			if got := LocationFor(c).String(); got != tc.want {
				t.Errorf("LocationFor = %s, want %s", got, tc.want)
			}
		})
	}
}

// TestTimezoneRejectsBadValues is the adversarial half. Each of these must
// fail the LOAD, naming the problem, rather than surface as wrong rows on the
// first question that says "today".
func TestTimezoneRejectsBadValues(t *testing.T) {
	cases := map[string]struct {
		fragment string
		wantErr  string
	}{
		"unknown zone":        {`"timezone":"Mars/Olympus_Mons",`, "not a known IANA zone"},
		"abbreviation":        {`"timezone":"IST",`, "not a known IANA zone"},
		"offset string":       {`"timezone":"+05:30",`, "not a known IANA zone"},
		"host-dependent":      {`"timezone":"Local",`, "depends on the host"},
		"host-dependent case": {`"timezone":"local",`, "depends on the host"},
		"surrounding space":   {`"timezone":" Asia/Kolkata",`, "whitespace"},
		"path traversal":      {`"timezone":"../../etc/passwd",`, "not a known IANA zone"},
		"wrong JSON type":     {`"timezone":330,`, "parse config"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ParseConfig([]byte(tzConfig(tc.fragment)))
			if err == nil {
				t.Fatalf("config with %s loaded; it must be rejected", tc.fragment)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %v, want it to mention %q", err, tc.wantErr)
			}
		})
	}
}

// TestLocationForUnfinalizedConfig covers a Config built in Go and handed
// straight to a generator: the zone is still honoured, and a bad one degrades
// to UTC rather than panicking mid-generation (load is where it is reported).
func TestLocationForUnfinalizedConfig(t *testing.T) {
	if got := LocationFor(&Config{Timezone: "Asia/Kolkata"}).String(); got != "Asia/Kolkata" {
		t.Errorf("unfinalized zone = %s", got)
	}
	if got := LocationFor(&Config{Timezone: "Nope/Nowhere"}); got != time.UTC {
		t.Errorf("unfinalized bad zone = %s, want UTC", got)
	}
	if got := LocationFor(nil); got != time.UTC {
		t.Errorf("nil config = %s, want UTC", got)
	}
}

// TestTimezoneOmittedFromJSONWhenUnset keeps existing configs byte-identical
// when re-serialized: an unset timezone must not appear as "timezone":"".
func TestTimezoneOmittedFromJSONWhenUnset(t *testing.T) {
	c, err := ParseConfig([]byte(tzConfig(``)))
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	out, _ := json.Marshal(c)
	if strings.Contains(string(out), "timezone") {
		t.Errorf("unset timezone leaked into JSON: %s", out)
	}
	c.Timezone = "Asia/Kolkata"
	out, _ = json.Marshal(c)
	if !strings.Contains(string(out), `"timezone":"Asia/Kolkata"`) {
		t.Errorf("set timezone missing from JSON: %s", out)
	}
}
