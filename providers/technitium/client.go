// Package technitium implements the DNSWeaver provider interface for Technitium DNS Server.
package technitium

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"gitlab.bluewillows.net/root/dnsweaver/pkg/httputil"
	"gitlab.bluewillows.net/root/dnsweaver/pkg/provider"
)

// apiRecord represents a DNS record from the Technitium API.
type apiRecord struct {
	Name     string   `json:"name"`
	Type     string   `json:"type"`
	TTL      int      `json:"ttl"`
	RData    apiRData `json:"rData"`
	Disabled bool     `json:"disabled"`
}

// apiRData contains the record-specific data from Technitium.
type apiRData struct {
	IPAddress string `json:"ipAddress,omitempty"` // For A/AAAA records
	CName     string `json:"cname,omitempty"`     // For CNAME records
	Text      string `json:"text,omitempty"`      // For TXT records
	// SRV record fields
	Priority  int    `json:"priority,omitempty"` // For SRV records
	Weight    int    `json:"weight,omitempty"`   // For SRV records
	Port      int    `json:"port,omitempty"`     // For SRV records
	SrvTarget string `json:"target,omitempty"`   // For SRV records
	// HTTPS/SVCB record fields
	SvcPriority   int       `json:"svcPriority,omitempty"`   // For HTTPS/SVCB records
	SvcTargetName string    `json:"svcTargetName,omitempty"` // For HTTPS/SVCB records ("." = self)
	SvcParams     svcParams `json:"svcParams,omitempty"`     // For HTTPS/SVCB records (see svcParams docs)
}

// svcParams is the normalized pipe-separated representation of HTTPS/SVCB
// service parameters (e.g. "alpn|h2 port|443"). Technitium's API is
// inconsistent across endpoints and versions:
//
//   - Older servers and the add/delete query-string endpoints use/accept the
//     pipe form: svcParams=alpn|h2
//   - Newer zones/records/get responses return it as a nested JSON object:
//     {"alpn":{"alpnIds":["h2"]},"port":{"port":443}}
//   - Some responses omit it entirely or return null
//
// Any of those must parse without error, otherwise the whole record list
// (including A/TXT records we DO manage) fails to deserialize and the
// reconciler cannot run. We normalize to the pipe form on read so all
// downstream consumers — extractALPN, ProviderID construction, add/delete
// round-trips — continue to work with a single representation.
type svcParams string

// UnmarshalJSON accepts string, object, and null forms from the Technitium
// API and stores the pipe-separated normalized value.
func (s *svcParams) UnmarshalJSON(data []byte) error {
	if len(data) == 0 || string(data) == "null" {
		*s = ""
		return nil
	}
	// String form: "alpn|h2"
	if data[0] == '"' {
		var raw string
		if err := json.Unmarshal(data, &raw); err != nil {
			return fmt.Errorf("svcParams string: %w", err)
		}
		*s = svcParams(raw)
		return nil
	}
	// Object form: {"alpn":{"alpnIds":["h2"]},"port":{"port":443},...}
	// We only need to round-trip known keys safely. Unknown keys are
	// preserved as key|<compact-json> so record identity (ProviderID)
	// stays deterministic even for parameter types we don't explicitly
	// understand — rather than silently dropping them.
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(data, &obj); err != nil {
		return fmt.Errorf("svcParams object: %w", err)
	}
	*s = svcParams(flattenSvcParamsObject(obj))
	return nil
}

// MarshalJSON is defined so tests that round-trip apiRData through JSON
// produce a well-formed document. On the wire we only serialize to
// Technitium via query strings (see AddHTTPSRecord), never via JSON body.
func (s svcParams) MarshalJSON() ([]byte, error) {
	return json.Marshal(string(s))
}

// String returns the pipe-separated form so callers can treat svcParams
// as if it were a plain string.
func (s svcParams) String() string { return string(s) }

