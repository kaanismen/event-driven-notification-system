// Command watch follows a notification or a batch in real time over the WebSocket API
// (ADR-0020) and prints what happens until everything is final. It exists for demos and
// manual checks, because curl cannot speak WebSocket.
//
//	go run ./cmd/watch -notification <id>
//	go run ./cmd/watch -batch <id> [-v]
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// message holds the fields of both message types; unused ones stay empty.
type message struct {
	Type         string `json:"type"`
	Notification *struct {
		ID       string `json:"id"`
		Status   string `json:"status"`
		Attempts int    `json:"attempts"`
	} `json:"notification"`
	Batch *struct {
		Total         int `json:"total"`
		Notifications []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"notifications"`
	} `json:"batch"`

	ID        string `json:"id"`
	Status    string `json:"status"`
	Attempts  int    `json:"attempts"`
	LastError string `json:"lastError"`
}

func main() {
	base := flag.String("url", "ws://localhost:8080", "base URL of the API")
	notificationID := flag.String("notification", "", "notification ID to watch")
	batchID := flag.String("batch", "", "batch ID to watch")
	verbose := flag.Bool("v", false, "for a batch, print every event instead of a summary")
	flag.Parse()

	var path string
	switch {
	case *notificationID != "" && *batchID == "":
		path = "/v1/notifications/" + *notificationID + "/events"
	case *batchID != "" && *notificationID == "":
		path = "/v1/batches/" + *batchID + "/events"
	default:
		fmt.Fprintln(os.Stderr, "usage: watch -notification <id> | -batch <id> [-v] [-url ws://host:port]")
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := watch(ctx, *base+path, *batchID != "" && !*verbose); err != nil {
		fmt.Fprintln(os.Stderr, "watch:", err)
		os.Exit(1)
	}
}

func watch(ctx context.Context, url string, summarize bool) error {
	dialCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	conn, resp, err := websocket.Dial(dialCtx, url, nil)
	cancel()
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer func() { _ = conn.CloseNow() }()
	// A batch snapshot holds up to 1000 notifications with their content: about 0.4 MB
	// for short SMS, several MB for long emails. The library's default limit is 32 KiB.
	conn.SetReadLimit(32 << 20)

	start := time.Now()
	statuses := map[string]string{} // notification ID -> latest status
	var lastSummary time.Time
	printSummary := func() {
		fmt.Printf("%7.2fs  %s\n", time.Since(start).Seconds(), counts(statuses))
		lastSummary = time.Now()
	}

	for {
		var msg message
		err := wsjson.Read(ctx, conn, &msg)
		if code := websocket.CloseStatus(err); code != -1 {
			if summarize {
				printSummary()
			}
			var ce websocket.CloseError
			errors.As(err, &ce)
			fmt.Printf("closed by server: %d %s\n", code, ce.Reason)
			if code != websocket.StatusNormalClosure {
				return fmt.Errorf("connection closed with %d", code)
			}
			return nil
		}
		if err != nil {
			return fmt.Errorf("read: %w", err)
		}

		switch {
		case msg.Type == "snapshot" && msg.Notification != nil:
			n := msg.Notification
			statuses[n.ID] = n.Status
			fmt.Printf("snapshot  %s  %s (attempts %d)\n", n.ID, n.Status, n.Attempts)
		case msg.Type == "snapshot" && msg.Batch != nil:
			for _, n := range msg.Batch.Notifications {
				statuses[n.ID] = n.Status
			}
			fmt.Printf("snapshot  %d items  %s\n", msg.Batch.Total, counts(statuses))
		case msg.Type == "status":
			statuses[msg.ID] = msg.Status
			if summarize {
				if time.Since(lastSummary) >= 500*time.Millisecond {
					printSummary()
				}
				continue
			}
			line := fmt.Sprintf("%7.2fs  %s  %-10s attempt %d", time.Since(start).Seconds(), msg.ID, msg.Status, msg.Attempts)
			if msg.LastError != "" {
				line += "  (" + msg.LastError + ")"
			}
			fmt.Println(line)
		}
	}
}

// counts formats the number of notifications per status, for example
// "processing=12 sent=988".
func counts(statuses map[string]string) string {
	n := map[string]int{}
	for _, s := range statuses {
		n[s]++
	}
	keys := make([]string, 0, len(n))
	for k := range n {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s=%d", k, n[k])
	}
	return strings.Join(parts, " ")
}
