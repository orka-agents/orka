/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package api

import (
	"testing"
)

func TestParsePagination(t *testing.T) {
	tests := []struct {
		name          string
		limitStr      string
		continueToken string
		wantLimit     int64
		wantContinue  string
		wantErr       bool
	}{
		{
			name:          "default values",
			limitStr:      "",
			continueToken: "",
			wantLimit:     DefaultLimit,
			wantContinue:  "",
			wantErr:       false,
		},
		{
			name:          "valid limit",
			limitStr:      "50",
			continueToken: "",
			wantLimit:     50,
			wantContinue:  "",
			wantErr:       false,
		},
		{
			name:          "valid limit with continue token",
			limitStr:      "25",
			continueToken: "abc123",
			wantLimit:     25,
			wantContinue:  "abc123",
			wantErr:       false,
		},
		{
			name:          "limit exceeds max",
			limitStr:      "1000",
			continueToken: "",
			wantLimit:     MaxLimit,
			wantContinue:  "",
			wantErr:       false,
		},
		{
			name:          "limit equals max",
			limitStr:      "500",
			continueToken: "",
			wantLimit:     MaxLimit,
			wantContinue:  "",
			wantErr:       false,
		},
		{
			name:          "minimum valid limit",
			limitStr:      "1",
			continueToken: "",
			wantLimit:     1,
			wantContinue:  "",
			wantErr:       false,
		},
		{
			name:          "invalid limit - not a number",
			limitStr:      "abc",
			continueToken: "",
			wantErr:       true,
		},
		{
			name:          "invalid limit - negative",
			limitStr:      "-1",
			continueToken: "",
			wantErr:       true,
		},
		{
			name:          "invalid limit - zero",
			limitStr:      "0",
			continueToken: "",
			wantErr:       true,
		},
		{
			name:          "invalid limit - float",
			limitStr:      "10.5",
			continueToken: "",
			wantErr:       true,
		},
		{
			name:          "empty limit with continue token",
			limitStr:      "",
			continueToken: "token123",
			wantLimit:     DefaultLimit,
			wantContinue:  "token123",
			wantErr:       false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := ParsePagination(tt.limitStr, tt.continueToken)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParsePagination() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.wantErr {
				return
			}
			if p.Limit != tt.wantLimit {
				t.Errorf("ParsePagination() Limit = %v, want %v", p.Limit, tt.wantLimit)
			}
			if p.Continue != tt.wantContinue {
				t.Errorf("ParsePagination() Continue = %v, want %v", p.Continue, tt.wantContinue)
			}
		})
	}
}

func TestParsePagination_Constants(t *testing.T) {
	// Verify constants are set correctly
	if DefaultLimit != 100 {
		t.Errorf("DefaultLimit = %d, want 100", DefaultLimit)
	}
	if MaxLimit != 500 {
		t.Errorf("MaxLimit = %d, want 500", MaxLimit)
	}
}
