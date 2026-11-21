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

// targetd-fixture-check checks the appliance directly inside a disposable VM.
package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/kubernetes-csi/csi-driver-iscsi/pkg/targetd"
)

const (
	pool      = "vg-targetd-e2e"
	volume    = "csi_e2e_volume"
	target    = "iqn.2026-01.test.csi:targetd"
	initiator = "iqn.2026-01.test.csi:initiator"
)

type rpcClient struct {
	url  string
	http *http.Client
	id   int
}

func (c *rpcClient) ready() error {
	var err error
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(time.Second) {
		err = c.call("pool_list", nil, nil)
		if err == nil {
			return nil
		}
	}
	return fmt.Errorf("targetd did not become ready: %w", err)
}

func (c *rpcClient) call(method string, params map[string]any, out any) error {
	c.id++
	data, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": c.id, "method": method, "params": params})
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, c.url, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.SetBasicAuth("e2e", "fixture-only")
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s returned HTTP %d", method, resp.StatusCode)
	}
	var reply struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      int             `json:"id"`
		Result  json.RawMessage `json:"result"`
		Error   *struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&reply); err != nil {
		return err
	}
	if reply.JSONRPC != "2.0" || reply.ID != c.id {
		return errors.New("invalid JSON-RPC response identity")
	}
	if reply.Error != nil {
		return fmt.Errorf("%s: %w", method, &targetd.Error{Code: reply.Error.Code})
	}
	if out != nil {
		return json.Unmarshal(reply.Result, out)
	}
	return nil
}

