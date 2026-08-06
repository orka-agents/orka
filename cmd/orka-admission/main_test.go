package main

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestOptionsValidate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		mutate  func(*options)
		wantErr bool
	}{
		{name: "valid"},
		{name: "certificate directory required", mutate: func(o *options) { o.webhookCertPath = "" }, wantErr: true},
		{name: "controller identity required", mutate: func(o *options) { o.controllerUsername = "" }, wantErr: true},
		{name: "admin group required", mutate: func(o *options) { o.adminGroups = " , " }, wantErr: true},
		{name: "certificate filename only", mutate: func(o *options) { o.webhookCertName = "../tls.crt" }, wantErr: true},
		{name: "key filename only", mutate: func(o *options) { o.webhookCertKey = "/tls.key" }, wantErr: true},
		{name: "port lower bound", mutate: func(o *options) { o.webhookPort = 0 }, wantErr: true},
		{name: "port upper bound", mutate: func(o *options) { o.webhookPort = 65536 }, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			opts := options{
				webhookCertPath:    "/certs",
				webhookCertName:    "tls.crt",
				webhookCertKey:     "tls.key",
				webhookPort:        9443,
				controllerUsername: "system:serviceaccount:orka-system:orka-controller-manager",
				adminGroups:        "system:masters",
			}
			if tt.mutate != nil {
				tt.mutate(&opts)
			}
			if err := opts.validate(); (err != nil) != tt.wantErr {
				t.Fatalf("validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestServingCertificateFilesChecker(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	checker := servingCertificateFilesChecker(directory, "tls.crt", "tls.key")
	request := httptest.NewRequest("GET", "/readyz", nil)

	if err := checker(request); err == nil {
		t.Fatal("certificate readiness succeeded without certificate files")
	}
	if err := os.WriteFile(filepath.Join(directory, "tls.crt"), []byte("certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := checker(request); err == nil {
		t.Fatal("certificate readiness succeeded without the private key")
	}
	if err := os.WriteFile(filepath.Join(directory, "tls.key"), []byte("private-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := checker(request); err != nil {
		t.Fatalf("certificate readiness failed with both nonempty files: %v", err)
	}
	if err := os.WriteFile(filepath.Join(directory, "tls.crt"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := checker(request); err == nil {
		t.Fatal("certificate readiness succeeded with an empty certificate file")
	}
}

func TestSplitCommaList(t *testing.T) {
	t.Parallel()
	got := splitCommaList(" one, ,two , three")
	want := []string{"one", "two", "three"}
	if len(got) != len(want) {
		t.Fatalf("splitCommaList() = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("splitCommaList() = %#v, want %#v", got, want)
		}
	}
}
