package s3

import (
	"context"
	"crypto/rand"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// The worked example from the AWS documentation ("GET Object" with a Range header).
func TestSignatureVector(t *testing.T) {
	c := &Client{cfg: Config{Region: "us-east-1", AccessKey: "AKIAIOSFODNN7EXAMPLE", SecretKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"}}
	req, _ := http.NewRequest("GET", "https://examplebucket.s3.amazonaws.com/test.txt", nil)
	req.Header.Set("Range", "bytes=0-9")
	c.sign(req, emptySHA, time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC))
	want := "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request, " +
		"SignedHeaders=host;range;x-amz-content-sha256;x-amz-date, " +
		"Signature=f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41"
	if got := req.Header.Get("Authorization"); got != want {
		t.Fatalf("authorization\n got %s\nwant %s", got, want)
	}
}

// fakeS3 is a small in-memory path-style S3 server.
type fakeS3 struct {
	mu      sync.Mutex
	objs    map[string][]byte
	uploads map[string]map[int][]byte
	auth    int
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 ") {
		f.auth++
	}
	key := strings.TrimPrefix(r.URL.Path, "/bkt/")
	q := r.URL.Query()
	switch {
	case r.Method == "GET" && q.Get("list-type") == "2":
		var keys []string
		for k := range f.objs {
			if strings.HasPrefix(k, q.Get("prefix")) {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		fmt.Fprint(w, `<ListBucketResult><IsTruncated>false</IsTruncated>`)
		for _, k := range keys {
			fmt.Fprintf(w, `<Contents><Key>%s</Key><Size>%d</Size><LastModified>2026-01-02T03:04:05.000Z</LastModified></Contents>`, k, len(f.objs[k]))
		}
		fmt.Fprint(w, `</ListBucketResult>`)
	case r.Method == "POST" && q.Has("uploads"):
		id := "up1"
		f.uploads[id] = map[int][]byte{}
		fmt.Fprintf(w, `<InitiateMultipartUploadResult><UploadId>%s</UploadId></InitiateMultipartUploadResult>`, id)
	case r.Method == "PUT" && q.Has("partNumber"):
		var n int
		fmt.Sscan(q.Get("partNumber"), &n)
		b, _ := io.ReadAll(r.Body)
		f.uploads[q.Get("uploadId")][n] = b
		w.Header().Set("ETag", fmt.Sprintf(`"etag%d"`, n))
	case r.Method == "POST" && q.Has("uploadId"):
		var req struct {
			Parts []struct {
				Number int `xml:"PartNumber"`
				ETag   string
			} `xml:"Part"`
		}
		b, _ := io.ReadAll(r.Body)
		if err := xml.Unmarshal(b, &req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		var all []byte
		for _, p := range req.Parts {
			all = append(all, f.uploads[q.Get("uploadId")][p.Number]...)
		}
		f.objs[key] = all
		fmt.Fprint(w, `<CompleteMultipartUploadResult/>`)
	case r.Method == "PUT":
		b, _ := io.ReadAll(r.Body)
		f.objs[key] = b
	case r.Method == "GET":
		b, ok := f.objs[key]
		if !ok {
			w.WriteHeader(404)
			fmt.Fprint(w, `<Error><Code>NoSuchKey</Code><Message>missing</Message></Error>`)
			return
		}
		w.Write(b)
	case r.Method == "DELETE":
		delete(f.objs, key)
		w.WriteHeader(204)
	default:
		http.Error(w, "unsupported", 400)
	}
}

func newTestClient(t *testing.T) (*Client, *fakeS3) {
	f := &fakeS3{objs: map[string][]byte{}, uploads: map[string]map[int][]byte{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	c, err := New(Config{Endpoint: srv.URL, Bucket: "bkt", AccessKey: "a", SecretKey: "s", Prefix: "/cubeherd/", PathStyle: true})
	if err != nil {
		t.Fatal(err)
	}
	return c, f
}

func TestRoundTrip(t *testing.T) {
	old := partSize
	partSize = 5 << 10
	t.Cleanup(func() { partSize = old })
	c, f := newTestClient(t)
	ctx := context.Background()

	for name, n := range map[string]int{"small.bin": 100, "large.bin": 5<<10*3 + 17} {
		data := make([]byte, n)
		rand.Read(data)
		p := filepath.Join(t.TempDir(), name)
		os.WriteFile(p, data, 0o600)
		key := c.Key("srv", name)
		if err := c.PutFile(ctx, key, p); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		rc, _, err := c.Get(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := io.ReadAll(rc)
		rc.Close()
		if string(got) != string(data) {
			t.Fatalf("%s: content mismatch (%d bytes)", name, len(got))
		}
	}
	list, err := c.List(ctx, c.Key("srv")+"/")
	if err != nil || len(list) != 2 || list[0].Key != "cubeherd/srv/large.bin" || list[1].Size != 100 {
		t.Fatalf("list: %v %+v", err, list)
	}
	if err := c.Delete(ctx, c.Key("srv", "small.bin")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.Get(ctx, c.Key("srv", "small.bin")); !IsNotFound(err) {
		t.Fatalf("want not found, got %v", err)
	}
	if err := c.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if f.auth == 0 {
		t.Fatal("requests were not signed")
	}
}

func TestURLs(t *testing.T) {
	v, _ := New(Config{Endpoint: "s3.example.com", Bucket: "b", AccessKey: "a", SecretKey: "s"})
	if got := v.objectURL("p/a b.tar.zst", url.Values{"x": {"1 2"}}).String(); got != "https://b.s3.example.com/p/a%20b.tar.zst?x=1%202" {
		t.Fatal(got)
	}
	p, _ := New(Config{Endpoint: "https://s3.example.com/", Bucket: "b", AccessKey: "a", SecretKey: "s", PathStyle: true})
	if got := p.objectURL("k", nil).String(); got != "https://s3.example.com/b/k" {
		t.Fatal(got)
	}
	if got := p.bucketURL(url.Values{"list-type": {"2"}}).String(); got != "https://s3.example.com/b/?list-type=2" {
		t.Fatal(got)
	}
	if _, err := New(Config{Endpoint: "ftp://x", Bucket: "b", AccessKey: "a", SecretKey: "s"}); err == nil {
		t.Fatal("want an error for ftp endpoints")
	}
}
