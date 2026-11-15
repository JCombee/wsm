package helper

import (
	"bytes"
	"context"
	"fmt"
	"net/netip"
	"os"
	"strings"
	"testing"
)

const testPassword = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJ0123-_x" // 43 characters

func TestServiceRequestValidate(t *testing.T) {
	if len(testPassword) != PasswordLen {
		t.Fatalf("test password has %d characters", len(testPassword))
	}
	pw := testPassword
	ops := &fakeOps{}
	srv := &Server{Ops: ops, AllowedUID: uint32(os.Getuid()), Logf: func(format string, args ...any) {
		// The helper log must never carry the password.
		if line := fmt.Sprintf(format, args...); strings.Contains(line, pw) || strings.Contains(line, pw[:42]) {
			t.Errorf("log line contains the password")
		}
	}}
	tests := []struct {
		name string
		msg  string
	}{
		// pull_service_image
		{"pull without tag", `{"type":"pull_service_image","service":"mariadb"}`},
		{"pull without service", `{"type":"pull_service_image","tag":"11.8"}`},
		{"pull with port", `{"type":"pull_service_image","service":"mariadb","tag":"11.8","port":20000}`},
		{"pull with uid", `{"type":"pull_service_image","service":"mariadb","tag":"11.8","uid":50000}`},
		{"pull with password", `{"type":"pull_service_image","service":"mariadb","tag":"11.8","password":"` + pw + `"}`},
		{"pull with image", `{"type":"pull_service_image","service":"mariadb","tag":"11.8","image":"docker.io/evil:1"}`},
		{"pull with empty password field", `{"type":"pull_service_image","service":"mariadb","tag":"11.8","password":""}`},
		{"pull service redis", `{"type":"pull_service_image","service":"redis","tag":"11.8"}`},
		{"pull service MariaDB", `{"type":"pull_service_image","service":"MariaDB","tag":"11.8"}`},
		{"pull Service field", `{"type":"pull_service_image","Service":"mariadb","tag":"11.8"}`},
		// ensure_service_instance
		{"ensure without password", `{"type":"ensure_service_instance","service":"mariadb","tag":"11.8","port":20000}`},
		{"ensure without port", `{"type":"ensure_service_instance","service":"mariadb","tag":"11.8","password":"` + pw + `"}`},
		{"ensure with uid", `{"type":"ensure_service_instance","service":"mariadb","tag":"11.8","port":20000,"uid":50000,"password":"` + pw + `"}`},
		{"ensure with unit", `{"type":"ensure_service_instance","service":"mariadb","tag":"11.8","port":20000,"password":"` + pw + `","unit":"x"}`},
		{"ensure Port", `{"type":"ensure_service_instance","service":"mariadb","tag":"11.8","Port":20000,"password":"` + pw + `"}`},
		{"ensure port 19999", `{"type":"ensure_service_instance","service":"mariadb","tag":"11.8","port":19999,"password":"` + pw + `"}`},
		{"ensure port 21000", `{"type":"ensure_service_instance","service":"mariadb","tag":"11.8","port":21000,"password":"` + pw + `"}`},
		{"ensure port 7443", `{"type":"ensure_service_instance","service":"mariadb","tag":"11.8","port":7443,"password":"` + pw + `"}`},
		{"ensure port string", `{"type":"ensure_service_instance","service":"mariadb","tag":"11.8","port":"20000","password":"` + pw + `"}`},
		{"ensure password 42", `{"type":"ensure_service_instance","service":"mariadb","tag":"11.8","port":20000,"password":"` + pw[:42] + `"}`},
		{"ensure password 44", `{"type":"ensure_service_instance","service":"mariadb","tag":"11.8","port":20000,"password":"` + pw + `a"}`},
		{"ensure password +", `{"type":"ensure_service_instance","service":"mariadb","tag":"11.8","port":20000,"password":"` + pw[:42] + `+"}`},
		{"ensure password /", `{"type":"ensure_service_instance","service":"mariadb","tag":"11.8","port":20000,"password":"` + pw[:42] + `/"}`},
		{"ensure password =", `{"type":"ensure_service_instance","service":"mariadb","tag":"11.8","port":20000,"password":"` + pw[:42] + `="}`},
		{"ensure service redis", `{"type":"ensure_service_instance","service":"redis","tag":"11.8","port":20000,"password":"` + pw + `"}`},
		// allow_service and revoke_service
		{"allow without port", `{"type":"allow_service","uid":50000}`},
		{"allow without uid", `{"type":"allow_service","port":20000}`},
		{"allow uid 49999", `{"type":"allow_service","uid":49999,"port":20000}`},
		{"allow uid 60000", `{"type":"allow_service","uid":60000,"port":20000}`},
		{"allow uid 0", `{"type":"allow_service","uid":0,"port":20000}`},
		{"allow port 19999", `{"type":"allow_service","uid":50000,"port":19999}`},
		{"allow port 21000", `{"type":"allow_service","uid":50000,"port":21000}`},
		{"allow port 7443", `{"type":"allow_service","uid":50000,"port":7443}`},
		{"allow with service", `{"type":"allow_service","uid":50000,"port":20000,"service":"mariadb"}`},
		{"allow with address", `{"type":"allow_service","uid":50000,"port":20000,"address":"10.0.0.1"}`},
		{"allow Port", `{"type":"allow_service","uid":50000,"Port":20000}`},
		{"revoke without port", `{"type":"revoke_service","uid":50000}`},
		{"revoke port 21000", `{"type":"revoke_service","uid":50000,"port":21000}`},
		{"revoke with tag", `{"type":"revoke_service","uid":50000,"port":20000,"tag":"11.8"}`},
		// The existing types take none of the new fields.
		{"create user with port", `{"type":"create_project_user","uid":50000,"port":20000}`},
		{"delete user with password", `{"type":"delete_project_user","uid":50000,"password":"` + pw + `"}`},
		{"wsm0 with service", `{"type":"setup_wsm0","service":"mariadb"}`},
		{"rules with tag", `{"type":"apply_base_rules","tag":"11.8"}`},
		{"rules with empty tag", `{"type":"apply_base_rules","tag":""}`},
	}
	for _, tag := range []string{"10.6", "10.10", "9.9", "latest", "11", "11.8.9.1", "011.8", "11.08",
		"11.8-ubi", "11.8;id", "../11.8", " 11.8", "11.8\n", ""} {
		q, _ := jsonString(tag)
		tests = append(tests,
			struct{ name, msg string }{"pull tag " + q, `{"type":"pull_service_image","service":"mariadb","tag":` + q + `}`},
			struct{ name, msg string }{"ensure tag " + q, `{"type":"ensure_service_instance","service":"mariadb","tag":` + q + `,"port":20000,"password":"` + pw + `"}`},
		)
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := DecodeRequest(strings.NewReader(tt.msg)); err == nil {
				t.Fatalf("DecodeRequest accepted %s", tt.msg)
			} else if strings.Contains(err.Error(), pw) {
				t.Errorf("error contains the password: %v", err)
			}
			before := len(ops.Calls())
			resp := rawExchange(t, srv, []byte(tt.msg))
			if !strings.Contains(resp, `"ok":false`) {
				t.Errorf("response %q, want ok false", resp)
			}
			if strings.Contains(resp, pw) || strings.Contains(resp, pw[:42]) {
				t.Errorf("response contains the password")
			}
			if after := ops.Calls(); len(after) != before {
				t.Errorf("operation ran: %v", after[before:])
			}
		})
	}

	// Controls: valid requests reach the operations, through the client.
	uid, port := 50001, 20000
	for _, req := range []Request{
		{Type: TypePullServiceImage, Service: "mariadb", Tag: "11.8"},
		{Type: TypeEnsureServiceInstance, Service: "mariadb", Tag: "12.3", Port: &port, Password: pw},
		{Type: TypeAllowService, UID: &uid, Port: &port},
		{Type: TypeRevokeService, UID: &uid, Port: &port},
	} {
		if err := clientDo(t, srv, req); err != nil {
			t.Fatalf("valid %s: %v", req.Type, err)
		}
	}
	want := []string{"pull mariadb 11.8", "ensure mariadb 12.3 20000", "allow 50001 20000", "revoke 50001 20000"}
	if got := ops.Calls(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("calls = %v, want %v", got, want)
	}
	if d := describe(Request{Type: TypeEnsureServiceInstance, Service: "mariadb", Tag: "12.3", Port: &port, Password: pw}); strings.Contains(d, pw) || d != "service=mariadb tag=12.3 port=20000" {
		t.Errorf("describe = %q", d)
	}
	if (&Server{}).timeout(TypeEnsureServiceInstance) <= (&Server{}).timeout(TypeAllowService) {
		t.Error("ensure does not get the long timeout")
	}
}

