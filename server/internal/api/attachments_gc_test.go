package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/OpenALC/LiteTalk/server/internal/blob"
	"github.com/OpenALC/LiteTalk/server/internal/store"
)

type assertUnclaimableDuringPut struct {
	blob.Store
	st        store.Store
	userID    int64
	channelID int64
	checked   int
}

func (b *assertUnclaimableDuringPut) Put(ctx context.Context, key string, data []byte, contentType string) error {
	if err := b.Store.Put(ctx, key, data, contentType); err != nil {
		return err
	}
	exists, err := b.st.AttachmentKeyExists(ctx, key)
	if err != nil {
		return err
	}
	if exists {
		return errors.New("未完成上传已暴露元数据行")
	}
	if strings.HasSuffix(key, ".bin") {
		id, err := strconv.ParseInt(strings.TrimSuffix(key, ".bin"), 10, 64)
		if err != nil {
			return err
		}
		msg := &store.Message{ChannelID: b.channelID, UserID: b.userID, Content: "x", AttachmentIDs: []int64{id}}
		if err := b.st.CreateMessage(ctx, msg); !errors.Is(err, store.ErrAttachmentUnavailable) {
			return fmt.Errorf("未完成上传可认领: %v", err)
		}
	}
	b.checked++
	return nil
}

func TestAttachmentUploadKeepsUnfinishedObjectUnclaimable(t *testing.T) {
	srv, mem, _ := newAttachmentTestServer(t, 1<<20)
	h := srv.Handler()
	access, _ := registerUser(t, h, mem, "beforeblob@ex.com", "beforeblob", "password123")
	u, err := mem.UserByEmail(context.Background(), "beforeblob@ex.com")
	if err != nil {
		t.Fatal(err)
	}
	sv := &store.Server{Name: "test", OwnerID: u.ID, IconName: "hash", IconColor: "#4FA4F3"}
	if err := mem.CreateServer(context.Background(), sv); err != nil {
		t.Fatal(err)
	}
	ch := &store.Channel{ServerID: sv.ID, Name: "test", Type: store.ChannelText}
	if err := mem.CreateChannel(context.Background(), ch); err != nil {
		t.Fatal(err)
	}
	guard := &assertUnclaimableDuringPut{Store: srv.blob, st: mem, userID: u.ID, channelID: ch.ID}
	srv.blob = guard
	rec := uploadAttachment(t, h, access, uploadPart{"file", "x.png", testPNG(t, 8, 8)})
	if rec.Code != http.StatusCreated || guard.checked == 0 {
		t.Fatalf("未完成上传不可认领: status=%d checked=%d body=%s", rec.Code, guard.checked, rec.Body.String())
	}
}
