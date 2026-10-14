// main.go
package main

import (
	"context"
	"log"
	"math/rand"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}

type welcome struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}
type update struct {
	Type      string  `json:"type"`
	Timestamp string  `json:"timestamp"`
	Value     float64 `json:"value"`
}

func wsHandler(w http.ResponseWriter, r *http.Request) {
	c, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("[ERROR] upgrade: %v", err)
		return
	}
	defer c.Close()
	_ = c.WriteJSON(welcome{Type: "welcome", Message: "Connected to ARB stream"})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// read pump (just to detect disconnect)
	go func() {
		defer cancel()
		for {
			_, _, err := c.ReadMessage()
			if err != nil {
				return
			}
		}
	}()

	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case t := <-tick.C:
			msg := update{
				Type:      "update",
				Timestamp: t.UTC().Format(time.RFC3339),
				Value:     rand.Float64() * 100,
			}
			if err := c.WriteJSON(msg); err != nil {
				log.Printf("[ERROR] write: %v", err)
				return
			}
		}
	}
}

func main() {
	http.HandleFunc("/ws", wsHandler)
	log.Println("[INFO] listening on :8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}
