// Package s3 is a minimal S3 client (SigV4, standard library only) for remote backups.
// It works with AWS S3 and S3-compatible services such as Backblaze B2, Hetzner, Wasabi, Cloudflare R2 and MinIO.
package s3

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
)

// Config describes one bucket. Endpoint is the service URL, e.g. https://s3.eu-central-1.amazonaws.com.
type Config struct {
	Endpoint  string `json:"endpoint"`
	Region    string `json:"region"`
	Bucket    string `json:"bucket"`
	Prefix    string `json:"prefix,omitempty"` // key prefix inside the bucket, e.g. "cubeherd"
	AccessKey string `json:"accessKey"`
	SecretKey string `json:"secretKey"`
	PathStyle bool   `json:"pathStyle,omitempty"` // https://host/bucket/key instead of https://bucket.host/key
}

// Valid reports whether all fields needed to connect are set.
func (c Config) Valid() bool {
	return c.Endpoint != "" && c.Bucket != "" && c.AccessKey != "" && c.SecretKey != ""
}

type Object struct {
	Key      string
	Size     int64
	Modified time.Time
}

type Client struct {
	cfg  Config
	base *url.URL
	hc   *http.Client
	now  func() time.Time
}

const (
	unsignedPayload = "UNSIGNED-PAYLOAD"
	emptySHA        = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
)

// partSize is 8 MiB: at most 80 GB with S3's 10000 parts. The file is streamed, never held in memory.
var partSize int64 = 8 << 20

func New(cfg Config) (*Client, error) {
	if !cfg.Valid() {
		return nil, errors.New("incomplete S3 settings")
	}
	if !strings.Contains(cfg.Endpoint, "://") {
		cfg.Endpoint = "https://" + cfg.Endpoint
	}
	u, err := url.Parse(strings.TrimRight(cfg.Endpoint, "/"))
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, fmt.Errorf("invalid endpoint %q", cfg.Endpoint)
	}
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
	cfg.Prefix = strings.Trim(cfg.Prefix, "/")
	return &Client{cfg: cfg, base: u, now: time.Now, hc: &http.Client{Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 10 * time.Minute,
		IdleConnTimeout:       30 * time.Second,
	}}}, nil
}

// Key joins the configured prefix with the given path elements.
func (c *Client) Key(parts ...string) string {
	if c.cfg.Prefix != "" {
		parts = append([]string{c.cfg.Prefix}, parts...)
	}
	return strings.Join(parts, "/")
}

func (c *Client) objectURL(key string, q url.Values) *url.URL {
	u := *c.base
	if c.cfg.PathStyle {
		u.Path = strings.TrimRight(c.base.Path, "/") + "/" + c.cfg.Bucket + "/" + key
	} else {
		u.Host = c.cfg.Bucket + "." + c.base.Host
		u.Path = strings.TrimRight(c.base.Path, "/") + "/" + key
	}
	u.RawPath = escapePath(u.Path)
	u.RawQuery = canonicalQuery(q)
	return &u
}

func (c *Client) bucketURL(q url.Values) *url.URL {
	if c.cfg.PathStyle {
		u := c.objectURL("", q)
		u.Path = strings.TrimRight(u.Path, "/") + "/"
		u.RawPath = escapePath(u.Path)
		return u
	}
	u := c.objectURL("", q)
	u.Path, u.RawPath = "/", "/"
	return u
}

// escapePath percent-encodes a path the way SigV4 expects for S3 (each segment once, '/' kept).
func escapePath(p string) string {
	return strings.ReplaceAll(uriEncode(p), "%2F", "/")
}

func uriEncode(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case ch >= 'A' && ch <= 'Z', ch >= 'a' && ch <= 'z', ch >= '0' && ch <= '9', ch == '-', ch == '_', ch == '.', ch == '~':
			b.WriteByte(ch)
		default:
			fmt.Fprintf(&b, "%%%02X", ch)
		}
	}
	return b.String()
}

func canonicalQuery(q url.Values) string {
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		vs := append([]string(nil), q[k]...)
		sort.Strings(vs)
		for _, v := range vs {
			parts = append(parts, uriEncode(k)+"="+uriEncode(v))
		}
	}
	return strings.Join(parts, "&")
}

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

