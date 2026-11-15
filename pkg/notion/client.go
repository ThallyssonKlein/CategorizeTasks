// Package notion is a minimal client for the few Notion API endpoints this
// webhook needs: retrieve a page, retrieve a data source, and set a select.
package notion

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	baseURL = "https://api.notion.com/v1"
	// APIVersion is sent as the Notion-Version header on every request.
	APIVersion = "2026-03-11"
)

type Client struct {
	token string
	http  *http.Client
}

func NewClient(token string) *Client {
	return &Client{token: token, http: &http.Client{Timeout: 15 * time.Second}}
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
	var reqBody io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reqBody = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, baseURL+path, reqBody)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Notion-Version", APIVersion)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("notion: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		apiErr := &APIError{Status: resp.StatusCode}
		if json.Unmarshal(data, apiErr) != nil || apiErr.Message == "" {
			apiErr.Message = strings.TrimSpace(string(data))
		}
		apiErr.Status = resp.StatusCode
		return apiErr
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
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
