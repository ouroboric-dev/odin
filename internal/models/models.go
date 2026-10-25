// Package models is the client of the shared model server (spec §3.9): it
// checks that a model is on the server, allows it, loads it and waits.
package models

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ouroboric-dev/gungnir/schema"
)

// Server is the model server as a run sees it.
type Server interface {
	// Ready returns when the model is loaded, or says why it cannot be.
	Ready(ctx context.Context, name string) error
}

type server struct {
	list     *schema.ModelList
	client   *http.Client
	interval time.Duration // between polls
	timeout  time.Duration // for the whole wait for a load
}

// New returns the client for the server the list names.
func New(list *schema.ModelList, client *http.Client) Server {
	if client == nil {
		client = http.DefaultClient
	}
	return &server{list: list, client: client, interval: time.Second, timeout: 300 * time.Second}
}

type entry struct {
	ID     string `json:"id"`
	Status struct {
		Value    string `json:"value"`
		Failed   bool   `json:"failed"`
		ExitCode *int   `json:"exit_code"`
	} `json:"status"`
}

func (s *server) Ready(ctx context.Context, name string) error {
	e, err := s.find(ctx, name)
	if err != nil {
		return err
	}
	if e.Status.Value == "loaded" {
		return nil
	}
	if err := s.allow(name); err != nil {
		return fmt.Errorf("model %q: write the allow file: %w", name, err)
	}
	if err := s.load(ctx, name); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	for {
		e, err := s.find(ctx, name)
		if err != nil {
			if ctx.Err() != nil {
				return fmt.Errorf("model %q: not loaded within %s (status wanted: loaded)", name, s.timeout)
			}
			return err
		}
		switch {
		case e.Status.Value == "loaded":
			return nil
		case e.Status.Failed:
			code := "unknown"
			if e.Status.ExitCode != nil {
				code = fmt.Sprint(*e.Status.ExitCode)
			}
			return fmt.Errorf("model %q: the load failed, exit_code %s", name, code)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("model %q: not loaded within %s (status %q, wanted loaded)", name, s.timeout, e.Status.Value)
		case <-time.After(s.interval):
		}
	}
}

// find reads the list and returns the model's entry.
func (s *server) find(ctx context.Context, name string) (entry, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.list.HostURL()+"/models", nil)
	if err != nil {
		return entry{}, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return entry{}, fmt.Errorf("model %q: the model server does not answer at %s (start it: scripts/ygg-models.sh start): %w",
			name, s.list.HostURL(), err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return entry{}, fmt.Errorf("model %q: read the model list: %w", name, err)
	}
	if resp.StatusCode != http.StatusOK {
		return entry{}, fmt.Errorf("model %q: GET /models answered %d", name, resp.StatusCode)
	}
	var out struct {
		Data []entry `json:"data"`
	}
	if err := json.Unmarshal(escapeControls(body), &out); err != nil {
		return entry{}, fmt.Errorf("model %q: the model list is not JSON: %w", name, err)
	}
	for _, e := range out.Data {
		if e.ID == name {
			return e, nil
		}
	}
	return entry{}, fmt.Errorf("model %q: the model server does not list it", name)
}

// allow writes <state>/allowed atomically.
func (s *server) allow(name string) error {
	dir := s.list.Server.State
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".allowed-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, werr := f.WriteString(name + "\n")
	cerr := f.Close()
	if werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Chmod(tmp, 0o644)
	}
	if werr == nil {
		werr = os.Rename(tmp, filepath.Join(dir, "allowed"))
	}
	if werr != nil {
		os.Remove(tmp)
	}
	return werr
}

func (s *server) load(ctx context.Context, name string) error {
	payload, _ := json.Marshal(map[string]string{"model": name})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.list.HostURL()+"/models/load", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("model %q: the load request failed (start the server: scripts/ygg-models.sh start): %w", name, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	if resp.StatusCode == http.StatusBadRequest && strings.Contains(string(body), "model is already running") {
		return nil
	}
	return fmt.Errorf("model %q: the load request answered %d: %s", name, resp.StatusCode, strings.TrimSpace(string(body)))
}

// escapeControls escapes raw control characters inside JSON strings, which
// llama-server leaves raw in status.preset and encoding/json refuses.
func escapeControls(b []byte) []byte {
	out := make([]byte, 0, len(b)+16)
	inStr, esc := false, false
	for _, c := range b {
		switch {
		case !inStr:
			if c == '"' {
				inStr = true
			}
			out = append(out, c)
		case esc:
			esc = false
			out = append(out, c)
		case c == '\\':
			esc = true
			out = append(out, c)
		case c == '"':
			inStr = false
			out = append(out, c)
		case c == '\n':
			out = append(out, `\n`...)
		case c == '\t':
			out = append(out, `\t`...)
		case c == '\r':
			out = append(out, `\r`...)
		case c < 0x20:
			out = append(out, fmt.Sprintf(`\u%04x`, c)...)
		default:
			out = append(out, c)
		}
	}
	return out
}
