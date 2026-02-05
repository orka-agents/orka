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
	"fmt"
	"strconv"
)

const (
	// DefaultLimit is the default number of items per page
	DefaultLimit = 100

	// MaxLimit is the maximum number of items per page
	MaxLimit = 500
)

// Pagination holds pagination parameters
type Pagination struct {
	Limit    int64
	Continue string
}

// ParsePagination parses pagination parameters from query strings
func ParsePagination(limitStr, continueToken string) (*Pagination, error) {
	p := &Pagination{
		Limit:    DefaultLimit,
		Continue: continueToken,
	}

	if limitStr != "" {
		limit, err := strconv.ParseInt(limitStr, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid limit parameter: %w", err)
		}
		if limit < 1 {
			return nil, fmt.Errorf("limit must be at least 1")
		}
		if limit > MaxLimit {
			limit = MaxLimit
		}
		p.Limit = limit
	}

	return p, nil
}