// sign adds the SigV4 Authorization header. It signs host and every x-amz-*, content-md5, content-type and range header.
func (c *Client) sign(req *http.Request, payloadHash string, t time.Time) {
	t = t.UTC()
	amzDate, day := t.Format("20060102T150405Z"), t.Format("20060102")
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)

	names := []string{"host"}
	vals := map[string]string{"host": req.URL.Host}
	for k, v := range req.Header {
		lk := strings.ToLower(k)
		if strings.HasPrefix(lk, "x-amz-") || lk == "content-md5" || lk == "content-type" || lk == "range" {
			names = append(names, lk)
			vals[lk] = strings.TrimSpace(strings.Join(v, ","))
		}
	}
	sort.Strings(names)
	var canonHeaders strings.Builder
	for _, n := range names {
		canonHeaders.WriteString(n + ":" + vals[n] + "\n")
	}
	signed := strings.Join(names, ";")
	canon := strings.Join([]string{req.Method, req.URL.EscapedPath(), req.URL.RawQuery, canonHeaders.String(), signed, payloadHash}, "\n")
	scope := day + "/" + c.cfg.Region + "/s3/aws4_request"
	sum := sha256.Sum256([]byte(canon))
	toSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hex.EncodeToString(sum[:])
	k := hmacSHA256([]byte("AWS4"+c.cfg.SecretKey), day)
	k = hmacSHA256(k, c.cfg.Region)
	k = hmacSHA256(k, "s3")
	k = hmacSHA256(k, "aws4_request")
	sig := hex.EncodeToString(hmacSHA256(k, toSign))
	req.Header.Set("Authorization", fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s", c.cfg.AccessKey, scope, signed, sig))
}

type apiError struct {
	Status  int
	Code    string
	Message string
}

func (e *apiError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("S3 %s: %s", e.Code, e.Message)
	}
	return fmt.Sprintf("S3 HTTP %d", e.Status)
}

// IsNotFound reports whether err is a missing object.
func IsNotFound(err error) bool {
	var ae *apiError
	return errors.As(err, &ae) && (ae.Status == http.StatusNotFound || ae.Code == "NoSuchKey")
}

func readError(resp *http.Response) error {
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	e := &apiError{Status: resp.StatusCode}
	var x struct{ Code, Message string }
	if xml.Unmarshal(b, &x) == nil {
		e.Code, e.Message = x.Code, x.Message
	}
	return e
}

func (c *Client) do(ctx context.Context, method string, u *url.URL, body io.Reader, size int64, payloadHash string, hdr http.Header) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, err
	}
	req.ContentLength = size
	for k, v := range hdr {
		req.Header[k] = v
	}
	c.sign(req, payloadHash, c.now())
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		return nil, readError(resp)
	}
	return resp, nil
}

func (c *Client) simple(ctx context.Context, method string, u *url.URL) (*http.Response, error) {
	return c.do(ctx, method, u, nil, 0, emptySHA, nil)
}

// retry runs fn up to three times for network errors and 5xx responses.
func retry(ctx context.Context, fn func() error) error {
	var err error
	for i := 0; i < 3; i++ {
		if err = fn(); err == nil {
			return nil
		}
		var ae *apiError
		if errors.As(err, &ae) && ae.Status < 500 {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(i+1) * 2 * time.Second):
		}
	}
	return err
}

