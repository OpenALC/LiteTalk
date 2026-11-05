package blob

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

func TestS3ScanPagesAndStat(t *testing.T) {
	owner := strings.Repeat("a", 64)
	keys := []string{"1901.bin", "1902.bin", "1903.thumb.jpg"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.Header().Set("Last-Modified", "Wed, 05 Nov 2025 00:00:00 GMT")
			w.Header().Set("ETag", `"etag"`)
			w.Header().Set("X-Amz-Meta-Litetalk-Attachment", owner)
			w.Header().Set("Content-Length", "1")
			return
		}
		if r.Method != http.MethodGet || r.URL.Query().Get("list-type") != "2" {
			t.Errorf("意外请求 %s %s", r.Method, r.URL)
			w.WriteHeader(400)
			return
		}
		after := r.URL.Query().Get("start-after")
		var objects strings.Builder
		for _, key := range keys {
			if key <= after {
				continue
			}
			fmt.Fprintf(&objects, "<Contents><Key>%s</Key><LastModified>2025-11-05T00:00:00.000Z</LastModified><ETag>\"etag\"</ETag><Size>1</Size><StorageClass>STANDARD</StorageClass></Contents>", key)
		}
		w.Header().Set("Content-Type", "application/xml")
		fmt.Fprintf(w, "<?xml version=\"1.0\" encoding=\"UTF-8\"?><ListBucketResult><Name>test</Name><Prefix></Prefix><KeyCount>3</KeyCount><MaxKeys>3</MaxKeys><IsTruncated>false</IsTruncated>%s</ListBucketResult>", objects.String())
	}))
	defer srv.Close()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	st, _, err := newS3(Config{Endpoint: u.Host, Bucket: "test", AccessKey: "test", SecretKey: "test", Region: "us-east-1", PathStyle: true, OwnerID: owner}, nil)
	if err != nil {
		t.Fatal(err)
	}
	s := st.(*s3Store)
	first, next, done, err := s.Scan(context.Background(), "", 2)
	if err != nil || done || len(first) != 2 || next != "1902.bin" {
		t.Fatalf("首批: %+v %q %v %v", first, next, done, err)
	}
	second, _, done, err := s.Scan(context.Background(), next, 2)
	if err != nil || !done || len(second) != 1 || second[0].Key != "1903.thumb.jpg" {
		t.Fatalf("次批: %+v %v %v", second, done, err)
	}
	info, err := s.Stat(context.Background(), "1901.bin")
	if err != nil || info.Key != "1901.bin" || info.Size != 1 || info.ETag == "" || !info.Owned {
		t.Fatalf("Stat: %+v %v", info, err)
	}
}

func TestS3PutMarksOwnership(t *testing.T) {
	owner := strings.Repeat("a", 64)
	var marked atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Errorf("意外方法 %s", r.Method)
			w.WriteHeader(400)
			return
		}
		marked.Store(r.Header.Get("X-Amz-Meta-Litetalk-Attachment") == owner)
		w.Header().Set("ETag", `"etag"`)
	}))
	defer srv.Close()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	st, _, err := newS3(Config{Endpoint: u.Host, Bucket: "test", AccessKey: "test", SecretKey: "test", Region: "us-east-1", PathStyle: true, OwnerID: owner}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Put(context.Background(), "1904.bin", []byte("x"), "application/octet-stream"); err != nil {
		t.Fatal(err)
	}
	if !marked.Load() {
		t.Fatal("S3 写入缺少归属标记")
	}
}

func TestS3SharedBucketOwnerIsolation(t *testing.T) {
	ownerA, ownerB := strings.Repeat("a", 64), strings.Repeat("b", 64)
	var marker atomic.Value
	marker.Store(ownerA)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Last-Modified", "Wed, 05 Nov 2025 00:00:00 GMT")
		w.Header().Set("Content-Length", "1")
		w.Header().Set("ETag", `"etag"`)
		w.Header().Set("X-Amz-Meta-Litetalk-Attachment", marker.Load().(string))
	}))
	defer srv.Close()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	config := Config{Endpoint: u.Host, Bucket: "shared", AccessKey: "test", SecretKey: "test", Region: "us-east-1", PathStyle: true, OwnerID: ownerA}
	aStore, _, err := newS3(config, nil)
	if err != nil {
		t.Fatal(err)
	}
	config.OwnerID = ownerB
	bStore, _, err := newS3(config, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		marker     string
		ownA, ownB bool
	}{{ownerA, true, false}, {ownerB, false, true}, {"1", false, false}, {"", false, false}} {
		marker.Store(tc.marker)
		a, err := aStore.(*s3Store).Stat(context.Background(), "2001.bin")
		if err != nil {
			t.Fatal(err)
		}
		b, err := bStore.(*s3Store).Stat(context.Background(), "2001.bin")
		if err != nil {
			t.Fatal(err)
		}
		if a.Owned != tc.ownA || b.Owned != tc.ownB {
			t.Fatalf("标记 %q 归属错误: A=%v B=%v", tc.marker, a.Owned, b.Owned)
		}
	}
}
