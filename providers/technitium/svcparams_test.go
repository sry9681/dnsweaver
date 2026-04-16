package technitium

import (
	"encoding/json"
	"testing"
)

// TestSvcParamsUnmarshal_StringForm pins backward compatibility with older
// Technitium versions and the add/delete query-string roundtrip: a JSON
// string value must be stored verbatim as the pipe-separated form.
func TestSvcParamsUnmarshal_StringForm(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{`"alpn|h2"`, "alpn|h2"},
		{`"alpn|h2,h3 port|443"`, "alpn|h2,h3 port|443"},
		{`""`, ""},
	}
	for _, tt := range tests {
		var s svcParams
		if err := json.Unmarshal([]byte(tt.in), &s); err != nil {
			t.Errorf("Unmarshal(%s) err = %v", tt.in, err)
			continue
		}
		if string(s) != tt.want {
			t.Errorf("Unmarshal(%s) = %q, want %q", tt.in, string(s), tt.want)
		}
	}
}

// TestSvcParamsUnmarshal_Null pins that null/missing values don't error and
// produce an empty svcParams. Without this, any HTTPS record with a null
// svcParams in the zone listing would fail the entire list deserialization
// and block reconcile for records we DO manage.
func TestSvcParamsUnmarshal_Null(t *testing.T) {
	var s svcParams
	if err := json.Unmarshal([]byte("null"), &s); err != nil {
		t.Fatalf("Unmarshal(null) err = %v", err)
	}
	if string(s) != "" {
		t.Errorf("Unmarshal(null) = %q, want empty", string(s))
	}
}

// TestSvcParamsUnmarshal_ObjectForm is the regression test for the bug
// reported from production: Technitium's zones/records/get endpoint
// returns svcParams as a nested object, and the previous string-typed
// field failed to unmarshal, breaking the whole zone listing.
func TestSvcParamsUnmarshal_ObjectForm(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string // normalized pipe form
	}{
		{
			name: "alpn only",
			in:   `{"alpn":{"alpnIds":["h2"]}}`,
			want: "alpn|h2",
		},
		{
			name: "alpn multi-value",
			in:   `{"alpn":{"alpnIds":["h2","h3"]}}`,
			want: "alpn|h2,h3",
		},
		{
			name: "port only",
			in:   `{"port":{"port":443}}`,
			want: "port|443",
		},
		{
			name: "ipv4hint",
			in:   `{"ipv4hint":{"addresses":["10.0.0.1","10.0.0.2"]}}`,
			want: "ipv4hint|10.0.0.1,10.0.0.2",
		},
		{
			name: "ipv6hint",
			in:   `{"ipv6hint":{"addresses":["::1"]}}`,
			want: "ipv6hint|::1",
		},
		{
			// Multiple keys must emit in lexicographic order so the
			// pipe form is stable across reconciles (feeds ProviderID).
			name: "multiple keys, stable order",
			in:   `{"port":{"port":443},"alpn":{"alpnIds":["h2"]}}`,
			want: "alpn|h2 port|443",
		},
		{
			name: "empty object",
			in:   `{}`,
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var s svcParams
			if err := json.Unmarshal([]byte(tt.in), &s); err != nil {
				t.Fatalf("Unmarshal(%s) err = %v", tt.in, err)
			}
			if string(s) != tt.want {
				t.Errorf("Unmarshal(%s) = %q, want %q", tt.in, string(s), tt.want)
			}
		})
	}
}

// TestSvcParamsUnmarshal_UnknownKey pins that unrecognized object keys
// don't break the listing — they fall through to a compact-JSON form,
// preserving record identity determinism even for parameter types we
// don't explicitly handle.
func TestSvcParamsUnmarshal_UnknownKey(t *testing.T) {
	var s svcParams
	in := `{"futureparam":{"some":"value","n":42}}`
	if err := json.Unmarshal([]byte(in), &s); err != nil {
		t.Fatalf("Unmarshal err = %v", err)
	}
	if string(s) == "" {
		t.Error("expected non-empty output for unknown key")
	}
	// Idempotence: parsing the same input twice should yield the same
	// output (otherwise ProviderID would thrash across reconciles).
	var s2 svcParams
	if err := json.Unmarshal([]byte(in), &s2); err != nil {
		t.Fatalf("Unmarshal(second) err = %v", err)
	}
	if string(s) != string(s2) {
		t.Errorf("non-deterministic: first=%q second=%q", string(s), string(s2))
	}
}

// TestApiRecord_HTTPSObjectForm_DoesNotBreakListing is the end-to-end
// regression test: a full zones/records/get response payload containing
// an HTTPS record in the new object form must deserialize without error,
// AND the A record in the same payload must come through correctly.
// This is the actual production failure mode — HTTPS records blocked
// reconcile for the A records we do manage.
func TestApiRecord_HTTPSObjectForm_DoesNotBreakListing(t *testing.T) {
	payload := `{
		"zone":{"name":"example.com","type":"Primary","disabled":false},
		"name":"example.com",
		"records":[
			{
				"name":"app.example.com",
				"type":"A",
				"ttl":300,
				"rData":{"ipAddress":"10.0.0.1"}
			},
			{
				"name":"svc.example.com",
				"type":"HTTPS",
				"ttl":300,
				"rData":{
					"svcPriority":1,
					"svcTargetName":".",
					"svcParams":{"alpn":{"alpnIds":["h2"]}}
				}
			}
		]
	}`
	var resp zoneRecordsResponse
	if err := json.Unmarshal([]byte(payload), &resp); err != nil {
		t.Fatalf("full listing failed to parse: %v", err)
	}
	if len(resp.Records) != 2 {
		t.Fatalf("expected 2 records, got %d", len(resp.Records))
	}
	if resp.Records[0].Type != "A" || resp.Records[0].RData.IPAddress != "10.0.0.1" {
		t.Errorf("A record not parsed correctly: %+v", resp.Records[0])
	}
	if resp.Records[1].Type != "HTTPS" {
		t.Fatalf("HTTPS record missing")
	}
	if got := string(resp.Records[1].RData.SvcParams); got != "alpn|h2" {
		t.Errorf("HTTPS svcParams = %q, want %q", got, "alpn|h2")
	}
}
