package config

import (
	"testing"

	"gitlab.bluewillows.net/root/dnsweaver/pkg/provider"
)

func TestValidateTargetRecordType(t *testing.T) {
	tests := []struct {
		name       string
		recordType provider.RecordType
		target     string
		wantErr    bool
		errMatch   string
	}{
		{
			name:       "A record with IP is valid",
			recordType: provider.RecordTypeA,
			target:     "10.0.0.100",
			wantErr:    false,
		},
		{
			name:       "A record with IPv6 is invalid",
			recordType: provider.RecordTypeA,
			target:     "2001:db8::1",
			wantErr:    true,
			errMatch:   "A records must point to an IPv4 address",
		},
		{
			name:       "A record with hostname is invalid",
			recordType: provider.RecordTypeA,
			target:     "example.com",
			wantErr:    true,
			errMatch:   "A records must point to an IP",
		},
		{
			name:       "CNAME record with hostname is valid",
			recordType: provider.RecordTypeCNAME,
			target:     "example.com",
			wantErr:    false,
		},
		{
			name:       "CNAME record with subdomain is valid",
			recordType: provider.RecordTypeCNAME,
			target:     "app.example.com",
			wantErr:    false,
		},
		{
			name:       "CNAME record with IP is invalid",
			recordType: provider.RecordTypeCNAME,
			target:     "10.0.0.100",
			wantErr:    true,
			errMatch:   "CNAME records cannot point to IP",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			inst := &ProviderInstanceConfig{
				Name:       "test",
				RecordType: tc.recordType,
				Target:     tc.target,
			}

			errs := validateTargetRecordType(inst)

			if tc.wantErr {
				if len(errs) == 0 {
					t.Error("expected validation error, got none")
					return
				}
				found := false
				for _, err := range errs {
					if containsSubstring(err.Error(), tc.errMatch) {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("expected error containing %q, got %v", tc.errMatch, errs)
				}
			} else {
				if len(errs) > 0 {
					t.Errorf("unexpected errors: %v", errs)
				}
			}
		})
	}
}

func TestValidateConfig_DuplicateProviderNames(t *testing.T) {
	cfg := &Config{
		Global: &GlobalConfig{
			LogLevel:  "info",
			LogFormat: "text",
		},
		ProviderNames: []string{"dns1", "dns1"},
		ProviderInstances: []*ProviderInstanceConfig{
			{Name: "dns1", TypeName: "technitium", RecordType: "A", Target: "10.0.0.1"},
			{Name: "dns1", TypeName: "cloudflare", RecordType: "A", Target: "10.0.0.2"},
		},
	}

	errs := validateConfig(cfg)

	if len(errs) == 0 {
		t.Error("expected duplicate name error, got none")
		return
	}

	found := false
	for _, err := range errs {
		if containsSubstring(err.Error(), "duplicate") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected error about duplicate names, got %v", errs)
	}
}

// TestValidateConfig_DockerMode_Both pins that "both" is accepted for
// docker_mode. Guards against regression when touching the allowed value
// list and ensures the validate.go switch stays aligned with the real
// docker.ModeBoth runtime value.
func TestValidateConfig_DockerMode_Both(t *testing.T) {
	cfg := &Config{
		Global: &GlobalConfig{
			LogLevel:   "info",
			LogFormat:  "text",
			DockerMode: "both",
			Platform:   "docker",
		},
	}
	errs := validateConfig(cfg)
	for _, err := range errs {
		if containsSubstring(err.Error(), "docker_mode") {
			t.Errorf("docker_mode=both should be valid, got error: %v", err)
		}
	}
}

// TestValidateConfig_DockerMode_Invalid pins that unexpected values remain
// rejected, so the expanded allow-list can't accidentally turn into a
// permissive accept-anything switch.
func TestValidateConfig_DockerMode_Invalid(t *testing.T) {
	cfg := &Config{
		Global: &GlobalConfig{
			LogLevel:   "info",
			LogFormat:  "text",
			DockerMode: "bogus",
			Platform:   "docker",
		},
	}
	errs := validateConfig(cfg)
	found := false
	for _, err := range errs {
		if containsSubstring(err.Error(), "docker_mode") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected docker_mode validation error for invalid value, got %v", errs)
	}
}

func TestValidationError_SingleError(t *testing.T) {
	err := &ValidationError{Errors: []*ConfigError{configErr("test_field", "single error message")}}
	got := err.Error()
	want := "configuration error:\n  test_field: single error message"

	if got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestValidationError_MultipleErrors(t *testing.T) {
	err := &ValidationError{Errors: []*ConfigError{
		configErr("field1", "error 1"),
		configErr("field2", "error 2"),
		configErr("field3", "error 3"),
	}}
	got := err.Error()

	// Should contain count and all errors
	if !containsSubstring(got, "3 configuration errors") {
		t.Errorf("Error() should contain '3 configuration errors', got %q", got)
	}
	if !containsSubstring(got, "error 1") {
		t.Errorf("Error() should contain 'error 1', got %q", got)
	}
	if !containsSubstring(got, "error 2") {
		t.Errorf("Error() should contain 'error 2', got %q", got)
	}
	if !containsSubstring(got, "error 3") {
		t.Errorf("Error() should contain 'error 3', got %q", got)
	}
}

func TestValidateProviderType(t *testing.T) {
	knownTypes := []string{"technitium", "cloudflare", "webhook"}

	tests := []struct {
		typeName string
		wantErr  bool
	}{
		{"technitium", false},
		{"cloudflare", false},
		{"webhook", false},
		{"unknown", true},
		{"route53", true},
	}

	for _, tc := range tests {
		err := validateProviderType(tc.typeName, knownTypes)

		if tc.wantErr {
			if err == nil {
				t.Errorf("validateProviderType(%q) = nil, want error", tc.typeName)
			}
		} else {
			if err != nil {
				t.Errorf("validateProviderType(%q) = %v, want nil", tc.typeName, err)
			}
		}
	}
}
