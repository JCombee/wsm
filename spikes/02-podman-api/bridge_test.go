package main

import (
	"strings"
	"testing"
)

func TestParseControl(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    control
		wantErr bool
	}{
		{"resize", `{"type":"resize","rows":37,"cols":123}`, control{"resize", 37, 123}, false},
		{"bounds", `{"type":"resize","rows":1,"cols":1000}`, control{"resize", 1, 1000}, false},
		{"zero rows", `{"type":"resize","rows":0,"cols":80}`, control{}, true},
		{"negative cols", `{"type":"resize","rows":24,"cols":-1}`, control{}, true},
		{"too large", `{"type":"resize","rows":24,"cols":1001}`, control{}, true},
		{"unknown type", `{"type":"exec","rows":24,"cols":80}`, control{}, true},
		{"unknown field", `{"type":"resize","rows":24,"cols":80,"cmd":"sh"}`, control{}, true},
		{"trailing data", `{"type":"resize","rows":24,"cols":80}{}`, control{}, true},
		{"not json", `resize 24 80`, control{}, true},
		{"string number", `{"type":"resize","rows":"24","cols":80}`, control{}, true},
		{"oversized", `{"type":"resize","rows":24,"cols":80}` + strings.Repeat(" ", maxControlMessage), control{}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseControl([]byte(tt.in))
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && got != tt.want {
				t.Fatalf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}