// flattenSvcParamsObject converts Technitium's object-form svcParams into
// the pipe-separated string form that the rest of dnsweaver expects.
//
// Known shapes:
//
//	{"alpn":{"alpnIds":["h2","h3"]}}   → "alpn|h2,h3"
//	{"port":{"port":443}}              → "port|443"
//	{"ipv4hint":{"addresses":["1.2.3.4"]}} → "ipv4hint|1.2.3.4"
//	{"ipv6hint":{"addresses":["::1"]}} → "ipv6hint|::1"
//
// Unknown keys are serialized as key|<compact-json-of-value> so record
// identity remains deterministic across reconciles. Keys are emitted in
// lexicographic order so the output is stable regardless of map
// iteration order — stability matters because the result feeds into
// ProviderID.
func flattenSvcParamsObject(obj map[string]json.RawMessage) string {
	if len(obj) == 0 {
		return ""
	}
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var parts []string
	for _, k := range keys {
		val := obj[k]
		switch k {
		case "alpn":
			var v struct {
				ALPNIDs []string `json:"alpnIds"`
			}
			if err := json.Unmarshal(val, &v); err == nil && len(v.ALPNIDs) > 0 {
				parts = append(parts, "alpn|"+strings.Join(v.ALPNIDs, ","))
				continue
			}
		case "port":
			var v struct {
				Port int `json:"port"`
			}
			if err := json.Unmarshal(val, &v); err == nil && v.Port != 0 {
				parts = append(parts, fmt.Sprintf("port|%d", v.Port))
				continue
			}
		case "ipv4hint", "ipv6hint":
			var v struct {
				Addresses []string `json:"addresses"`
			}
			if err := json.Unmarshal(val, &v); err == nil && len(v.Addresses) > 0 {
				parts = append(parts, k+"|"+strings.Join(v.Addresses, ","))
				continue
			}
		}
		// Fallback: preserve the key+compact-JSON for determinism. Worst
		// case this doesn't round-trip into an add/delete call correctly,
		// but dnsweaver doesn't create unknown svcParam types anyway.
		parts = append(parts, k+"|"+string(bytes.TrimSpace(val)))
	}
	return strings.Join(parts, " ")
}

// apiResponse is the standard Technitium API response wrapper.
type apiResponse struct {
	Status       string          `json:"status"`
	ErrorMessage string          `json:"errorMessage,omitempty"`
	Response     json.RawMessage `json:"response,omitempty"`
}

// zoneRecordsResponse is the response from the zones/records/get endpoint.
type zoneRecordsResponse struct {
	Zone    zoneInfo    `json:"zone"`
	Name    string      `json:"name"`
	Records []apiRecord `json:"records"`
}

// zoneInfo contains zone metadata from the API response.
type zoneInfo struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Disabled bool   `json:"disabled"`
}

// Client is a Technitium DNS Server API client.
type Client struct {
	baseURL    string
	token      string
	httpClient *http.Client
	logger     *slog.Logger
}

// ClientOption is a functional option for configuring the Client.
type ClientOption func(*Client)

// WithHTTPClient sets a custom HTTP client.
func WithHTTPClient(httpClient *http.Client) ClientOption {
	return func(c *Client) {
		c.httpClient = httpClient
	}
}

// WithLogger sets a custom logger.
func WithLogger(logger *slog.Logger) ClientOption {
	return func(c *Client) {
		if logger != nil {
			c.logger = logger
		}
	}
}

// WithInsecureSkipVerify configures the client to skip TLS certificate verification.
// WARNING: This should only be used for testing or when connecting to servers with
// self-signed certificates. It is insecure and should not be used in production.
func WithInsecureSkipVerify(skip bool) ClientOption {
	return func(c *Client) {
		if skip {
			c.httpClient = httputil.NewClient(&httputil.ClientConfig{
				TLSSkipVerify: true,
			})
		}
	}
}

// NewClient creates a new Technitium API client.
func NewClient(baseURL, token string, opts ...ClientOption) *Client {
	c := &Client{
		baseURL:    baseURL,
		token:      token,
		httpClient: httputil.DefaultClient(),
		logger:     slog.Default(),
	}

	for _, opt := range opts {
		opt(c)
	}

	return c
}

