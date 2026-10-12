// Package transport is the agent's HTTPS client for the control plane.
package transport

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"linux-dash-agent/internal/protocol"
)

// ErrUnauthorized means the control plane rejected the agent's credentials
// (revoked or unknown agent) and re-enrolment is needed.
var ErrUnauthorized = errors.New("control plane rejected agent credentials")

type Options struct {
	BaseURL            string
	CAFile             string
	InsecureSkipVerify bool
	UserAgent          string
}

type Client struct {
	base    string
	ua      string
	http    *http.Client
	agentID string
	secret  string
}

func New(o Options) (*Client, error) {
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: o.InsecureSkipVerify} //nolint:gosec // opt-in for self-signed dev setups
	if o.CAFile != "" {
		pem, err := os.ReadFile(o.CAFile)
		if err != nil {
			return nil, fmt.Errorf("reading ca_file: %w", err)
		}
		pool, err := x509.SystemCertPool()
		if err != nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("ca_file contains no PEM certificates")
		}
		tlsCfg.RootCAs = pool
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = tlsCfg
	return &Client{
		base: strings.TrimRight(o.BaseURL, "/"),
		ua:   o.UserAgent,
		http: &http.Client{Transport: tr, Timeout: 30 * time.Second},
	}, nil
}

func (c *Client) SetCredentials(agentID, secret string) { c.agentID, c.secret = agentID, secret }

func (c *Client) Enroll(ctx context.Context, req protocol.EnrollRequest) (protocol.EnrollResponse, error) {
	var resp protocol.EnrollResponse
	if err := c.post(ctx, protocol.PathEnroll, req, &resp, false); err != nil {
		return resp, err
	}
	if resp.AgentID == "" || resp.Secret == "" {
		return resp, errors.New("enroll response missing agent_id or secret")
	}
	return resp, nil
}

func (c *Client) Report(ctx context.Context, r *protocol.Report) (protocol.ReportResponse, error) {
	var resp protocol.ReportResponse
	err := c.post(ctx, protocol.PathReport, r, &resp, true)
	return resp, err
}

func (c *Client) post(ctx context.Context, path string, body, out any, auth bool) error {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if err := json.NewEncoder(gz).Encode(body); err != nil {
		return err
	}
	if err := gz.Close(); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "gzip")
	req.Header.Set("User-Agent", c.ua)
	req.Header.Set("X-Agent-Protocol", protocol.Version)
	if auth {
		req.Header.Set("X-Agent-ID", c.agentID)
		req.Header.Set("Authorization", "Bearer "+c.secret)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return ErrUnauthorized
	}
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("POST %s: %s: %s", path, resp.Status, strings.TrimSpace(string(msg)))
	}
	if out == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(out); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("POST %s: decoding response: %w", path, err)
	}
	return nil
}
