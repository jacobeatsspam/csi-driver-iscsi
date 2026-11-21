/*
Copyright 2026 The Kubernetes Authors.

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

package main

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestRPC(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		wantError  bool
	}{
		{"valid", `{"jsonrpc":"2.0","id":1,"result":[{"name":"pool"}]}`, 200, false},
		{"wrong id", `{"jsonrpc":"2.0","id":2,"result":[]}`, 200, true},
		{"backend error", `{"jsonrpc":"2.0","id":1,"error":{"code":-1,"message":"bad"}}`, 200, true},
		{"HTTP error", ``, 401, true},
		{"malformed", `{`, 200, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				user, pass, ok := r.BasicAuth()
				if r.Method != http.MethodPost || user != "e2e" || pass != "fixture-only" || !ok {
					t.Error("invalid request authentication")
				}
				return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.body)), Header: make(http.Header)}, nil
			})}
			c := rpcClient{url: "http://test/targetrpc", http: client}
			var result []map[string]string
			err := c.call("pool_list", nil, &result)
			if (err != nil) != tc.wantError {
				t.Fatalf("call error: %v", err)
			}
			if !tc.wantError && (len(result) != 1 || result[0]["name"] != "pool") {
				t.Fatalf("bad result: %v", result)
			}
		})
	}
}

func TestRequiresDisposableVM(t *testing.T) {
	t.Setenv("TARGETD_E2E_DISPOSABLE_VM", "")
	if err := run(); err == nil {
		t.Fatal("expected refusal outside the test VM")
	}
}
