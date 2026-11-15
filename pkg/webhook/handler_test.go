package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/thallyssonklein/categorize-tasks/pkg/classifier"
	"github.com/thallyssonklein/categorize-tasks/pkg/notion"
)

const secret = "secret_test"

type fakeNotion struct {
	page    *notion.Page
	ds      *notion.DataSource
	getErr  error
	setPage string
	setProp string
	setOpt  string
}

func (f *fakeNotion) GetPage(context.Context, string) (*notion.Page, error) {
	return f.page, f.getErr
}
func (f *fakeNotion) GetDataSource(context.Context, string) (*notion.DataSource, error) {
	return f.ds, nil
}
func (f *fakeNotion) SetSelect(_ context.Context, pageID, prop, opt string) error {
	f.setPage, f.setProp, f.setOpt = pageID, prop, opt
	return nil
}

type fakeClassifier struct {
	answer string
	err    error
	calls  int
	title  string
	desc   string
}

func (f *fakeClassifier) Classify(_ context.Context, title, desc string, _ []classifier.Category) (string, error) {
	f.calls++
	f.title, f.desc = title, desc
	return f.answer, f.err
}

func newPage(category string) *notion.Page {
	var sel *notion.SelectOption
	if category != "" {
		sel = &notion.SelectOption{Name: category}
	}
	return &notion.Page{
		ID:     "page-1",
		Parent: notion.Parent{Type: "data_source_id", DataSourceID: "ds-1", DatabaseID: "1234-abcd"},
		Properties: map[string]notion.PageProperty{
			"Name":        {ID: "title", Type: "title", Title: []notion.RichText{{PlainText: "Pay "}, {PlainText: "rent"}}},
			"Description": {ID: "%3AUPp", Type: "rich_text", RichText: []notion.RichText{{PlainText: "monthly bill"}}},
			"Category":    {ID: "cat%40", Type: "select", Select: sel},
		},
	}
}

func newDataSource() *notion.DataSource {
	p := notion.DataSourceProperty{ID: "cat%40", Name: "Category", Type: "select"}
	p.Select = &struct {
		Options []notion.SelectOption `json:"options"`
	}{Options: []notion.SelectOption{{Name: "Finance"}, {Name: "Work"}}}
	return &notion.DataSource{ID: "ds-1", Properties: map[string]notion.DataSourceProperty{"Category": p}}
}