// doRequest performs an HTTP request to the Technitium API.
func (c *Client) doRequest(ctx context.Context, endpoint string, params url.Values) (*apiResponse, error) {
	// Add token to params
	if params == nil {
		params = url.Values{}
	}
	params.Set("token", c.token)

	reqURL := fmt.Sprintf("%s%s?%s", c.baseURL, endpoint, params.Encode())

	c.logger.Debug("making API request",
		slog.String("endpoint", endpoint),
		slog.String("url", c.baseURL+endpoint),
	)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("executing request: %w", err)
	}
	defer resp.Body.Close()

	body, err := httputil.ReadBody(resp, 0)
	if err != nil {
		return nil, fmt.Errorf("reading response body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status code %d: %s", resp.StatusCode, string(body))
	}

	var apiResp apiResponse
	if err := json.Unmarshal(body, &apiResp); err != nil {
		return nil, fmt.Errorf("parsing response JSON: %w", err)
	}

	if apiResp.Status == "error" {
		// Detect "record already exists" errors and return ErrConflict.
		// Technitium returns different messages:
		// - "Record already exists" (general)
		// - "Identical record already exists" (error code 81058)
		errLower := strings.ToLower(apiResp.ErrorMessage)
		if strings.Contains(errLower, "record already exists") ||
			strings.Contains(errLower, "identical record") {
			return nil, fmt.Errorf("API error: %s: %w", apiResp.ErrorMessage, provider.ErrConflict)
		}
		// Detect CNAME type conflicts.
		// Technitium returns: "a CNAME record cannot exists with other record types"
		if strings.Contains(errLower, "cname record cannot") {
			return nil, fmt.Errorf("API error: %s: %w", apiResp.ErrorMessage, provider.ErrTypeConflict)
		}
		return nil, fmt.Errorf("API error: %s", apiResp.ErrorMessage)
	}

	return &apiResp, nil
}

// Ping checks connectivity to the Technitium server.
// Uses the /api/user/session/get endpoint which is lightweight.
func (c *Client) Ping(ctx context.Context) error {
	_, err := c.doRequest(ctx, "/api/user/session/get", nil)
	if err != nil {
		return fmt.Errorf("ping failed: %w", err)
	}
	return nil
}

// AddARecord creates an A record in the specified zone.
func (c *Client) AddARecord(ctx context.Context, zone, hostname, ip string, ttl int) error {
	params := url.Values{}
	params.Set("zone", zone)
	params.Set("domain", hostname)
	params.Set("type", "A")
	params.Set("ipAddress", ip)
	params.Set("ttl", strconv.Itoa(ttl))

	_, err := c.doRequest(ctx, "/api/zones/records/add", params)
	if err != nil {
		return fmt.Errorf("adding A record for %s: %w", hostname, err)
	}

	c.logger.Info("added A record",
		slog.String("hostname", hostname),
		slog.String("ip", ip),
		slog.String("zone", zone),
		slog.Int("ttl", ttl),
	)

	return nil
}

// AddAAAARecord creates an AAAA (IPv6) record in the specified zone.
func (c *Client) AddAAAARecord(ctx context.Context, zone, hostname, ip string, ttl int) error {
	params := url.Values{}
	params.Set("zone", zone)
	params.Set("domain", hostname)
	params.Set("type", "AAAA")
	params.Set("ipAddress", ip)
	params.Set("ttl", strconv.Itoa(ttl))

	_, err := c.doRequest(ctx, "/api/zones/records/add", params)
	if err != nil {
		return fmt.Errorf("adding AAAA record for %s: %w", hostname, err)
	}

	c.logger.Info("added AAAA record",
		slog.String("hostname", hostname),
		slog.String("ip", ip),
		slog.String("zone", zone),
		slog.Int("ttl", ttl),
	)

	return nil
}

// AddCNAMERecord creates a CNAME record in the specified zone.
func (c *Client) AddCNAMERecord(ctx context.Context, zone, hostname, target string, ttl int) error {
	params := url.Values{}
	params.Set("zone", zone)
	params.Set("domain", hostname)
	params.Set("type", "CNAME")
	params.Set("cname", target)
	params.Set("ttl", strconv.Itoa(ttl))

	_, err := c.doRequest(ctx, "/api/zones/records/add", params)
	if err != nil {
		return fmt.Errorf("adding CNAME record for %s: %w", hostname, err)
	}

	c.logger.Info("added CNAME record",
		slog.String("hostname", hostname),
		slog.String("target", target),
		slog.String("zone", zone),
		slog.Int("ttl", ttl),
	)

	return nil
}

// DeleteARecord removes an A record from the specified zone.
func (c *Client) DeleteARecord(ctx context.Context, zone, hostname, ip string) error {
	params := url.Values{}
	params.Set("zone", zone)
	params.Set("domain", hostname)
	params.Set("type", "A")
	params.Set("ipAddress", ip)

	_, err := c.doRequest(ctx, "/api/zones/records/delete", params)
	if err != nil {
		return fmt.Errorf("deleting A record for %s: %w", hostname, err)
	}

	c.logger.Info("deleted A record",
		slog.String("hostname", hostname),
		slog.String("ip", ip),
		slog.String("zone", zone),
	)

	return nil
}

