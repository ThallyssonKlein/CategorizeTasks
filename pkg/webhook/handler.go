// Package webhook receives Notion webhook events and fills a select property
// with a category chosen by Claude from the page's title and description.
package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go/v2"

	"github.com/thallyssonklein/categorize-tasks/pkg/classifier"
	"github.com/thallyssonklein/categorize-tasks/pkg/notion"
	"github.com/thallyssonklein/categorize-tasks/pkg/usage"
)

// Notion is the subset of the Notion API the handler uses.
type Notion interface {
	GetPage(ctx context.Context, pageID string) (*notion.Page, error)
	GetDataSource(ctx context.Context, dataSourceID string) (*notion.DataSource, error)
	SetSelect(ctx context.Context, pageID, property, option string) error
}

// Classifier picks a category for a task.
type Classifier interface {
	Classify(ctx context.Context, title, description string, categories []classifier.Category) (string, classifier.Usage, error)
	Estimate(ctx context.Context, title, description string, categories []classifier.Category) (classifier.Usage, error)
}

type Config struct {
	// Secret is the verification_token Notion sent when the subscription was
	// created. It signs every event. Empty disables signature checks, which
	// is only meant for the first deploy, before you have the token.
	Secret string
	// DatabaseID, if set, limits processing to pages of that database.
	DatabaseID string
	// DescriptionProperty and CategoryProperty are property names in the
	// database. The title property is found by type.
	DescriptionProperty string
	CategoryProperty    string
	// FallbackCategory is the select option used when the model cannot pick
	// one (refusal, truncation, invalid answer). It must exist in the select.
	// Empty disables the fallback.
	FallbackCategory string
	// DryRun never runs the model nor writes to Notion: it only counts the
	// tokens a classification would use (free count_tokens call) and saves them.
	DryRun bool
	// Paused acknowledges every event without processing it.
	Paused bool
}

// ConfigFromEnv reads the configuration from environment variables.
func ConfigFromEnv() Config {
	return Config{
		Secret:              os.Getenv("NOTION_WEBHOOK_SECRET"),
		DatabaseID:          os.Getenv("NOTION_DATABASE_ID"),
		DescriptionProperty: envOr("NOTION_DESCRIPTION_PROPERTY", "Description"),
		CategoryProperty:    envOr("NOTION_CATEGORY_PROPERTY", "Category"),
		FallbackCategory:    envOr("NOTION_FALLBACK_CATEGORY", "Não encontrado"),
		DryRun:              os.Getenv("DRY_RUN") == "true",
		Paused:              os.Getenv("WEBHOOK_PAUSED") == "true",
	}
}

type Handler struct {
	cfg        Config
	notion     Notion
	classifier Classifier
	usage      usage.Recorder
}

// WithUsage makes the handler save token usage of every classification.
// Failures to save are logged and never fail the event.
func (h *Handler) WithUsage(r usage.Recorder) *Handler {
	h.usage = r
	return h
}

func NewHandler(cfg Config, n Notion, c Classifier) *Handler {
	return &Handler{cfg: cfg, notion: n, classifier: c}
}

// Event is the part of a Notion webhook payload the handler reads.
type Event struct {
	ID                string `json:"id"`
	Type              string `json:"type"`
	VerificationToken string `json:"verification_token"`
	Entity            struct {
		ID   string `json:"id"`
		Type string `json:"type"`
	} `json:"entity"`
	Data struct {
		UpdatedProperties []string `json:"updated_properties"`
	} `json:"data"`
}

// errSkip marks events that are valid but need no work.
type errSkip struct{ reason string }

func (e errSkip) Error() string { return e.reason }