// PutFile uploads a local file, as multipart upload when it is larger than one part.
func (c *Client) PutFile(ctx context.Context, key, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	size := info.Size()
	if size <= partSize {
		return retry(ctx, func() error {
			resp, err := c.do(ctx, http.MethodPut, c.objectURL(key, nil), io.NewSectionReader(f, 0, size), size, unsignedPayload, nil)
			if err != nil {
				return err
			}
			resp.Body.Close()
			return nil
		})
	}

	var up struct {
		UploadID string `xml:"UploadId"`
	}
	err = retry(ctx, func() error {
		resp, err := c.simple(ctx, http.MethodPost, c.objectURL(key, url.Values{"uploads": {""}}))
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		return xml.NewDecoder(resp.Body).Decode(&up)
	})
	if err != nil {
		return err
	}
	abort := func() {
		actx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if resp, err := c.simple(actx, http.MethodDelete, c.objectURL(key, url.Values{"uploadId": {up.UploadID}})); err == nil {
			resp.Body.Close()
		}
	}

	type part struct {
		XMLName xml.Name `xml:"Part"`
		Number  int      `xml:"PartNumber"`
		ETag    string   `xml:"ETag"`
	}
	var parts []part
	for off, n := int64(0), 1; off < size; off, n = off+partSize, n+1 {
		length := min(partSize, size-off)
		var etag string
		q := url.Values{"partNumber": {fmt.Sprint(n)}, "uploadId": {up.UploadID}}
		err := retry(ctx, func() error {
			resp, err := c.do(ctx, http.MethodPut, c.objectURL(key, q), io.NewSectionReader(f, off, length), length, unsignedPayload, nil)
			if err != nil {
				return err
			}
			resp.Body.Close()
			if etag = resp.Header.Get("ETag"); etag == "" {
				return errors.New("S3 returned no ETag for an uploaded part")
			}
			return nil
		})
		if err != nil {
			abort()
			return err
		}
		parts = append(parts, part{Number: n, ETag: etag})
	}

	body, err := xml.Marshal(struct {
		XMLName xml.Name `xml:"CompleteMultipartUpload"`
		Parts   []part
	}{Parts: parts})
	if err != nil {
		abort()
		return err
	}
	sum := sha256.Sum256(body)
	err = retry(ctx, func() error {
		resp, err := c.do(ctx, http.MethodPost, c.objectURL(key, url.Values{"uploadId": {up.UploadID}}),
			strings.NewReader(string(body)), int64(len(body)), hex.EncodeToString(sum[:]), nil)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		// CompleteMultipartUpload can answer 200 with an error document.
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		if strings.Contains(string(b), "<Error>") {
			var x struct{ Code, Message string }
			_ = xml.Unmarshal(b, &x)
			return &apiError{Status: 500, Code: x.Code, Message: x.Message}
		}
		return nil
	})
	if err != nil {
		abort()
	}
	return err
}

// List returns all objects whose key starts with prefix.
func (c *Client) List(ctx context.Context, prefix string) ([]Object, error) {
	return c.list(ctx, prefix, 0)
}

func (c *Client) list(ctx context.Context, prefix string, limit int) ([]Object, error) {
	var out []Object
	token := ""
	for {
		q := url.Values{"list-type": {"2"}, "prefix": {prefix}}
		if token != "" {
			q.Set("continuation-token", token)
		}
		if limit > 0 {
			q.Set("max-keys", fmt.Sprint(limit))
		}
		var res struct {
			Contents []struct {
				Key          string
				Size         int64
				LastModified time.Time
			}
			Truncated bool   `xml:"IsTruncated"`
			Next      string `xml:"NextContinuationToken"`
		}
		err := retry(ctx, func() error {
			resp, err := c.simple(ctx, http.MethodGet, c.bucketURL(q))
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			return xml.NewDecoder(resp.Body).Decode(&res)
		})
		if err != nil {
			return nil, err
		}
		for _, o := range res.Contents {
			out = append(out, Object{Key: o.Key, Size: o.Size, Modified: o.LastModified})
		}
		if !res.Truncated || res.Next == "" || limit > 0 {
			return out, nil
		}
		token = res.Next
	}
}

// Get streams an object; the caller closes the body.
func (c *Client) Get(ctx context.Context, key string) (io.ReadCloser, int64, error) {
	resp, err := c.simple(ctx, http.MethodGet, c.objectURL(key, nil))
	if err != nil {
		return nil, 0, err
	}
	return resp.Body, resp.ContentLength, nil
}

func (c *Client) Delete(ctx context.Context, key string) error {
	return retry(ctx, func() error {
		resp, err := c.simple(ctx, http.MethodDelete, c.objectURL(key, nil))
		if err != nil {
			return err
		}
		resp.Body.Close()
		return nil
	})
}

// Check verifies bucket access and credentials by listing a single key.
func (c *Client) Check(ctx context.Context) error {
	_, err := c.list(ctx, c.Key(""), 1)
	return err
}
