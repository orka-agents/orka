/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import "testing"

func TestSafeCommandErrors(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"flags", []string{"--unknown=SYNTHETIC_PRIVATE"}, "invalid command flags"},
		{"positional", []string{"SYNTHETIC_PRIVATE"}, "invalid command flags"},
		{"url", []string{"--upstream=http://fixture\nSYNTHETIC_PRIVATE"}, "invalid upstream URL"},
		{"scheme", []string{"--upstream=file://fixture"}, "invalid upstream URL"},
		{"userinfo", []string{"--upstream=http://fixture@localhost"}, "invalid upstream URL"},
		{"listen", []string{"--listen=invalid:SYNTHETIC_PRIVATE"}, "capture server failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := run(tc.args); err == nil || err.Error() != tc.want {
				t.Fatal("command error was missing or unsafe")
			}
		})
	}
}
