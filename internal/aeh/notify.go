package aeh

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Event is what AEH reports to the owner when an outcome blocks or finishes.
type Event struct {
	Event   string `json:"event"`
	Outcome string `json:"outcome"`
	Title   string `json:"title"`
	Message string `json:"message"`
}

// notify posts the event to the configured webhook. Failures are returned, never fatal.
func notify(url string, e Event) error {
	if url == "" {
		return nil
	}
	body, err := json.Marshal(e)
	if err != nil {
		return err
	}
	client := http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("webhook answered %s", resp.Status)
	}
	return nil
}

// TestNotify sends a sample event so the owner can check the channel.
func TestNotify(r Repo) error {
	c, err := r.LoadConfig()
	if err != nil {
		return err
	}
	if c.Notify.Webhook == "" {
		return fmt.Errorf("notify.webhook is not set in %s", rel(r.Root, r.ConfigPath()))
	}
	return notify(c.Notify.Webhook, Event{Event: "test", Message: "AEH notification test"})
}
