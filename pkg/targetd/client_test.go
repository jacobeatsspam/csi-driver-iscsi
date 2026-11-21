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

package targetd

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func testClient(t *testing.T, fn roundTripFunc) *Client {
	t.Helper()
	c, err := New(Config{Endpoint: "https://targetd:18700", Username: "alice", Password: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	c.http.Transport = fn
	return c
}

func response(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}
}

func TestOperations(t *testing.T) {
	var calls int
	tests := []struct {
		method string
		params map[string]any
		result string
		call   func(*Client) error
	}{
		{"pool_list", map[string]any{}, `[{"name":"vg","size":100,"free_size":70,"type":"lvm"}]`, func(c *Client) error {
			p, err := c.Pools(context.Background())
			if err == nil && (len(p) != 1 || p[0].Name != "vg" || p[0].FreeSize != 70) {
				return errors.New("incorrect pool data")
			}
			return err
		}},
		{"vol_list", map[string]any{"pool": "vg"}, `[{"name":"vol","size":30,"uuid":"abc"}]`, func(c *Client) error {
			v, err := c.Volumes(context.Background(), "vg")
			if err == nil && (len(v) != 1 || v[0].UUID != "abc") {
				return errors.New("incorrect volume data")
			}
			return err
		}},
		{"vol_create", map[string]any{"pool": "vg", "name": "vol", "size": float64(30)}, `null`, func(c *Client) error { return c.CreateVolume(context.Background(), "vg", "vol", 30) }},
		{"vol_destroy", map[string]any{"pool": "vg", "name": "vol"}, `null`, func(c *Client) error { return c.DeleteVolume(context.Background(), "vg", "vol") }},
		{"export_list", map[string]any{}, `[{"pool":"vg","vol_name":"vol","vol_uuid":"abc","vol_size":30,"initiator_wwn":"iqn.test","lun":5}]`, func(c *Client) error {
			e, err := c.Exports(context.Background())
			if err == nil && (len(e) != 1 || e[0].VolumeUUID != "abc" || e[0].LUN != 5 || e[0].InitiatorWWN != "iqn.test") {
				return errors.New("incorrect export data")
			}
			return err
		}},
		{"export_create", map[string]any{"pool": "vg", "vol": "vol", "initiator_wwn": "iqn.test", "lun": float64(5)}, `null`, func(c *Client) error { return c.CreateExport(context.Background(), "vg", "vol", "iqn.test", 5) }},
		{"export_destroy", map[string]any{"pool": "vg", "vol": "vol", "initiator_wwn": "iqn.test"}, `null`, func(c *Client) error { return c.DeleteExport(context.Background(), "vg", "vol", "iqn.test") }},
	}
	c := testClient(t, func(r *http.Request) (*http.Response, error) {
		t.Helper()
		test := tests[calls]
		calls++
		user, pass, ok := r.BasicAuth()
		if !ok || user != "alice" || pass != "secret" || r.Method != http.MethodPost || r.URL.Path != "/targetrpc" || r.Header.Get("Content-Type") != "application/json" {
			t.Fatal("incorrect request authentication or endpoint")
		}
		var request struct {
			JSONRPC string         `json:"jsonrpc"`
			ID      uint64         `json:"id"`
			Method  string         `json:"method"`
			Params  map[string]any `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if request.JSONRPC != "2.0" || request.ID != uint64(calls) || request.Method != test.method || !reflect.DeepEqual(request.Params, test.params) {
			t.Fatalf("unexpected request: %+v", request)
		}
		return response(http.StatusOK, fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":%s}`, request.ID, test.result)), nil
	})
	for _, test := range tests {
		if err := test.call(c); err != nil {
			t.Fatalf("%s: %v", test.method, err)
		}
	}
}

func TestBadResponses(t *testing.T) {
	for _, body := range []string{
		`not json`,
		`{"jsonrpc":"2.0","id":2,"result":[]}`,
		`{"jsonrpc":"2.0","result":[]}`,
		`{"jsonrpc":"1.0","id":1,"result":[]}`,
		`{"jsonrpc":"2.0","id":1}`,
		`{"jsonrpc":"2.0","id":1,"result":[],"error":{"code":-1}}`,
		`{"jsonrpc":"2.0","id":1,"result":null}`,
		`{"jsonrpc":"2.0","id":1,"result":{}}`,
		strings.Repeat("x", maxResponseSize+1),
	} {
		c := testClient(t, func(*http.Request) (*http.Response, error) { return response(http.StatusOK, body), nil })
		if _, err := c.Pools(context.Background()); err == nil {
			t.Fatalf("accepted invalid response %.100q", body)
		}
	}

	c := testClient(t, func(*http.Request) (*http.Response, error) {
		return response(200, `{"jsonrpc":"2.0","id":1,"error":{"code":-123,"message":"secret"}}`), nil
	})
	_, err := c.Pools(context.Background())
	var rpcErr *Error
	if !errors.As(err, &rpcErr) || rpcErr.Code != -123 || strings.Contains(err.Error(), "secret") {
		t.Fatalf("incorrect RPC error: %v", err)
	}
	c = testClient(t, func(*http.Request) (*http.Response, error) { return response(401, "secret"), nil })
	_, err = c.Pools(context.Background())
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != 401 || strings.Contains(err.Error(), "secret") {
		t.Fatalf("incorrect HTTP error: %v", err)
	}
	c = testClient(t, func(*http.Request) (*http.Response, error) {
		return response(200, `{"jsonrpc":"2.0","id":1,"result":{"status":"queued"}}`), nil
	})
	if err := c.DeleteVolume(context.Background(), "pool", "volume"); err == nil {
		t.Fatal("accepted an unconfirmed mutation")
	}
}