func jsonString(s string) (string, error) {
	var b bytes.Buffer
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r < 0x20:
			b.WriteString(`\u00`)
			b.WriteByte("0123456789abcdef"[r>>4])
			b.WriteByte("0123456789abcdef"[r&0xf])
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String(), nil
}

func TestServiceUnit(t *testing.T) {
	wsm0 := netip.MustParseAddr("10.255.0.1")
	got, err := ServiceUnit(wsm0, "mariadb", "11.8", 20000)
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile("testdata/wsm-mariadb-11-8.container")
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("unit differs from the golden file:\n%s", got)
	}
	if strings.Contains(strings.ToLower(got), "password=") {
		t.Error("unit sets a password")
	}
	n := NamesFor("mariadb", "11.8")
	if n != (ServiceNames{Base: "wsm-mariadb-11-8", File: "wsm-mariadb-11-8.container",
		Unit: "wsm-mariadb-11-8.service", Container: "wsm-mariadb-11-8",
		Secret: "wsm-mariadb-11-8-root", Volume: "wsm-mariadb-11-8-data"}) {
		t.Errorf("NamesFor = %+v", n)
	}
	for _, s := range []string{"ContainerName=" + n.Container, "Secret=" + n.Secret + ",", "Volume=" + n.Volume + ":"} {
		if !strings.Contains(got, s) {
			t.Errorf("unit lacks %q", s)
		}
	}
	if got := NamesFor("mariadb", "11.8.9").Unit; got != "wsm-mariadb-11-8-9.service" {
		t.Errorf("unit for 11.8.9 = %q", got)
	}
	if got := ServiceUnitDir(999); got != "/etc/containers/systemd/users/999" {
		t.Errorf("ServiceUnitDir = %q", got)
	}
	for _, tt := range []struct {
		service, tag string
		port         int
		addr         string
	}{
		{"redis", "11.8", 20000, "10.255.0.1"},
		{"mariadb", "latest", 20000, "10.255.0.1"},
		{"mariadb", "11.8", 19999, "10.255.0.1"},
		{"mariadb", "11.8", 21000, "10.255.0.1"},
		{"mariadb", "11.8", 20000, "::1"},
	} {
		if _, err := ServiceUnit(netip.MustParseAddr(tt.addr), tt.service, tt.tag, tt.port); err == nil {
			t.Errorf("ServiceUnit(%+v) succeeded", tt)
		}
	}
	// The system operations validate again, before they touch anything.
	o := &SystemOps{WSM0Addr: wsm0}
	if err := o.EnsureServiceInstance(context.Background(), "mariadb", "11.8", 20000, "short"); err == nil ||
		strings.Contains(err.Error(), "short") {
		t.Errorf("EnsureServiceInstance with a bad password: %v", err)
	}
	if err := o.PullServiceImage(context.Background(), "mariadb", "latest"); err == nil {
		t.Error("PullServiceImage with tag latest succeeded")
	}
}
