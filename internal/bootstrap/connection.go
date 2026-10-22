// Package bootstrap uses OpenCode's supported CLI and pairing HTTP interfaces.
// It never reads the harness's private service registry or authentication cache.
package bootstrap

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type Connection struct {
	URL      string `json:"url"`
	Password string `json:"password"`
	Version  string `json:"version"`
}

func privateWrite(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if _, err = f.Write(data); err != nil {
		return err
	}
	return f.Sync()
}

func Load(directory string) (Connection, error) {
	path := filepath.Join(directory, "opencode-connection.json")
	st, err := os.Lstat(path)
	if err != nil {
		return Connection{}, err
	}
	if !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 {
		return Connection{}, errors.New("OpenCode connection configuration must be a regular owner-only file")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return Connection{}, err
	}
	var c Connection
	if err = json.Unmarshal(b, &c); err != nil {
		return c, errors.New("invalid OpenCode connection configuration")
	}
	return c, nil
}

func command(ctx context.Context, binary string, args ...string) ([]byte, error) {
	c := exec.CommandContext(ctx, binary, args...)
	// Command failures are intentionally sanitized: lifecycle diagnostics may
	// contain sensitive endpoint/control information supplied by the harness.
	b, err := c.Output()
	if err != nil {
		return nil, fmt.Errorf("OpenCode %s failed; inspect its supported CLI diagnostics", args[0])
	}
	if len(b) > 1<<20 {
		return nil, errors.New("OpenCode response exceeds bootstrap budget")
	}
	return b, nil
}

func loopback(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" {
		return errors.New("pairing requires a loopback HTTP server root")
	}
	ip := net.ParseIP(u.Hostname())
	if ip == nil || !ip.IsLoopback() {
		return errors.New("automatic pairing is restricted to an explicit loopback IP")
	}
	return nil
}

// Pair preflights an already-running local service and issues its supported
// one-time pairing grant. It never requests a restart or upgrade. OpenCode's
// native api command retains its own discovery/autostart semantics if the
// service disappears between that preflight and the command.
func Pair(ctx context.Context, binary, directory string) (Connection, error) {
	if err := os.MkdirAll(directory, 0700); err != nil {
		return Connection{}, err
	}
	if _, err := os.Lstat(filepath.Join(directory, "opencode-connection.json")); err == nil {
		return Connection{}, errors.New("connection already configured; use another instance directory for a different runtime")
	} else if !errors.Is(err, os.ErrNotExist) {
		return Connection{}, err
	}
	status, err := command(ctx, binary, "service", "status")
	if err != nil {
		return Connection{}, err
	}
	endpoint := strings.TrimSpace(string(status))
	if err = loopback(endpoint); err != nil {
		return Connection{}, err
	}
	info, err := command(ctx, binary, "api", "get", "/api/info")
	if err != nil {
		return Connection{}, err
	}
	var server struct {
		Version string   `json:"version"`
		URLs    []string `json:"urls"`
	}
	if err = json.Unmarshal(info, &server); err != nil || server.Version != "2.1.0" {
		return Connection{}, errors.New("pairing requires verified OpenCode 2.1.0")
	}
	found := false
	for _, u := range server.URLs {
		if u == endpoint {
			found = true
		}
	}
	if !found {
		return Connection{}, errors.New("service identity changed during pairing preflight")
	}
	b, err := command(ctx, binary, "api", "post", "/api/pair")
	if err != nil {
		return Connection{}, err
	}
	var grant struct {
		Code string `json:"code"`
	}
	if json.Unmarshal(b, &grant) != nil || grant.Code == "" {
		return Connection{}, errors.New("invalid pairing grant")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/auth/connect/"+url.PathEscape(grant.Code), nil)
	if err != nil {
		return Connection{}, err
	}
	request.Header.Set("Accept", "application/json")
	client := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		return Connection{}, errors.New("pairing exchange failed")
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return Connection{}, errors.New("pairing grant was not accepted")
	}
	var token struct {
		Token string `json:"token"`
	}
	if err = json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&token); err != nil || token.Token == "" {
		return Connection{}, errors.New("invalid pairing token response")
	}
	c := Connection{URL: endpoint, Password: token.Token, Version: server.Version}
	data, err := json.Marshal(c)
	if err != nil {
		return Connection{}, err
	}
	if err = privateWrite(filepath.Join(directory, "opencode-connection.json"), data); err != nil {
		return Connection{}, err
	}
	return c, nil
}

// ClientToken returns only Tandem-owned configuration, not a harness credential.
func ClientToken(directory string) (string, error) {
	path := filepath.Join(directory, "client.token")
	if st, err := os.Lstat(path); err == nil {
		if !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 {
			return "", errors.New("client.token must be a regular owner-only file")
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		token := strings.TrimSpace(string(b))
		if len(token) < 32 {
			return "", errors.New("client token is invalid")
		}
		return token, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	token := hex.EncodeToString(random[:])
	if err := privateWrite(path, []byte(token+"\n")); err != nil {
		return "", err
	}
	return token, nil
}
