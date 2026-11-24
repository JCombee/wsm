package main

import (
	"strings"
	"testing"
)

func validConfig() Config {
	return Config{
		AuditPath:       "/var/lib/s06/audit.jsonl",
		AdminSocket:     "/run/wsm/api.sock",
		PodmanSocketDir: "/run/wsm-podman",
		Tenants: []TenantConfig{
			{Project: "s06a", ControlRoomUID: 61001, SocketDir: "/run/s06/tenant/s06a", MaxSandboxes: 2},
			{Project: "s06b", ControlRoomUID: 61002, SocketDir: "/run/s06/tenant/s06b", MaxSandboxes: 2},
		},
	}
}

func TestConfigValidate(t *testing.T) {
	tests := []struct {
		name   string
		change func(*Config)
		want   string
	}{
		{"valid", func(*Config) {}, ""},
		{"relative audit path", func(c *Config) { c.AuditPath = "audit.jsonl" }, "audit_path"},
		{"unclean socket dir", func(c *Config) { c.Tenants[0].SocketDir = "/run/s06/../x" }, "socket_dir"},
		{"root control room", func(c *Config) { c.Tenants[0].ControlRoomUID = 0 }, "root"},
		{"bad slug", func(c *Config) { c.Tenants[0].Project = "S06A" }, "slug"},
		{"no sandboxes", func(c *Config) { c.Tenants[0].MaxSandboxes = 0 }, "max_sandboxes"},
		{"duplicate uid", func(c *Config) { c.Tenants[1].ControlRoomUID = 61001 }, "unique"},
		{"duplicate dir", func(c *Config) { c.Tenants[1].SocketDir = c.Tenants[0].SocketDir }, "unique"},
		{"no tenants", func(c *Config) { c.Tenants = nil }, "tenants"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := validConfig()
			tt.change(&c)
			err := c.Validate()
			if tt.want == "" {
				if err != nil {
					t.Fatalf("Validate: %v", err)
				}
				if c.Tenants[0].MaxExecs != defaultMaxExecs {
					t.Fatalf("MaxExecs = %d, want the default %d", c.Tenants[0].MaxExecs, defaultMaxExecs)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Validate = %v, want an error about %q", err, tt.want)
			}
		})
	}
}

func TestDecodeStrict(t *testing.T) {
	var v struct {
		A int `json:"a"`
	}
	tests := []struct {
		in      string
		wantErr bool
	}{
		{`{"a":1}`, false},
		{`{"a":1,"b":2}`, true},
		{`{"a":1}{"a":2}`, true},
		{`{"a":1} x`, true},
		{strings.Repeat(" ", 100) + `{"a":1}`, true},
	}
	for _, tt := range tests {
		err := decodeStrict(strings.NewReader(tt.in), 64, &v)
		if (err != nil) != tt.wantErr {
			t.Errorf("decodeStrict(%.20q) = %v, want error %v", tt.in, err, tt.wantErr)
		}
	}
}