// DeleteAAAARecord removes an AAAA (IPv6) record from the specified zone.
func (c *Client) DeleteAAAARecord(ctx context.Context, zone, hostname, ip string) error {
	params := url.Values{}
	params.Set("zone", zone)
	params.Set("domain", hostname)
	params.Set("type", "AAAA")
	params.Set("ipAddress", ip)

	_, err := c.doRequest(ctx, "/api/zones/records/delete", params)
	if err != nil {
		return fmt.Errorf("deleting AAAA record for %s: %w", hostname, err)
	}

	c.logger.Info("deleted AAAA record",
		slog.String("hostname", hostname),
		slog.String("ip", ip),
		slog.String("zone", zone),
	)

	return nil
}

// DeleteCNAMERecord removes a CNAME record from the specified zone.
func (c *Client) DeleteCNAMERecord(ctx context.Context, zone, hostname, target string) error {
	params := url.Values{}
	params.Set("zone", zone)
	params.Set("domain", hostname)
	params.Set("type", "CNAME")
	params.Set("cname", target)

	_, err := c.doRequest(ctx, "/api/zones/records/delete", params)
	if err != nil {
		return fmt.Errorf("deleting CNAME record for %s: %w", hostname, err)
	}

	c.logger.Info("deleted CNAME record",
		slog.String("hostname", hostname),
		slog.String("target", target),
		slog.String("zone", zone),
	)

	return nil
}

// AddTXTRecord creates a TXT record in the specified zone.
func (c *Client) AddTXTRecord(ctx context.Context, zone, hostname, text string, ttl int) error {
	params := url.Values{}
	params.Set("zone", zone)
	params.Set("domain", hostname)
	params.Set("type", "TXT")
	params.Set("text", text)
	params.Set("ttl", strconv.Itoa(ttl))

	_, err := c.doRequest(ctx, "/api/zones/records/add", params)
	if err != nil {
		return fmt.Errorf("adding TXT record for %s: %w", hostname, err)
	}

	c.logger.Info("added TXT record",
		slog.String("hostname", hostname),
		slog.String("text", text),
		slog.String("zone", zone),
		slog.Int("ttl", ttl),
	)

	return nil
}

// DeleteTXTRecord removes a TXT record from the specified zone.
func (c *Client) DeleteTXTRecord(ctx context.Context, zone, hostname, text string) error {
	params := url.Values{}
	params.Set("zone", zone)
	params.Set("domain", hostname)
	params.Set("type", "TXT")
	params.Set("text", text)

	_, err := c.doRequest(ctx, "/api/zones/records/delete", params)
	if err != nil {
		return fmt.Errorf("deleting TXT record for %s: %w", hostname, err)
	}

	c.logger.Info("deleted TXT record",
		slog.String("hostname", hostname),
		slog.String("text", text),
		slog.String("zone", zone),
	)

	return nil
}

// AddSRVRecord creates an SRV record in the specified zone.
func (c *Client) AddSRVRecord(ctx context.Context, zone, hostname string, priority, weight, port int, target string, ttl int) error {
	params := url.Values{}
	params.Set("zone", zone)
	params.Set("domain", hostname)
	params.Set("type", "SRV")
	params.Set("priority", strconv.Itoa(priority))
	params.Set("weight", strconv.Itoa(weight))
	params.Set("port", strconv.Itoa(port))
	params.Set("target", target)
	params.Set("ttl", strconv.Itoa(ttl))

	_, err := c.doRequest(ctx, "/api/zones/records/add", params)
	if err != nil {
		return fmt.Errorf("adding SRV record for %s: %w", hostname, err)
	}

	c.logger.Info("added SRV record",
		slog.String("hostname", hostname),
		slog.Int("priority", priority),
		slog.Int("weight", weight),
		slog.Int("port", port),
		slog.String("target", target),
		slog.String("zone", zone),
		slog.Int("ttl", ttl),
	)

	return nil
}

