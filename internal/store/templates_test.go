package store

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/kaanismen/event-driven-notification-system/internal/notification"
)

func TestCreateTemplateIsIdempotentByName(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	first, created, err := s.CreateTemplate(ctx, "otp", notification.ChannelSMS, "Your code is {{code}}")
	if err != nil || !created {
		t.Fatalf("first create: created=%t err=%v", created, err)
	}
	if first.Name != "otp" || first.Channel != notification.ChannelSMS || first.CreatedAt.IsZero() {
		t.Errorf("stored template = %+v", first)
	}

	again, created, err := s.CreateTemplate(ctx, "otp", notification.ChannelSMS, "Your code is {{code}}")
	if err != nil || created || again.ID != first.ID {
		t.Errorf("same template again: id %s created=%t err=%v; want the original %s", again.ID, created, err, first.ID)
	}

	for name, args := range map[string][2]string{
		"different body":    {string(notification.ChannelSMS), "Code: {{code}}"},
		"different channel": {string(notification.ChannelPush), "Your code is {{code}}"},
	} {
		_, _, err := s.CreateTemplate(ctx, "otp", notification.Channel(args[0]), args[1])
		if !errors.Is(err, ErrTemplateNameTaken) {
			t.Errorf("%s: err = %v, want ErrTemplateNameTaken", name, err)
		}
	}
}

// Concurrent identical creates must end with one template, which every caller gets back.
func TestConcurrentTemplateCreates(t *testing.T) {
	s := newTestStore(t)
	const callers = 10

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		ids     = map[uuid.UUID]bool{}
		created int
	)
	for range callers {
		wg.Go(func() {
			tpl, c, err := s.CreateTemplate(context.Background(), "welcome", notification.ChannelEmail, "Welcome, {{name}}!")
			if err != nil {
				t.Errorf("create: %v", err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			ids[tpl.ID] = true
			if c {
				created++
			}
		})
	}
	wg.Wait()
	if len(ids) != 1 || created != 1 {
		t.Errorf("%d distinct IDs and %d creations, want 1 and 1", len(ids), created)
	}
}

func TestGetTemplates(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	a, _, _ := s.CreateTemplate(ctx, "a", notification.ChannelSMS, "A {{x}}")
	b, _, _ := s.CreateTemplate(ctx, "b", notification.ChannelPush, "B")
	unknown := uuid.Must(uuid.NewV7())

	got, err := s.GetTemplates(ctx, []uuid.UUID{a.ID, b.ID, unknown})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[a.ID].Body != "A {{x}}" || got[b.ID].Channel != notification.ChannelPush {
		t.Errorf("GetTemplates = %+v, want a and b only", got)
	}
	if empty, err := s.GetTemplates(ctx, nil); err != nil || len(empty) != 0 {
		t.Errorf("GetTemplates(nil) = %v, %v", empty, err)
	}

	if one, err := s.GetTemplate(ctx, a.ID); err != nil || one.Name != "a" {
		t.Errorf("GetTemplate = %+v, %v", one, err)
	}
	if _, err := s.GetTemplate(ctx, unknown); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetTemplate(unknown) err = %v, want ErrNotFound", err)
	}
}

func TestListTemplatesPaginates(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	want := map[uuid.UUID]bool{}
	for i := range 5 {
		ch := notification.ChannelSMS
		if i%2 == 1 {
			ch = notification.ChannelEmail
		}
		tpl, _, err := s.CreateTemplate(ctx, fmt.Sprintf("t%d", i), ch, "x")
		if err != nil {
			t.Fatal(err)
		}
		want[tpl.ID] = true
	}

	seen := map[uuid.UUID]bool{}
	f := TemplateFilter{Limit: 2}
	for pages := 0; ; pages++ {
		if pages > 5 {
			t.Fatal("pagination does not end")
		}
		page, err := s.ListTemplates(ctx, f)
		if err != nil {
			t.Fatal(err)
		}
		for _, tpl := range page.Items {
			if seen[tpl.ID] {
				t.Errorf("template %s returned twice", tpl.ID)
			}
			seen[tpl.ID] = true
		}
		if page.Next == nil {
			break
		}
		f.After = page.Next
	}
	if len(seen) != len(want) {
		t.Errorf("walked %d templates, want %d", len(seen), len(want))
	}

	page, err := s.ListTemplates(ctx, TemplateFilter{Channel: notification.ChannelEmail, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 2 {
		t.Errorf("email filter returned %d templates, want 2", len(page.Items))
	}
}

func TestNotificationKeepsItsTemplate(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	tpl, _, err := s.CreateTemplate(ctx, "shipped", notification.ChannelSMS, "Order {{order}} shipped")
	if err != nil {
		t.Fatal(err)
	}

	req := notification.Request{
		Channel: notification.ChannelSMS, Recipient: "+905551234567", Content: "Order 42 shipped",
		TemplateID: &tpl.ID,
	}.Normalized()
	plain := notification.Request{Channel: notification.ChannelSMS, Recipient: "+905551234567", Content: "plain"}.Normalized()
	res, err := s.Create(ctx, CreateParams{Requests: []notification.Request{req, plain}, Batch: true, MaxAttempts: 8})
	if err != nil {
		t.Fatal(err)
	}

	got, err := s.Get(ctx, res.Notifications[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.TemplateID == nil || *got.TemplateID != tpl.ID || got.Content != "Order 42 shipped" {
		t.Errorf("templated notification: template %v content %q", got.TemplateID, got.Content)
	}
	if res.Notifications[1].TemplateID != nil {
		t.Errorf("plain notification has template %v", res.Notifications[1].TemplateID)
	}

	// The foreign key catches a template ID the API layer failed to check.
	unknown := uuid.Must(uuid.NewV7())
	req.TemplateID = &unknown
	if _, err := s.Create(ctx, CreateParams{Requests: []notification.Request{req}, MaxAttempts: 8}); err == nil {
		t.Error("insert with an unknown template ID succeeded")
	}
}