func TestCancellation(t *testing.T) {
	c := testClient(t, func(r *http.Request) (*http.Response, error) { <-r.Context().Done(); return nil, r.Context().Err() })
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	if err := c.CreateVolume(ctx, "vg", "vol", 10); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline, got %v", err)
	}
	c.http.Timeout = time.Millisecond
	if err := c.CreateVolume(context.Background(), "vg", "vol", 10); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected client timeout, got %v", err)
	}
}

func TestIncompleteInventory(t *testing.T) {
	for _, row := range []string{
		`{"initiator_wwn":"iqn.test","pool":"vg","vol_name":"vol","vol_uuid":"abc"}`,
		`{"initiator_wwn":"iqn.test","pool":"vg","vol_name":"vol","vol_uuid":"abc","lun":null}`,
		`{"initiator_wwn":"iqn.test","pool":"vg","vol_name":"vol","vol_uuid":"abc","lun":256}`,
		`{"initiator_wwn":"iqn.test","vol_name":"vol","vol_uuid":"abc","lun":0}`,
	} {
		c := testClient(t, func(*http.Request) (*http.Response, error) {
			return response(200, `{"jsonrpc":"2.0","id":1,"result":[`+row+`]}`), nil
		})
		if _, err := c.Exports(context.Background()); err == nil {
			t.Fatalf("accepted malformed export %s", row)
		}
	}
	c := testClient(t, func(*http.Request) (*http.Response, error) {
		return response(200, `{"jsonrpc":"2.0","id":1,"result":[{"size":10,"uuid":"id"}]}`), nil
	})
	if _, err := c.Volumes(context.Background(), "vg"); err == nil {
		t.Fatal("accepted volume without name")
	}
}

func TestMutationCanCompleteBeforeTimeout(t *testing.T) {
	created := false
	c := testClient(t, func(r *http.Request) (*http.Response, error) {
		var request struct {
			ID     int    `json:"id"`
			Method string `json:"method"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			return nil, err
		}
		if request.Method == "vol_create" {
			created = true
			<-r.Context().Done()
			return nil, r.Context().Err()
		}
		if !created {
			return response(200, fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":[]}`, request.ID)), nil
		}
		return response(200, fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":[{"name":"vol","size":10,"uuid":"id"}]}`, request.ID)), nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	if err := c.CreateVolume(ctx, "vg", "vol", 10); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected timeout after mutation, got %v", err)
	}
	volumes, err := c.Volumes(context.Background(), "vg")
	if err != nil || len(volumes) != 1 || volumes[0].UUID != "id" {
		t.Fatalf("cannot reconcile successful mutation: %v, %v", volumes, err)
	}
}

func TestConfiguration(t *testing.T) {
	for _, cfg := range []Config{
		{Endpoint: "https://user:secret@host", Username: "alice", Password: "secret"},
		{Endpoint: "file:///targetrpc", Username: "alice", Password: "secret"},
		{Endpoint: "https://host/?secret=1", Username: "alice", Password: "secret"},
		{Endpoint: "https://host", Username: "alice", Password: "has:colon"},
		{Endpoint: "http://host", Username: "alice", Password: "secret"},
		{Endpoint: "https://host", Username: "alice", Password: "secret", CA: []byte{}},
		{Endpoint: "https://host", Username: "alice", Password: "secret", CA: []byte("invalid")},
		{Endpoint: "https://host", Username: "alice", Password: "secret", Timeout: -1},
	} {
		if _, err := New(cfg); err == nil {
			t.Fatalf("accepted invalid configuration: %+v", cfg)
		}
	}
}

func TestTLSAndRedirect(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), DNSNames: []string{"localhost"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	serverCert := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	connect := func(c *Client) {
		transport := c.http.Transport.(*http.Transport)
		transport.Proxy = nil
		transport.DisableKeepAlives = true
		transport.DialContext = func(context.Context, string, string) (net.Conn, error) {
			client, server := net.Pipe()
			go func() {
				// Unbuffered net.Pipe cannot support simultaneous TLS 1.3 handshake writes.
				conn := tls.Server(server, &tls.Config{Certificates: []tls.Certificate{serverCert}, MaxVersion: tls.VersionTLS12})
				defer server.Close()
				req, err := http.ReadRequest(bufio.NewReader(conn))
				if err != nil {
					return
				}
				_, _ = io.Copy(io.Discard, req.Body)
				_ = req.Body.Close()
				res := response(http.StatusOK, `{"jsonrpc":"2.0","id":1,"result":[]}`)
				if req.URL.Path == "/redirect" {
					res = response(http.StatusFound, "")
					res.Header.Set("Location", "/targetrpc")
				}
				_ = res.Write(conn)
			}()
			return client, nil
		}
	}
	c, err := New(Config{Endpoint: "https://localhost", Username: "alice", Password: "secret", CA: cert})
	if err != nil {
		t.Fatal(err)
	}
	connect(c)
	if _, err = c.Pools(context.Background()); err != nil {
		t.Fatalf("TLS with custom CA: %v", err)
	}
	c, err = New(Config{Endpoint: "https://localhost", Username: "alice", Password: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	connect(c)
	if _, err = c.Pools(context.Background()); err == nil {
		t.Fatal("untrusted TLS certificate accepted")
	}
	c, err = New(Config{Endpoint: "https://localhost", Username: "alice", Password: "secret", CA: cert})
	if err != nil {
		t.Fatal(err)
	}
	connect(c)
	c.endpoint = "https://localhost/redirect"
	_, err = c.Pools(context.Background())
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusFound {
		t.Fatalf("redirect followed or unexpected error: %v", err)
	}
}
