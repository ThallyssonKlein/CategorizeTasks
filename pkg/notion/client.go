// Package notion is a minimal client for the few Notion API endpoints this
// webhook needs: retrieve a page, retrieve a data source, and set a select.
package notion

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"

	"golang.org/x/time/rate"
)

const (
	baseURL = "https://api.notion.com/v1"
	// APIVersion is sent as the Notion-Version header on every request.
	APIVersion = "2026-03-11"
)

const (
	// Notion documents an average of 3 requests per second per integration,
	// with some burst allowed.
	// https://developers.notion.com/reference/request-limits
	requestsPerSecond = 3
	requestBurst      = 3

	// maxRetries bounds retries on 429/503; each retry is a new request and
	// goes through the limiter again.
	maxRetries     = 3
	defaultBackoff = 500 * time.Millisecond
	maxBackoff     = 8 * time.Second
	requestTimeout = 15 * time.Second
)

type Client struct {
	token string
	http  *http.Client
	// limiter is shared by every goroutine using this client.
	limiter *rate.Limiter
}

func NewClient(token string) *Client {
	return &Client{
		token:   token,
		http:    &http.Client{Timeout: requestTimeout},
		limiter: rate.NewLimiter(rate.Limit(requestsPerSecond), requestBurst),
	}
}

// APIError is returned when Notion answers with a non-2xx status.
type APIError struct {
	Status  int    `json:"status"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *APIError) Error() string {
	return fmt.Sprintf("notion: %d %s: %s", e.Status, e.Code, e.Message)
}

// Retryable reports whether Notion may succeed if the request is tried again.
func (e *APIError) Retryable() bool {
	return e.Status == http.StatusTooManyRequests || e.Status >= 500
}

type RichText struct {
	PlainText string `json:"plain_text"`
}

type SelectOption struct {
	ID          string `json:"id,omitempty"`
	Name        string `json:"name"`
	Color       string `json:"color,omitempty"`
	Description string `json:"description,omitempty"`
}

type PageProperty struct {
	ID       string        `json:"id"`
	Type     string        `json:"type"`
	Title    []RichText    `json:"title,omitempty"`
	RichText []RichText    `json:"rich_text,omitempty"`
	Select   *SelectOption `json:"select,omitempty"`
}

type Parent struct {
	Type         string `json:"type"`
	DataSourceID string `json:"data_source_id,omitempty"`
	DatabaseID   string `json:"database_id,omitempty"`
}

type Page struct {
	ID         string                  `json:"id"`
	Parent     Parent                  `json:"parent"`
	InTrash    bool                    `json:"in_trash"`
	Properties map[string]PageProperty `json:"properties"`
}

type DataSourceProperty struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Type   string `json:"type"`
	Select *struct {
		Options []SelectOption `json:"options"`
	} `json:"select,omitempty"`
}

type DataSource struct {
	ID         string                        `json:"id"`
	Properties map[string]DataSourceProperty `json:"properties"`
}

func (c *Client) GetPage(ctx context.Context, pageID string) (*Page, error) {
	var p Page
	if err := c.do(ctx, http.MethodGet, "/pages/"+pageID, nil, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

func (c *Client) GetDataSource(ctx context.Context, dataSourceID string) (*DataSource, error) {
	var ds DataSource
	if err := c.do(ctx, http.MethodGet, "/data_sources/"+dataSourceID, nil, &ds); err != nil {
		return nil, err
	}
	return &ds, nil
}

// SetSelect sets the select property named property on a page to the option
// called option.
func (c *Client) SetSelect(ctx context.Context, pageID, property, option string) error {
	body := map[string]any{
		"properties": map[string]any{
			property: map[string]any{"select": map[string]string{"name": option}},
		},
	}
	return c.do(ctx, http.MethodPatch, "/pages/"+pageID, body, nil)
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var payload []byte
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		payload = b
	}

	for attempt := 0; ; attempt++ {
		if err := c.limiter.Wait(ctx); err != nil {
			return fmt.Errorf("notion: waiting for rate limit: %w", err)
		}
		wait, err := c.attempt(ctx, method, path, payload, out)
		var apiErr *APIError
		retry := errors.As(err, &apiErr) &&
			(apiErr.Status == http.StatusTooManyRequests || apiErr.Status == http.StatusServiceUnavailable)
		if !retry || attempt >= maxRetries {
			return err
		}
		if wait <= 0 {
			wait = backoff(attempt)
		}
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// attempt performs one request. On an API error it also returns the delay
// requested by the Retry-After header, or 0 when absent.
func (c *Client) attempt(ctx context.Context, method, path string, payload []byte, out any) (time.Duration, error) {
	var reqBody io.Reader
	if payload != nil {
		reqBody = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, baseURL+path, reqBody)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Notion-Version", APIVersion)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("notion: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, err
	}
	if resp.StatusCode/100 != 2 {
		apiErr := &APIError{Status: resp.StatusCode}
		if json.Unmarshal(data, apiErr) != nil || apiErr.Message == "" {
			apiErr.Message = strings.TrimSpace(string(data))
		}
		apiErr.Status = resp.StatusCode
		return retryAfter(resp.Header), apiErr
	}
	if out != nil {
		return 0, json.Unmarshal(data, out)
	}
	return 0, nil
}

// retryAfter parses the Retry-After header (seconds), capped at maxBackoff.
func retryAfter(h http.Header) time.Duration {
	secs, err := strconv.Atoi(h.Get("Retry-After"))
	if err != nil || secs < 0 {
		return 0
	}
	return min(time.Duration(secs)*time.Second, maxBackoff)
}

// backoff is exponential with full jitter.
func backoff(attempt int) time.Duration {
	d := min(defaultBackoff<<attempt, maxBackoff)
	return d/2 + rand.N(d/2+1)
}

// PlainText joins the plain text of a title or rich_text property.
func (p PageProperty) PlainText() string {
	parts := p.RichText
	if p.Type == "title" {
		parts = p.Title
	}
	var sb strings.Builder
	for _, t := range parts {
		sb.WriteString(t.PlainText)
	}
	return strings.TrimSpace(sb.String())
}

// TitleProperty returns the page's title property and its name.
func (p *Page) TitleProperty() (string, PageProperty, bool) {
	for name, prop := range p.Properties {
		if prop.Type == "title" {
			return name, prop, true
		}
	}
	return "", PageProperty{}, false
}

// NormalizeID strips dashes and lowercases a Notion ID so IDs copied from
// URLs and IDs returned by the API compare equal.
func NormalizeID(id string) string {
	return strings.ToLower(strings.ReplaceAll(id, "-", ""))
}
