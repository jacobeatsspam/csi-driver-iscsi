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

// Package targetd implements the targetd management API.
package targetd

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
)

const maxResponseSize = 1 << 20

type Config struct {
	Endpoint string
	Username string
	Password string
	CA       []byte
	Timeout  time.Duration
}

type Pool struct {
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	FreeSize int64  `json:"free_size"`
	Type     string `json:"type"`
	UUID     string `json:"uuid"`
}

type Volume struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
	UUID string `json:"uuid"`
}

type Export struct {
	InitiatorWWN string `json:"initiator_wwn"`
	LUN          int    `json:"lun"`
	VolumeName   string `json:"vol_name"`
	VolumeSize   int64  `json:"vol_size"`
	VolumeUUID   string `json:"vol_uuid"`
	Pool         string `json:"pool"`
}

func (e *Export) UnmarshalJSON(data []byte) error {
	type fields Export
	// A missing or null LUN must be distinguishable from a valid LUN 0.
	row := fields{LUN: -1}
	if err := json.Unmarshal(data, &row); err != nil {
		return err
	}
	if row.LUN < 0 || row.LUN > 255 || row.InitiatorWWN == "" || row.Pool == "" || row.VolumeName == "" || row.VolumeUUID == "" {
		return errors.New("invalid targetd export")
	}
	*e = Export(row)
	return nil
}

// Error reports a targetd JSON-RPC error code without exposing server text.
type Error struct{ Code int }

func (e *Error) Error() string { return fmt.Sprintf("targetd RPC error %d", e.Code) }

// HTTPError reports an HTTP error without exposing the response body.
type HTTPError struct{ StatusCode int }

func (e *HTTPError) Error() string { return fmt.Sprintf("targetd HTTP status %d", e.StatusCode) }

type Client struct {
	endpoint, username, password string
	http                         *http.Client
	id                           atomic.Uint64
}

func New(cfg Config) (*Client, error) {
	u, err := url.Parse(cfg.Endpoint)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/" && u.Path != "/targetrpc") {
		return nil, errors.New("targetd endpoint must be an HTTPS URL without credentials, query or fragment")
	}
	if cfg.Username == "" || cfg.Password == "" || strings.ContainsAny(cfg.Username+cfg.Password, ":\r\n") {
		return nil, errors.New("targetd credentials must be nonempty and cannot contain a colon or newline")
	}
	if cfg.Timeout < 0 {
		return nil, errors.New("targetd timeout must not be negative")
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 30 * time.Second
	}
	u.Path = "/targetrpc"
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	if cfg.CA != nil {
		roots, err := x509.SystemCertPool()
		if err != nil {
			return nil, fmt.Errorf("load system CA certificates: %w", err)
		}
		if !roots.AppendCertsFromPEM(cfg.CA) {
			return nil, errors.New("targetd CA contains no valid certificate")
		}
		transport.TLSClientConfig.RootCAs = roots
	}
	return &Client{
		endpoint: u.String(), username: cfg.Username, password: cfg.Password,
		http: &http.Client{Transport: transport, Timeout: cfg.Timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
	}, nil
}

func (c *Client) Pools(ctx context.Context) ([]Pool, error) {
	var pools []Pool
	err := c.call(ctx, "pool_list", nil, &pools)
	return pools, err
}

func (c *Client) Volumes(ctx context.Context, pool string) ([]Volume, error) {
	var volumes []Volume
	if err := c.call(ctx, "vol_list", map[string]any{"pool": pool}, &volumes); err != nil {
		return nil, err
	}
	for _, v := range volumes {
		if v.Name == "" || v.UUID == "" || v.Size <= 0 {
			return nil, errors.New("invalid targetd volume")
		}
	}
	return volumes, nil
}

func (c *Client) CreateVolume(ctx context.Context, pool, name string, size int64) error {
	return c.call(ctx, "vol_create", map[string]any{"pool": pool, "name": name, "size": size}, nil)
}

func (c *Client) DeleteVolume(ctx context.Context, pool, name string) error {
	return c.call(ctx, "vol_destroy", map[string]any{"pool": pool, "name": name}, nil)
}

func (c *Client) Exports(ctx context.Context) ([]Export, error) {
	var exports []Export
	err := c.call(ctx, "export_list", nil, &exports)
	return exports, err
}

func (c *Client) CreateExport(ctx context.Context, pool, volume, initiator string, lun int) error {
	return c.call(ctx, "export_create", map[string]any{"pool": pool, "vol": volume, "initiator_wwn": initiator, "lun": lun}, nil)
}

func (c *Client) DeleteExport(ctx context.Context, pool, volume, initiator string) error {
	return c.call(ctx, "export_destroy", map[string]any{"pool": pool, "vol": volume, "initiator_wwn": initiator}, nil)
}

func (c *Client) call(ctx context.Context, method string, params map[string]any, out any) error {
	id := c.id.Add(1)
	if params == nil {
		params = map[string]any{}
	}
	data, err := json.Marshal(struct {
		JSONRPC string         `json:"jsonrpc"`
		ID      uint64         `json:"id"`
		Method  string         `json:"method"`
		Params  map[string]any `json:"params"`
	}{"2.0", id, method, params})
	if err != nil {
		return fmt.Errorf("encode targetd request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("create targetd request: %w", err)
	}
	req.SetBasicAuth(c.username, c.password)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("targetd request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return &HTTPError{StatusCode: resp.StatusCode}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize+1))
	if err != nil {
		return fmt.Errorf("read targetd response: %w", err)
	}
	if len(body) > maxResponseSize {
		return errors.New("targetd response exceeds size limit")
	}
	var reply struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      *uint64         `json:"id"`
		Result  json.RawMessage `json:"result"`
		Error   *struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &reply); err != nil {
		return errors.New("invalid targetd JSON-RPC response")
	}
	if reply.JSONRPC != "2.0" || reply.ID == nil || *reply.ID != id || (reply.Result == nil) == (reply.Error == nil) {
		return errors.New("invalid targetd JSON-RPC response identity or result")
	}
	if reply.Error != nil {
		return &Error{Code: reply.Error.Code}
	}
	if out == nil {
		if !bytes.Equal(reply.Result, []byte("null")) {
			return errors.New("unexpected targetd mutation result")
		}
		return nil
	}
	if bytes.Equal(reply.Result, []byte("null")) || json.Unmarshal(reply.Result, out) != nil {
		return errors.New("invalid targetd response result")
	}
	return nil
}