func command(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

func checkWrite(f *os.File, data []byte, offset int64) error {
	if _, err := f.WriteAt(data, offset); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	backing, err := os.Open(filepath.Join("/dev", pool, volume))
	if err != nil {
		return err
	}
	actual := make([]byte, len(data))
	_, readErr := backing.ReadAt(actual, offset)
	closeErr := backing.Close()
	if readErr != nil || closeErr != nil || !bytes.Equal(actual, data) {
		return errors.New("iSCSI write did not reach the backing LV")
	}
	return nil
}

func run() error {
	if os.Getenv("TARGETD_E2E_DISPOSABLE_VM") != "1" {
		return errors.New("requires the disposable targetd test VM")
	}
	c := rpcClient{url: "http://127.0.0.1:18700/targetrpc", http: &http.Client{Timeout: 15 * time.Second}}
	if os.Getenv("TARGETD_E2E_TLS") == "1" {
		c.url = "https://127.0.0.1:18700/targetrpc"
	}
	var pools []struct {
		Name string `json:"name"`
		Free int64  `json:"free_size"`
		Type string `json:"type"`
	}
	if err := c.ready(); err != nil {
		return err
	}
	if err := c.call("pool_list", nil, &pools); err != nil {
		return err
	}
	fmt.Printf("fixture targetd pools: %+v\n", pools)
	if len(pools) != 1 || pools[0].Name != pool || pools[0].Type != "block" || pools[0].Free < 128<<20 {
		return errors.New("unexpected test pool inventory")
	}
	var vols []struct {
		Name, UUID string
		Size       int64
	}
	if err := c.call("vol_list", map[string]any{"pool": pool}, &vols); err != nil {
		return err
	}
	if len(vols) != 0 {
		return errors.New("test pool is not empty")
	}
	if err := c.call("vol_create", map[string]any{"pool": pool, "name": volume, "size": 64 << 20}, nil); err != nil {
		return err
	}
	if err := c.call("vol_list", map[string]any{"pool": pool}, &vols); err != nil {
		return err
	}
	if len(vols) != 1 || vols[0].Name != volume || vols[0].UUID == "" || vols[0].Size < 64<<20 {
		return errors.New("created LV inventory is incorrect")
	}
	if out, err := exec.Command("lvs", "--segments", "--noheadings", "-o", "segtype", pool+"/"+volume).Output(); err != nil || strings.TrimSpace(string(out)) != "linear" {
		return errors.New("created volume is not a linear LV")
	}
	if err := c.call("export_create", map[string]any{"pool": pool, "vol": volume, "initiator_wwn": initiator, "lun": 0}, nil); err != nil {
		return err
	}
	var exports []struct {
		Pool      string `json:"pool"`
		Name      string `json:"vol_name"`
		UUID      string `json:"vol_uuid"`
		Initiator string `json:"initiator_wwn"`
		LUN       int    `json:"lun"`
	}
	if err := c.call("export_list", nil, &exports); err != nil {
		return err
	}
	if len(exports) != 1 || exports[0].Pool != pool || exports[0].Name != volume || exports[0].UUID != vols[0].UUID || exports[0].Initiator != initiator || exports[0].LUN != 0 {
		return errors.New("export inventory is incorrect")
	}
	originalExport := exports[0]
	var refusal *targetd.Error
	if err := c.call("vol_destroy", map[string]any{"pool": pool, "name": volume}, nil); !errors.As(err, &refusal) || refusal.Code != -303 {
		return fmt.Errorf("expected targetd VOLUME_MASKED (-303) for exported volume, got %v", err)
	}
	if err := command("iscsiadm", "-m", "discovery", "-t", "sendtargets", "-p", "127.0.0.1:3260"); err != nil {
		return err
	}
	if err := command("iscsiadm", "-m", "node", "-T", target, "-p", "127.0.0.1:3260", "--login"); err != nil {
		return err
	}
	path := filepath.Join("/dev/disk/by-path", "ip-127.0.0.1:3260-iscsi-"+target+"-lun-0")
	var f *os.File
	var err error
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(time.Second) {
		f, err = os.OpenFile(path, os.O_RDWR, 0)
		if err == nil {
			break
		}
	}
	if err != nil {
		return fmt.Errorf("iSCSI device did not appear: %w", err)
	}
	defer f.Close()
	fmt.Printf("fixture targetd volume UUID: %s\n", vols[0].UUID)
	serial, err := exec.Command("lsblk", "--noheadings", "--output", "SERIAL", path).Output()
	if err != nil || strings.TrimSpace(string(serial)) != vols[0].UUID {
		return fmt.Errorf("iSCSI serial does not match targetd volume UUID: %q: %v", strings.TrimSpace(string(serial)), err)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	page, err := os.ReadFile(filepath.Join("/sys/class/block", filepath.Base(resolved), "device/vpd_pg80"))
	if err != nil || len(page) < 4 || page[1] != 0x80 || int(binary.BigEndian.Uint16(page[2:4])) != len(page)-4 || strings.TrimRight(string(page[4:]), "\x00 \t\r\n") != vols[0].UUID {
		return fmt.Errorf("iSCSI VPD serial page does not match targetd volume UUID: %x: %v", page, err)
	}
	fmt.Printf("fixture iSCSI VPD page 0x80: %x\n", page)
	if err := command("lsblk", "-o", "NAME,WWN,SERIAL", path); err != nil {
		return err
	}
	data := []byte("targetd-e2e-persistent-data")
	if err := checkWrite(f, data, 4096); err != nil {
		return err
	}
	if os.Getenv("TARGETD_E2E_CONTAINER") == "1" {
		err = command("docker", "restart", "targetd-e2e")
	} else {
		err = command("systemctl", "restart", "targetd")
	}
	if err != nil {
		return err
	}
	if err = c.ready(); err != nil {
		return err
	}
	if err = c.call("export_list", nil, &exports); err != nil {
		return err
	}
	if len(exports) != 1 || exports[0] != originalExport {
		return errors.New("export did not survive targetd restart")
	}
	read := make([]byte, len(data))
	if _, err = f.ReadAt(read, 4096); err != nil || !bytes.Equal(read, data) {
		return errors.New("iSCSI data did not survive targetd restart")
	}
	if err := checkWrite(f, []byte("targetd-after-restart"), 8192); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = command("iscsiadm", "-m", "node", "-T", target, "-p", "127.0.0.1:3260", "--logout"); err != nil {
		return err
	}
	if err = c.call("export_destroy", map[string]any{"pool": pool, "vol": volume, "initiator_wwn": initiator}, nil); err != nil {
		return err
	}
	if err = c.call("vol_destroy", map[string]any{"pool": pool, "name": volume}, nil); err != nil {
		return err
	}
	if err = c.call("vol_list", map[string]any{"pool": pool}, &vols); err != nil {
		return err
	}
	if len(vols) != 0 {
		return errors.New("test volume was not deleted")
	}
	if err = c.call("export_list", nil, &exports); err != nil {
		return err
	}
	if len(exports) != 0 {
		return errors.New("test export was not deleted")
	}
	return nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "targetd fixture:", err)
		os.Exit(1)
	}
	fmt.Println("targetd fixture: LV, export, iSCSI data, service restart and cleanup passed")
}