func sign(body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func send(h http.Handler, body, signature string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/webhook", strings.NewReader(body))
	if signature != "" {
		req.Header.Set("X-Notion-Signature", signature)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func cfg() Config {
	return Config{Secret: secret, DatabaseID: "1234abcd", DescriptionProperty: "Description", CategoryProperty: "Category"}
}

const updatedDesc = `{"id":"ev1","type":"page.properties_updated","entity":{"id":"page-1","type":"page"},"data":{"updated_properties":["%3AUPp"]}}`

func TestUpdatesCategoryWhenDescriptionChanges(t *testing.T) {
	n := &fakeNotion{page: newPage(""), ds: newDataSource()}
	c := &fakeClassifier{answer: "Finance"}
	rec := send(NewHandler(cfg(), n, c), updatedDesc, sign(updatedDesc))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if c.title != "Pay rent" || c.desc != "monthly bill" {
		t.Errorf("classified %q / %q", c.title, c.desc)
	}
	if n.setPage != "page-1" || n.setProp != "Category" || n.setOpt != "Finance" {
		t.Errorf("SetSelect(%q, %q, %q)", n.setPage, n.setProp, n.setOpt)
	}
}

func TestIgnoresOwnCategoryWrite(t *testing.T) {
	body := `{"id":"ev2","type":"page.properties_updated","entity":{"id":"page-1","type":"page"},"data":{"updated_properties":["cat%40"]}}`
	n := &fakeNotion{page: newPage("Finance"), ds: newDataSource()}
	c := &fakeClassifier{answer: "Finance"}
	rec := send(NewHandler(cfg(), n, c), body, sign(body))

	if rec.Code != http.StatusOK || c.calls != 0 || n.setOpt != "" {
		t.Errorf("status=%d calls=%d set=%q; want no work", rec.Code, c.calls, n.setOpt)
	}
}

func TestClassifiesNewPage(t *testing.T) {
	body := `{"id":"ev3","type":"page.created","entity":{"id":"page-1","type":"page"}}`
	n := &fakeNotion{page: newPage(""), ds: newDataSource()}
	c := &fakeClassifier{answer: "Work"}
	send(NewHandler(cfg(), n, c), body, sign(body))
	if n.setOpt != "Work" {
		t.Errorf("set = %q, want Work", n.setOpt)
	}
}

func TestSkipsWriteWhenCategoryUnchanged(t *testing.T) {
	n := &fakeNotion{page: newPage("Finance"), ds: newDataSource()}
	c := &fakeClassifier{answer: "Finance"}
	send(NewHandler(cfg(), n, c), updatedDesc, sign(updatedDesc))
	if c.calls != 1 || n.setOpt != "" {
		t.Errorf("calls=%d set=%q; want classify but no write", c.calls, n.setOpt)
	}
}

func TestIgnoresOtherDatabase(t *testing.T) {
	p := newPage("")
	p.Parent.DatabaseID = "ffff"
	n := &fakeNotion{page: p, ds: newDataSource()}
	c := &fakeClassifier{answer: "Finance"}
	send(NewHandler(cfg(), n, c), updatedDesc, sign(updatedDesc))
	if c.calls != 0 {
		t.Errorf("classified a page from another database")
	}
}

func TestPausedSkipsProcessing(t *testing.T) {
	c := cfg()
	c.Paused = true
	n := &fakeNotion{page: newPage(""), ds: newDataSource()}
	cls := &fakeClassifier{answer: "Finance"}
	rec := send(NewHandler(c, n, cls), updatedDesc, sign(updatedDesc))
	if rec.Code != http.StatusOK || cls.calls != 0 || n.setOpt != "" {
		t.Errorf("status=%d calls=%d set=%q; want ack without work", rec.Code, cls.calls, n.setOpt)
	}
}

func TestRejectsBadSignature(t *testing.T) {
	n := &fakeNotion{page: newPage(""), ds: newDataSource()}
	c := &fakeClassifier{answer: "Finance"}
	rec := send(NewHandler(cfg(), n, c), updatedDesc, "sha256=00")
	if rec.Code != http.StatusUnauthorized || c.calls != 0 {
		t.Errorf("status=%d calls=%d", rec.Code, c.calls)
	}
}

func TestVerificationRequestIsAccepted(t *testing.T) {
	c := &fakeClassifier{}
	rec := send(NewHandler(cfg(), &fakeNotion{}, c), `{"verification_token":"secret_abc"}`, "")
	if rec.Code != http.StatusOK || c.calls != 0 {
		t.Errorf("status=%d calls=%d", rec.Code, c.calls)
	}
}

func TestRetryableErrors(t *testing.T) {
	cases := []struct {
		name   string
		getErr error
		clsErr error
		want   int
	}{
		{"notion 429 retries", &notion.APIError{Status: 429}, nil, http.StatusServiceUnavailable},
		{"notion 404 acks", &notion.APIError{Status: 404}, nil, http.StatusOK},
		{"network error retries", errors.New("dial tcp: timeout"), nil, http.StatusServiceUnavailable},
		{"bad model answer acks", nil, classifier.ErrBadAnswer, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n := &fakeNotion{page: newPage(""), ds: newDataSource(), getErr: tc.getErr}
			c := &fakeClassifier{err: tc.clsErr}
			rec := send(NewHandler(cfg(), n, c), updatedDesc, sign(updatedDesc))
			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d", rec.Code, tc.want)
			}
		})
	}
}
