package pipeproxy_test

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/wslkit/skrog/internal/pipeproxy"
	"github.com/wslkit/skrog/internal/policy"
)

// postVolumeCreate drives one /volumes/create through the real bridge and
// reports both what the client saw and whether the engine was ever reached.
func postVolumeCreate(t *testing.T, gate pipeproxy.Gate, body string) (*http.Response, bool) {
	t.Helper()
	client, bridgeClient := net.Pipe()
	engineSide, bridgeEngine := net.Pipe()
	t.Cleanup(func() { client.Close(); engineSide.Close() })

	go pipeproxy.RewriteBindsGuarded(nil, gate)(bridgeClient, bridgeEngine)

	reached := make(chan struct{}, 1)
	go func() {
		br := bufio.NewReader(engineSide)
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			if line == "\r\n" {
				break
			}
		}
		reached <- struct{}{}
		// Answer FIRST. Draining before replying would block on this
		// unbuffered pipe until EOF, and the reply would never be written --
		// which reads as "the bridge swallowed it" rather than as a bug here.
		engineSide.Write([]byte("HTTP/1.1 201 Created\r\nContent-Length: 0\r\n\r\n"))
		io.Copy(io.Discard, engineSide)
	}()

	req := "POST /v1.45/volumes/create HTTP/1.1\r\nHost: d\r\nContent-Type: application/json\r\n" +
		"Content-Length: " + strconv.Itoa(len(body)) + "\r\n\r\n" + body
	go client.Write([]byte(req))

	client.SetReadDeadline(time.Now().Add(10 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(client), nil)
	if err != nil {
		t.Fatalf("reading response: %v", err)
	}
	select {
	case <-reached:
		return resp, true
	default:
		return resp, false
	}
}

// End to end through the bridge with the REAL policy gate (#419).
//
// The policy-level tests are not enough on their own, and this release is the
// reason: deny-unattributable-builds passed every Rules test while being a
// no-op in the product, because nothing drove the path the bridge takes.
func TestVolumeCreateIsJudgedByTheBridge(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(policy.MachineDirEnv, t.TempDir())
	rules := "allow-bind-sources:\n  - C:\\work\n"
	if err := os.WriteFile(filepath.Join(dir, "policy.yaml"), []byte(rules), 0o644); err != nil {
		t.Fatal(err)
	}
	gate := policy.NewWatcher(dir)

	t.Run("a device outside the allowlist is refused at the bridge", func(t *testing.T) {
		resp, reached := postVolumeCreate(t, gate,
			`{"Name":"esc","Driver":"local","DriverOpts":{"type":"none","o":"bind","device":"/mnt/c/secrets"}}`)
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("status = %d, want 403", resp.StatusCode)
		}
		if reached {
			t.Error("the request reached the engine; a denial must stop at the bridge")
		}
	})

	t.Run("an ordinary named volume passes through", func(t *testing.T) {
		resp, reached := postVolumeCreate(t, gate, `{"Name":"data"}`)
		if resp.StatusCode != http.StatusCreated {
			t.Errorf("status = %d, want 201", resp.StatusCode)
		}
		if !reached {
			t.Error("a permitted volume never reached the engine")
		}
	})

	t.Run("a device inside the allowlist passes through", func(t *testing.T) {
		resp, reached := postVolumeCreate(t, gate,
			`{"Name":"ok","Driver":"local","DriverOpts":{"type":"none","o":"bind","device":"/mnt/c/work/proj"}}`)
		if resp.StatusCode != http.StatusCreated {
			t.Errorf("status = %d, want 201", resp.StatusCode)
		}
		if !reached {
			t.Error("a permitted volume never reached the engine")
		}
	})
}
