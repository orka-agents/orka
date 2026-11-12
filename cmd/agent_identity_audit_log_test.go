package main

import (
	"bytes"
	"flag"
	"strings"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

func TestAgentIdentityAuditLoggerIgnoresTheDiagnosticLevel(t *testing.T) {
	for _, args := range [][]string{
		{"--zap-log-level=error"},
		{"--zap-log-level=error", "--zap-devel=false"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var diagnostic, audit bytes.Buffer
			opts := zap.Options{Development: true}
			flags := flag.NewFlagSet("controller", flag.ContinueOnError)
			opts.BindFlags(flags)
			if err := flags.Parse(args); err != nil {
				t.Fatal(err)
			}
			auditOpts := opts
			auditOpts.DestWriter = &audit
			auditLog := agentIdentityAuditLogger(auditOpts)
			opts.DestWriter = &diagnostic
			diagnosticLog := zap.New(zap.UseFlagOptions(&opts))

			diagnosticLog.Info("diagnostic record")
			for range 200 { // past the production encoder's sampling budget
				auditLog.Info("agent identity call issued")
			}
			if diagnostic.Len() != 0 {
				t.Fatalf("the diagnostic level should drop info records: %s", diagnostic.String())
			}
			if got := strings.Count(audit.String(), "agent identity call issued"); got != 200 {
				t.Fatalf("audit records = %d, want every one of 200", got)
			}
		})
	}
}
