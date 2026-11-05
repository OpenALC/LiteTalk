package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
)

const blobOwnerKVKey = "attachment:blob-owner:v1"

func newBlobOwnerID() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}

func validBlobOwnerID(id string) bool {
	if len(id) != 64 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}

func (m *Mem) BlobOwnerID(_ context.Context) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.blobOwnerID == "" {
		id, err := newBlobOwnerID()
		if err != nil {
			return "", err
		}
		m.blobOwnerID = id
	}
	return m.blobOwnerID, nil
}

func (s *SQLite) BlobOwnerID(ctx context.Context) (string, error) {
	return dbBlobOwnerID(ctx,
		func(candidate string) error {
			_, err := s.db.ExecContext(ctx, `INSERT INTO kv(kv_key,kv_value,expires_at) VALUES(?,?,NULL) ON CONFLICT(kv_key) DO NOTHING`, blobOwnerKVKey, candidate)
			return err
		},
		func() (string, error) {
			var id string
			err := s.db.QueryRowContext(ctx, `SELECT kv_value FROM kv WHERE kv_key = ? AND expires_at IS NULL`, blobOwnerKVKey).Scan(&id)
			return id, err
		})
}

func (m *MySQL) BlobOwnerID(ctx context.Context) (string, error) {
	return dbBlobOwnerID(ctx,
		func(candidate string) error {
			_, err := m.db.ExecContext(ctx, `INSERT INTO kv(kv_key,kv_value,expires_at) VALUES(?,?,NULL) ON DUPLICATE KEY UPDATE kv_key = kv_key`, blobOwnerKVKey, candidate)
			return err
		},
		func() (string, error) {
			var id string
			err := m.db.QueryRowContext(ctx, `SELECT kv_value FROM kv WHERE kv_key = ? AND expires_at IS NULL`, blobOwnerKVKey).Scan(&id)
			return id, err
		})
}

func dbBlobOwnerID(ctx context.Context, insert func(string) error, read func() (string, error)) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	candidate, err := newBlobOwnerID()
	if err != nil {
		return "", err
	}
	if err := insert(candidate); err != nil {
		return "", fmt.Errorf("附件归属初始化失败: %w", err)
	}
	id, err := read()
	if err != nil {
		return "", fmt.Errorf("附件归属读取失败: %w", err)
	}
	if !validBlobOwnerID(id) {
		return "", errors.New("附件归属值无效")
	}
	return id, nil
}
