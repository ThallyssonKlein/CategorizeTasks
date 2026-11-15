// Command local runs the webhook as a plain HTTP server for development.
// Expose it with a tunnel (ngrok, cloudflared) to receive Notion events.
package main

import (
	"log"
	"net/http"
	"os"

	"github.com/thallyssonklein/categorize-tasks/pkg/classifier"
	"github.com/thallyssonklein/categorize-tasks/pkg/notion"
	"github.com/thallyssonklein/categorize-tasks/pkg/usage"
	"github.com/thallyssonklein/categorize-tasks/pkg/webhook"
)

func main() {
	addr := ":" + envOr("PORT", "8080")
	h := webhook.NewHandler(
		webhook.ConfigFromEnv(),
		notion.NewClient(os.Getenv("NOTION_TOKEN")),
		classifier.New(),
	)
	rec, err := usage.NewMongoFromEnv()
	if err != nil {
		log.Printf("usage tracking disabled: %v", err)
	} else if rec != nil {
		h.WithUsage(rec)
	}
	http.Handle("/api/webhook", h)
	log.Printf("listening on %s/api/webhook", addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
