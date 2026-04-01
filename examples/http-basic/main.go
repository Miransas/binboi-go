package main

import (
	"encoding/json"
	"log"
	"net/http"
	"time"
)

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"service": "http-basic",
			"status":  "ok",
			"time":    time.Now().UTC(),
			"path":    r.URL.Path,
		})
	})

	log.Println("http-basic listening on :3000")
	log.Fatal(http.ListenAndServe(":3000", mux))
}
