package firebase

import (
	"context"
	"time"

	"cloud.google.com/go/firestore"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ── Types ─────────────────────────────────────────────────────────────────────

type UserProfile struct {
	UID       string                 `firestore:"uid"        json:"uid"`
	Email     string                 `firestore:"email"      json:"email"`
	Plan      string                 `firestore:"plan"       json:"plan"` // free | pro | enterprise
	CreatedAt time.Time              `firestore:"created_at" json:"created_at"`
	Settings  map[string]interface{} `firestore:"settings"   json:"settings"`
}

type ChatRecord struct {
	ID        string        `firestore:"id"         json:"id"`
	Title     string        `firestore:"title"      json:"title"`
	Model     string        `firestore:"model"      json:"model"`
	Messages  []ChatMessage `firestore:"messages"   json:"messages"`
	CreatedAt time.Time     `firestore:"created_at" json:"created_at"`
	UpdatedAt time.Time     `firestore:"updated_at" json:"updated_at"`
}

type ChatMessage struct {
	Role      string    `firestore:"role"      json:"role"`
	Content   string    `firestore:"content"   json:"content"`
	Timestamp time.Time `firestore:"timestamp" json:"timestamp"`
}

// ── Users ─────────────────────────────────────────────────────────────────────

// GetOrCreateUser fetches the user profile from Firestore, creating it if it doesn't exist.
// Only treats a true "not found" response as a creation trigger; all other errors propagate.
func GetOrCreateUser(ctx context.Context, uid, email string) (*UserProfile, error) {
	doc := fbStore.Collection("users").Doc(uid)
	snap, err := doc.Get(ctx)
	if err != nil {
		if status.Code(err) != codes.NotFound {
			return nil, err // network/permission error — don't create a phantom user
		}
		profile := &UserProfile{
			UID:       uid,
			Email:     email,
			Plan:      "free",
			CreatedAt: time.Now(),
			Settings:  map[string]interface{}{},
		}
		if _, err2 := doc.Set(ctx, profile); err2 != nil {
			return nil, err2
		}
		return profile, nil
	}
	var p UserProfile
	if err := snap.DataTo(&p); err != nil {
		return nil, err
	}
	return &p, nil
}

func GetUserProfile(ctx context.Context, uid string) (*UserProfile, error) {
	snap, err := fbStore.Collection("users").Doc(uid).Get(ctx)
	if err != nil {
		return nil, err
	}
	var p UserProfile
	snap.DataTo(&p)
	return &p, nil
}

func UpdateUserSettings(ctx context.Context, uid string, settings map[string]interface{}) error {
	_, err := fbStore.Collection("users").Doc(uid).Update(ctx, []firestore.Update{
		{Path: "settings", Value: settings},
	})
	return err
}

func UpdateUserPlan(ctx context.Context, uid, plan string) error {
	_, err := fbStore.Collection("users").Doc(uid).Update(ctx, []firestore.Update{
		{Path: "plan", Value: plan},
	})
	return err
}

// ── Chats ─────────────────────────────────────────────────────────────────────

func ListChats(ctx context.Context, uid string) ([]ChatRecord, error) {
	iter := fbStore.Collection("users").Doc(uid).Collection("chats").
		OrderBy("updated_at", firestore.Desc).Limit(200).Documents(ctx)
	defer iter.Stop()
	var chats []ChatRecord
	for {
		doc, err := iter.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			// A real failure (permissions, network) must not masquerade as
			// "you have no saved chats".
			return nil, err
		}
		var c ChatRecord
		doc.DataTo(&c)
		c.ID = doc.Ref.ID
		chats = append(chats, c)
	}
	return chats, nil
}

// SaveChat writes a chat and returns its id. When chat.ID names an existing
// document the chat is updated in place; otherwise a new one is created. Saving
// the same conversation twice must not leave two copies behind.
func SaveChat(ctx context.Context, uid string, chat ChatRecord) (string, error) {
	now := time.Now()
	col := fbStore.Collection("users").Doc(uid).Collection("chats")

	if chat.ID != "" {
		ref := col.Doc(chat.ID)
		if snap, err := ref.Get(ctx); err == nil {
			var prev ChatRecord
			if snap.DataTo(&prev) == nil && !prev.CreatedAt.IsZero() {
				chat.CreatedAt = prev.CreatedAt
			} else {
				chat.CreatedAt = now
			}
			chat.UpdatedAt = now
			if _, err := ref.Set(ctx, chat); err != nil {
				return "", err
			}
			return ref.ID, nil
		} else if status.Code(err) != codes.NotFound {
			return "", err
		}
		// Not found — fall through and create a fresh document.
	}

	ref := col.NewDoc()
	chat.ID = ref.ID
	chat.CreatedAt = now
	chat.UpdatedAt = now
	_, err := ref.Set(ctx, chat)
	return ref.ID, err
}

func UpdateChat(ctx context.Context, uid, chatID string, messages []ChatMessage) error {
	_, err := fbStore.Collection("users").Doc(uid).Collection("chats").Doc(chatID).Update(ctx,
		[]firestore.Update{
			{Path: "messages", Value: messages},
			{Path: "updated_at", Value: time.Now()},
		})
	return err
}

func DeleteChat(ctx context.Context, uid, chatID string) error {
	_, err := fbStore.Collection("users").Doc(uid).Collection("chats").Doc(chatID).Delete(ctx)
	return err
}