// DeleteSRVRecord removes an SRV record from the specified zone.
func (c *Client) DeleteSRVRecord(ctx context.Context, zone, hostname string, priority, weight, port int, target string) error {
	params := url.Values{}
	params.Set("zone", zone)
	params.Set("domain", hostname)
	params.Set("type", "SRV")
	params.Set("priority", strconv.Itoa(priority))
	params.Set("weight", strconv.Itoa(weight))
	params.Set("port", strconv.Itoa(port))
	params.Set("target", target)

	_, err := c.doRequest(ctx, "/api/zones/records/delete", params)
	if err != nil {
		return fmt.Errorf("deleting SRV record for %s: %w", hostname, err)
	}

	c.logger.Info("deleted SRV record",
		slog.String("hostname", hostname),
		slog.Int("priority", priority),
		slog.Int("weight", weight),
		slog.Int("port", port),
		slog.String("target", target),
		slog.String("zone", zone),
	)

	return nil
}

// AddHTTPSRecord creates an HTTPS (SVCB Type 65) record in the specified zone.
// The svcPriority controls record mode: 0 = AliasMode, 1+ = ServiceMode.
// The svcTargetName uses "." to indicate the record's owner name (self-referential).
// The svcParams is in Technitium's pipe-separated format (e.g., "alpn|h2").
func (c *Client) AddHTTPSRecord(ctx context.Context, zone, hostname string, svcPriority int, svcTargetName, svcParams string, ttl int) error {
	params := url.Values{}
	params.Set("zone", zone)
	params.Set("domain", hostname)
	params.Set("type", "HTTPS")
	params.Set("svcPriority", strconv.Itoa(svcPriority))
	params.Set("svcTargetName", svcTargetName)
	params.Set("svcParams", svcParams)
	params.Set("ttl", strconv.Itoa(ttl))

	_, err := c.doRequest(ctx, "/api/zones/records/add", params)
	if err != nil {
		return fmt.Errorf("adding HTTPS record for %s: %w", hostname, err)
	}

	c.logger.Info("added HTTPS record",
		slog.String("hostname", hostname),
		slog.Int("svc_priority", svcPriority),
		slog.String("svc_target", svcTargetName),
		slog.String("svc_params", svcParams),
		slog.String("zone", zone),
		slog.Int("ttl", ttl),
	)

	return nil
}

// DeleteHTTPSRecord removes an HTTPS (SVCB Type 65) record from the specified zone.
// Note: implemented earlier; duplicate guard to ensure compile stability when patching.

// parseSvcParams splits svcParams into key/value pairs for future use.
// Input: "alpn|h2" → map{"alpn":"h2"}
func parseSvcParams(s string) map[string]string {
	out := make(map[string]string)
	for _, part := range strings.Split(s, " ") {
		if part == "" {
			continue
		}
		kv := strings.SplitN(part, "|", 2)
		if len(kv) == 2 {
			out[kv[0]] = kv[1]
		}
	}
	return out
}

// DeleteHTTPSRecord removes an HTTPS (SVCB Type 65) record from the specified zone.
func (c *Client) DeleteHTTPSRecord(ctx context.Context, zone, hostname string, svcPriority int, svcTargetName, svcParams string) error {
	params := url.Values{}
	params.Set("zone", zone)
	params.Set("domain", hostname)
	params.Set("type", "HTTPS")
	params.Set("svcPriority", strconv.Itoa(svcPriority))
	params.Set("svcTargetName", svcTargetName)
	params.Set("svcParams", svcParams)

	_, err := c.doRequest(ctx, "/api/zones/records/delete", params)
	if err != nil {
		return fmt.Errorf("deleting HTTPS record for %s: %w", hostname, err)
	}

	c.logger.Info("deleted HTTPS record",
		slog.String("hostname", hostname),
		slog.Int("svc_priority", svcPriority),
		slog.String("svc_target", svcTargetName),
		slog.String("svc_params", svcParams),
		slog.String("zone", zone),
	)

	return nil
}

