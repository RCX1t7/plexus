// Command fakeslack runs the fake Slack standalone, for manual hub development
// (e.g. point a locally built Plexus at it via config.json "slack_api_url"). Control endpoints:
//
//	POST /_sim/say?thread=TS   body = text   (posts as the owner, prints ts)
//	GET  /_sim/messages                      (all messages as JSON)
//	GET  /_sim/duplicates                    (duplicate groups as JSON)
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"

	"github.com/RCX1t7/plexus/tests/fakeslack"
)

func main() {
	addr := flag.String("control", "127.0.0.1:8766", "control listen address")
	flag.Parse()
	s := fakeslack.New(fakeslack.DefaultOptions())
	defer s.Close()
	fmt.Printf("fake Slack API base: %s\n", s.APIURL())
	for _, b := range s.Options().Bots {
		fmt.Printf("bot %s: user=%s bot_token=%s app_token=%s\n", b.Name, b.UserID, b.Token, b.AppToken)
	}
	fmt.Printf("owner: %s channel: %s\ncontrol: http://%s/_sim/\n", s.Options().OwnerID, s.Options().Channel, *addr)
	mux := http.NewServeMux()
	mux.HandleFunc("/_sim/say", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		fmt.Fprintln(w, s.UserPost(r.URL.Query().Get("thread"), string(b)))
	})
	mux.HandleFunc("/_sim/messages", func(w http.ResponseWriter, r *http.Request) { _ = json.NewEncoder(w).Encode(s.Messages()) })
	mux.HandleFunc("/_sim/duplicates", func(w http.ResponseWriter, r *http.Request) { _ = json.NewEncoder(w).Encode(s.Duplicates()) })
	log.Fatal(http.ListenAndServe(*addr, mux))
}
