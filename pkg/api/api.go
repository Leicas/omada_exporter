package api

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"time"

	"github.com/charlie-haley/omada_exporter/pkg/config"
	log "github.com/rs/zerolog/log"
)

// apiResponse is a generic Omada API response with the result as raw JSON.
type apiResponse struct {
	ErrorCode int             `json:"errorCode"`
	Msg       string          `json:"msg"`
	Result    json.RawMessage `json:"result"`
}

// checkResponse parses an API response body, checks for API-level errors,
// and returns the raw result payload.
func checkResponse(body []byte) (json.RawMessage, error) {
	var resp apiResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("failed to parse API response: %w", err)
	}
	if resp.ErrorCode != 0 {
		return nil, fmt.Errorf("API error %d: %s", resp.ErrorCode, resp.Msg)
	}
	return resp.Result, nil
}

// parseListResult unmarshals a list result from an API response body.
// It handles both direct array format ({"result": [...]}) and the paginated
// format used by newer Omada controllers ({"result": {"data": [...]}}).
func parseListResult[T any](body []byte) ([]T, error) {
	raw, err := checkResponse(body)
	if err != nil {
		return nil, err
	}

	// Try paginated format first: {"data": [...], "totalRows": N, ...}
	var paginated struct {
		Data []T `json:"data"`
	}
	if err := json.Unmarshal(raw, &paginated); err == nil && paginated.Data != nil {
		return paginated.Data, nil
	}

	// Fallback to direct array format: [...]
	var direct []T
	if err := json.Unmarshal(raw, &direct); err != nil {
		return nil, fmt.Errorf("failed to parse list result: %w", err)
	}
	return direct, nil
}

type Client struct {
	Config     *config.Config
	httpClient *http.Client
	token      string
	omadaCID   string
	SiteId     string
}

func setuphttpClient(insecure bool, timeout int) (*http.Client, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, fmt.Errorf("failed to init cookiejar")
	}
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.MaxIdleConns = 100
	t.MaxConnsPerHost = 100
	t.MaxIdleConnsPerHost = 100

	client := &http.Client{Transport: t, Timeout: time.Duration(timeout) * time.Second, Jar: jar}

	if insecure {
		t.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}

	return client, nil
}

func Configure(c *config.Config) (*Client, error) {
	httpClient, err := setuphttpClient(c.Insecure, c.Timeout)
	if err != nil {
		return nil, err
	}

	client := &Client{
		Config:     c,
		httpClient: httpClient,
	}
	cid, err := client.getCid()
	if err != nil {
		return nil, err
	}
	client.omadaCID = cid

	sid, err := client.getSiteId(c.Site)
	if err != nil {
		return nil, err
	}
	client.SiteId = *sid

	return client, nil
}

func (c *Client) makeRequest(req *http.Request) (*http.Response, error) {
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("User-Agent", "omada_exporter")
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Omada-Request-Source", "web-local")

	if c.token != "" {
		req.Header.Set("Csrf-Token", c.token)
	}

	return c.httpClient.Do(req)
}

// makeLoggedInRequest ensures we are logged in before making the request.
// Instead of pre-checking login status on every call, it logs in once on first
// use and only re-authenticates when a request returns an auth error.
func (c *Client) makeLoggedInRequest(req *http.Request) (*http.Response, error) {
	// Login once if we don't have a token yet
	if c.token == "" {
		log.Info().Msg(fmt.Sprintf("not logged in, logging in with user: %s", c.Config.Username))
		if err := c.Login(); err != nil {
			return nil, fmt.Errorf("login failed: %w", err)
		}
	}

	return c.makeRequest(req)
}