// UpdateARecord updates an A record's target IP address in the specified zone.
// The Technitium API requires the old IP to identify the record.
func (c *Client) UpdateARecord(ctx context.Context, zone, hostname, oldIP, newIP string, ttl int) error {
	params := url.Values{}
	params.Set("zone", zone)
	params.Set("domain", hostname)
	params.Set("type", "A")
	params.Set("ipAddress", oldIP)
	params.Set("newIpAddress", newIP)
	params.Set("ttl", strconv.Itoa(ttl))

	_, err := c.doRequest(ctx, "/api/zones/records/update", params)
	if err != nil {
		return fmt.Errorf("updating A record for %s: %w", hostname, err)
	}

	c.logger.Info("updated A record",
		slog.String("hostname", hostname),
		slog.String("old_ip", oldIP),
		slog.String("new_ip", newIP),
		slog.String("zone", zone),
		slog.Int("ttl", ttl),
	)

	return nil
}

// UpdateAAAARecord updates an AAAA (IPv6) record's target IP address in the specified zone.
func (c *Client) UpdateAAAARecord(ctx context.Context, zone, hostname, oldIP, newIP string, ttl int) error {
	params := url.Values{}
	params.Set("zone", zone)
	params.Set("domain", hostname)
	params.Set("type", "AAAA")
	params.Set("ipAddress", oldIP)
	params.Set("newIpAddress", newIP)
	params.Set("ttl", strconv.Itoa(ttl))

	_, err := c.doRequest(ctx, "/api/zones/records/update", params)
	if err != nil {
		return fmt.Errorf("updating AAAA record for %s: %w", hostname, err)
	}

	c.logger.Info("updated AAAA record",
		slog.String("hostname", hostname),
		slog.String("old_ip", oldIP),
		slog.String("new_ip", newIP),
		slog.String("zone", zone),
		slog.Int("ttl", ttl),
	)

	return nil
}

// UpdateCNAMERecord updates a CNAME record's target in the specified zone.
func (c *Client) UpdateCNAMERecord(ctx context.Context, zone, hostname, oldTarget, newTarget string, ttl int) error {
	params := url.Values{}
	params.Set("zone", zone)
	params.Set("domain", hostname)
	params.Set("type", "CNAME")
	params.Set("cname", oldTarget)
	params.Set("newCname", newTarget)
	params.Set("ttl", strconv.Itoa(ttl))

	_, err := c.doRequest(ctx, "/api/zones/records/update", params)
	if err != nil {
		return fmt.Errorf("updating CNAME record for %s: %w", hostname, err)
	}

	c.logger.Info("updated CNAME record",
		slog.String("hostname", hostname),
		slog.String("old_target", oldTarget),
		slog.String("new_target", newTarget),
		slog.String("zone", zone),
		slog.Int("ttl", ttl),
	)

	return nil
}

// GetRecords retrieves all records for a given hostname in the specified zone.
func (c *Client) GetRecords(ctx context.Context, zone, hostname string) ([]apiRecord, error) {
	params := url.Values{}
	params.Set("zone", zone)
	params.Set("domain", hostname)

	apiResp, err := c.doRequest(ctx, "/api/zones/records/get", params)
	if err != nil {
		return nil, fmt.Errorf("getting records for %s: %w", hostname, err)
	}

	var recordsResp zoneRecordsResponse
	if err := json.Unmarshal(apiResp.Response, &recordsResp); err != nil {
		return nil, fmt.Errorf("parsing records response: %w", err)
	}

	c.logger.Debug("retrieved records",
		slog.String("hostname", hostname),
		slog.String("zone", zone),
		slog.Int("count", len(recordsResp.Records)),
	)

	return recordsResp.Records, nil
}

// ListZoneRecords retrieves all records in a zone.
// This is used for listing all managed records.
func (c *Client) ListZoneRecords(ctx context.Context, zone string) ([]apiRecord, error) {
	params := url.Values{}
	params.Set("zone", zone)
	// The domain parameter is required by the API even with listZone=true
	// Setting it to the zone apex returns all records in the zone
	params.Set("domain", zone)
	params.Set("listZone", "true")

	apiResp, err := c.doRequest(ctx, "/api/zones/records/get", params)
	if err != nil {
		return nil, fmt.Errorf("listing zone %s: %w", zone, err)
	}

	// The listZone response has a slightly different format
	var result struct {
		Zone    zoneInfo    `json:"zone"`
		Records []apiRecord `json:"records"`
	}
	if err := json.Unmarshal(apiResp.Response, &result); err != nil {
		return nil, fmt.Errorf("parsing zone records response: %w", err)
	}

	c.logger.Debug("listed zone records",
		slog.String("zone", zone),
		slog.Int("count", len(result.Records)),
	)

	return result.Records, nil
}
