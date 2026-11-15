// Package api is the Vercel serverless entrypoint, served at /api/webhook.
package api

import (
	"net/http"
	"os"
	"sync"

	"github.com/thallyssonklein/categorize-tasks/pkg/classifier"
	"github.com/thallyssonklein/categorize-tasks/pkg/notion"
	"github.com/thallyssonklein/categorize-tasks/pkg/webhook"
)

var (
	once    sync.Once
	handler http.Handler
)

// Handler is the function Vercel invokes for each request.
func Handler(w http.ResponseWriter, r *http.Request) {
	once.Do(func() {
		handler = webhook.NewHandler(
			webhook.ConfigFromEnv(),
			notion.NewClient(os.Getenv("NOTION_TOKEN")),
			classifier.New(),
		)
	})
	handler.ServeHTTP(w, r)
}