func skip(format string, args ...any) error { return errSkip{fmt.Sprintf(format, args...)} }

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}

	var ev Event
	if err := json.Unmarshal(body, &ev); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}

	// The one-time verification request is unsigned. Log the token so it
	// can be pasted into Notion and saved as NOTION_WEBHOOK_SECRET.
	if ev.VerificationToken != "" {
		log.Printf("notion verification_token: %s", ev.VerificationToken)
		w.WriteHeader(http.StatusOK)
		return
	}

	if h.cfg.Secret == "" {
		log.Printf("warning: NOTION_WEBHOOK_SECRET is not set; accepting unsigned event %s", ev.ID)
	} else if !validSignature(body, r.Header.Get("X-Notion-Signature"), h.cfg.Secret) {
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}

	if h.cfg.Paused {
		// Acknowledge so Notion does not queue retries; the event is dropped.
		log.Printf("event %s (%s): skipped: WEBHOOK_PAUSED is set", ev.ID, ev.Type)
		w.WriteHeader(http.StatusOK)
		return
	}

	err = h.process(r.Context(), ev)
	var s errSkip
	switch {
	case err == nil:
		w.WriteHeader(http.StatusOK)
	case errors.As(err, &s):
		log.Printf("event %s (%s): skipped: %s", ev.ID, ev.Type, s.reason)
		w.WriteHeader(http.StatusOK)
	case retryable(err):
		// A non-2xx answer makes Notion retry the delivery later.
		log.Printf("event %s (%s): retryable error: %v", ev.ID, ev.Type, err)
		http.Error(w, "temporary failure", http.StatusServiceUnavailable)
	default:
		// Retrying will not fix it (bad config, missing property...), so
		// acknowledge to stop Notion from redelivering.
		log.Printf("event %s (%s): error: %v", ev.ID, ev.Type, err)
		w.WriteHeader(http.StatusOK)
	}
}

func (h *Handler) process(ctx context.Context, ev Event) error {
	switch ev.Type {
	case "page.created", "page.properties_updated":
	default:
		return skip("event type not handled")
	}
	if ev.Entity.Type != "page" || ev.Entity.ID == "" {
		return skip("entity is not a page")
	}

	page, err := h.notion.GetPage(ctx, ev.Entity.ID)
	if err != nil {
		return fmt.Errorf("get page: %w", err)
	}
	if page.InTrash {
		return skip("page is in trash")
	}
	if page.Parent.DataSourceID == "" {
		return skip("page is not in a database")
	}
	if h.cfg.DatabaseID != "" && notion.NormalizeID(page.Parent.DatabaseID) != notion.NormalizeID(h.cfg.DatabaseID) {
		return skip("page belongs to another database")
	}

	_, titleProp, ok := page.TitleProperty()
	if !ok {
		return permanent("page has no title property")
	}
	descProp, hasDesc := page.Properties[h.cfg.DescriptionProperty]
	catProp, ok := page.Properties[h.cfg.CategoryProperty]
	if !ok || catProp.Type != "select" {
		return permanent("database has no select property %q", h.cfg.CategoryProperty)
	}

	// Only react to edits of the title or description. This also stops the
	// loop: our own write to the category fires another properties_updated
	// event, which lists only the category property.
	if ev.Type == "page.properties_updated" {
		watched := []string{titleProp.ID}
		if hasDesc {
			watched = append(watched, descProp.ID)
		}
		if !touches(ev.Data.UpdatedProperties, watched) {
			return skip("title and description unchanged")
		}
	}

	title := titleProp.PlainText()
	description := ""
	if hasDesc {
		description = descProp.PlainText()
	}
	if title == "" && description == "" {
		return skip("title and description are empty")
	}

	ds, err := h.notion.GetDataSource(ctx, page.Parent.DataSourceID)
	if err != nil {
		return fmt.Errorf("get data source: %w", err)
	}
	categories, err := categoriesOf(ds, h.cfg.CategoryProperty)
	if err != nil {
		return err
	}

	if h.cfg.DryRun {
		u, err := h.classifier.Estimate(ctx, title, description, categories)
		if err != nil {
			return err
		}
		h.saveUsage(ctx, ev.ID, page.ID, "", u)
		log.Printf("event %s: dry-run: page %s %q: ~%d input + ~%d output tokens (not classified)",
			ev.ID, page.ID, title, u.InputTokens, u.OutputTokens)
		return nil
	}

	category, u, err := h.classifier.Classify(ctx, title, description, categories)
	if errors.Is(err, classifier.ErrBadAnswer) && h.cfg.FallbackCategory != "" {
		log.Printf("event %s: page %s: %v; using %q", ev.ID, page.ID, err, h.cfg.FallbackCategory)
		category, err = h.cfg.FallbackCategory, nil
	}
	if u.Model != "" {
		h.saveUsage(ctx, ev.ID, page.ID, category, u)
	}
	if err != nil {
		return err
	}

	if catProp.Select != nil && catProp.Select.Name == category {
		return skip("category already %q", category)
	}
	if err := h.notion.SetSelect(ctx, page.ID, h.cfg.CategoryProperty, category); err != nil {
		return fmt.Errorf("set category: %w", err)
	}
	log.Printf("event %s: page %s %q -> %q", ev.ID, page.ID, title, category)
	return nil
}

