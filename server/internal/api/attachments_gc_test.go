package api

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/OpenALC/LiteTalk/server/internal/blob"
	"github.com/OpenALC/LiteTalk/server/internal/store"
)

type assertRowBeforePut struct {
	blob.Store
	st      store.Store
	checked int
}

func (b *assertRowBeforePut) Put(ctx context.Context, key string, data []byte, contentType string) error {
	exists, err := b.st.AttachmentKeyExists(ctx, key)
	if err != nil {
		return err
	}
	if !exists {
		return errors.New("对象写入前缺少元数据行")
	}
	b.checked++
	return b.Store.Put(ctx, key, data, contentType)
}

func TestAttachmentUploadReservesMetadataBeforeBlob(t *testing.T) {
	srv, mem, _ := newAttachmentTestServer(t, 1<<20)
	guard := &assertRowBeforePut{Store: srv.blob, st: mem}
	srv.blob = guard
	h := srv.Handler()
	access, _ := registerUser(t, h, mem, "beforeblob@ex.com", "beforeblob", "password123")
	rec := uploadAttachment(t, h, access, uploadPart{"file", "x.png", testPNG(t, 8, 8)})
	if rec.Code != http.StatusCreated || guard.checked == 0 {
		t.Fatalf("上传必须先写行: status=%d checked=%d body=%s", rec.Code, guard.checked, rec.Body.String())
	}
}