func (h *Handler) saveUsage(ctx context.Context, eventID, pageID, category string, u classifier.Usage) {
	if h.usage == nil {
		return
	}
	err := h.usage.Record(ctx, usage.Record{
		EventID: eventID, Time: time.Now().UTC(), PageID: pageID, Model: u.Model,
		InputTokens: u.InputTokens, OutputTokens: u.OutputTokens,
		CacheReadTokens: u.CacheReadTokens, CacheWriteTokens: u.CacheWriteTokens,
		Category: category, DryRun: h.cfg.DryRun, Estimated: u.Estimated,
	})
	if err != nil {
		log.Printf("event %s: %v", eventID, err)
	}
}

func categoriesOf(ds *notion.DataSource, property string) ([]classifier.Category, error) {
	prop, ok := ds.Properties[property]
	if !ok || prop.Type != "select" || prop.Select == nil {
		return nil, permanent("data source has no select property %q", property)
	}
	if len(prop.Select.Options) == 0 {
		return nil, permanent("select property %q has no options; add the categories in Notion", property)
	}
	cats := make([]classifier.Category, len(prop.Select.Options))
	for i, o := range prop.Select.Options {
		cats[i] = classifier.Category{Name: o.Name, Description: o.Description}
	}
	return cats, nil
}

// touches reports whether any updated property ID is one of watched. Notion
// returns property IDs URL-encoded; compare both forms to be safe.
func touches(updated, watched []string) bool {
	for _, u := range updated {
		for _, w := range watched {
			if u == w || unescape(u) == unescape(w) {
				return true
			}
		}
	}
	return false
}

func unescape(s string) string {
	if v, err := url.PathUnescape(s); err == nil {
		return v
	}
	return s
}

func validSignature(body []byte, header, secret string) bool {
	sig, ok := strings.CutPrefix(header, "sha256=")
	if !ok {
		return false
	}
	got, err := hex.DecodeString(sig)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hmac.Equal(got, mac.Sum(nil))
}

// permanentError marks failures that a redelivery will not fix, such as a
// missing property in the database.
type permanentError struct{ error }

func permanent(format string, args ...any) error {
	return permanentError{fmt.Errorf(format, args...)}
}

func retryable(err error) bool {
	var perm permanentError
	if errors.As(err, &perm) || errors.Is(err, classifier.ErrBadAnswer) {
		return false
	}
	var nerr *notion.APIError
	if errors.As(err, &nerr) {
		return nerr.Retryable()
	}
	var aerr *anthropic.Error
	if errors.As(err, &aerr) {
		return aerr.StatusCode == http.StatusTooManyRequests || aerr.StatusCode >= 500
	}
	// Timeouts and network failures: let Notion try again later.
	return true
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
