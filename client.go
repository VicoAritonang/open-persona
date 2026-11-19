package openpersona

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ChatMode chooses how a hosted persona's Chat reaches the service.
type ChatMode string

const (
	// ChatAsync submits a job and polls it until it finishes (the default).
	// No connection is held open while the model writes, so it suits
	// serverless deployments and long answers such as full itineraries.
	ChatAsync ChatMode = "async"
	// ChatSync waits for the answer on one request.
	ChatSync ChatMode = "sync"
)

// ClientOptions configures a Client.
type ClientOptions struct {
	// BaseURL of the service (default: $OPENPERSONA_URL, else
	// http://localhost:8080).
	BaseURL string
	// StreamURL serves streamed answers, when streaming runs on a different
	// host than the rest of the API (default BaseURL).
	StreamURL string
	ChatMode  ChatMode
	// PollInterval is the first wait between job polls (default 1s; it grows
	// to 5s).
	PollInterval time.Duration
	HTTP         *http.Client // default: no overall timeout, bounded by ctx
}

// Client talks to the hosted OpenPersona service. It is safe for
// concurrent use.
type Client struct {
	key  string
	opt  ClientOptions
	http *http.Client
}

// NewClient returns a client authenticating with apiKey.
func NewClient(apiKey string, opt ClientOptions) *Client {
	if opt.BaseURL == "" {
		opt.BaseURL = os.Getenv("OPENPERSONA_URL")
	}
	if opt.BaseURL == "" {
		opt.BaseURL = "http://localhost:8080"
	}
	opt.BaseURL = strings.TrimRight(opt.BaseURL, "/")
	if opt.StreamURL == "" {
		opt.StreamURL = opt.BaseURL
	}
	opt.StreamURL = strings.TrimRight(opt.StreamURL, "/")
	if opt.ChatMode == "" {
		opt.ChatMode = ChatAsync
	}
	if opt.PollInterval <= 0 {
		opt.PollInterval = time.Second
	}
	hc := opt.HTTP
	if hc == nil {
		hc = &http.Client{}
	}
	return &Client{key: apiKey, opt: opt, http: hc}
}

// APIError is an error answer from the service.
type APIError struct {
	Status int
	// Code classifies the error: "invalid_argument", "unauthenticated",
	// "permission_denied", "not_found", "unavailable", "unimplemented",
	// "upstream" or "internal" (see api/openapi.yaml). Empty when the
	// answer did not come from an OpenPersona server (a proxy, say).
	Code    string
	Message string
}

func (e *APIError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("openpersona: HTTP %d %s: %s", e.Status, e.Code, e.Message)
	}
	return fmt.Sprintf("openpersona: HTTP %d: %s", e.Status, e.Message)
}

// IsNotFound reports whether err is a 404 from the service.
func IsNotFound(err error) bool {
	var e *APIError
	return errors.As(err, &e) && e.Status == http.StatusNotFound
}

func (c *Client) newRequest(ctx context.Context, method, base, path string, body any) (*http.Request, error) {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, r)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("User-Agent", "openpersona-go")
	return req, nil
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	req, err := c.newRequest(ctx, method, c.opt.BaseURL, path, body)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := checkStatus(resp); err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func checkStatus(resp *http.Response) error {
	if resp.StatusCode < 300 {
		return nil
	}
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var e struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	if json.Unmarshal(data, &e) != nil || e.Error == "" {
		e.Error = strings.TrimSpace(string(data))
	}
	return &APIError{Status: resp.StatusCode, Code: e.Code, Message: e.Error}
}

// List returns the personas this API key may use.
func (c *Client) List(ctx context.Context) ([]Info, error) {
	var out struct {
		Personas []Info `json:"personas"`
	}
	err := c.do(ctx, http.MethodGet, "/v1/personas", nil, &out)
	return out.Personas, err
}

// Persona returns a hosted persona. No request is made until it is used.
func (c *Client) Persona(id string) *Remote { return &Remote{c: c, id: id} }

// Remote is a persona hosted by the service.
type Remote struct {
	c  *Client
	id string
}

var _ Persona = (*Remote)(nil)

func (r *Remote) path(suffix string) string { return "/v1/personas/" + url.PathEscape(r.id) + suffix }

// ID implements Persona.
func (r *Remote) ID() string { return r.id }

// Info implements Persona.
func (r *Remote) Info(ctx context.Context) (*Info, error) {
	var m struct {
		Spec Info `json:"spec"`
	}
	if err := r.c.do(ctx, http.MethodGet, r.path(""), nil, &m); err != nil {
		return nil, err
	}
	return &m.Spec, nil
}

// Knowledge implements Persona.
func (r *Remote) Knowledge(ctx context.Context, query string, opt KnowledgeOptions) (*Knowledge, error) {
	var k Knowledge
	err := r.c.do(ctx, http.MethodPost, r.path("/knowledge"), map[string]any{"query": query, "units": opt.Units, "sources": opt.Sources}, &k)
	return &k, err
}

type chatBody struct {
	Messages  []Message `json:"messages"`
	Context   string    `json:"context,omitempty"`
	MaxTokens int       `json:"max_tokens,omitempty"`
	Stream    bool      `json:"stream,omitempty"`
}

// Chat implements Persona. With ChatAsync (the default) it submits a job
// and waits for it; with ChatSync it waits on one request.
func (r *Remote) Chat(ctx context.Context, messages []Message, opt ChatOptions) (*Reply, error) {
	if r.c.opt.ChatMode == ChatSync {
		var reply Reply
		err := r.c.do(ctx, http.MethodPost, r.path("/chat"), chatBody{Messages: messages, Context: opt.Context, MaxTokens: opt.MaxTokens}, &reply)
		return &reply, err
	}
	job, err := r.Submit(ctx, messages, opt)
	if err != nil {
		return nil, err
	}
	return r.c.Wait(ctx, job.ID)
}

// Submit starts an async answer and returns at once. Poll it with
// Client.Job or block on Client.Wait.
func (r *Remote) Submit(ctx context.Context, messages []Message, opt ChatOptions) (*Job, error) {
	var j Job
	err := r.c.do(ctx, http.MethodPost, r.path("/jobs"), chatBody{Messages: messages, Context: opt.Context, MaxTokens: opt.MaxTokens}, &j)
	return &j, err
}

// Stream implements Persona over server-sent events, from StreamURL.
func (r *Remote) Stream(ctx context.Context, messages []Message, opt ChatOptions, onDelta func(string) error) (*Reply, error) {
	req, err := r.c.newRequest(ctx, http.MethodPost, r.c.opt.StreamURL, r.path("/chat"), chatBody{Messages: messages, Context: opt.Context, MaxTokens: opt.MaxTokens, Stream: true})
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := r.c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if err := checkStatus(resp); err != nil {
		return nil, err
	}
	return readEvents(resp.Body, onDelta)
}

func readEvents(body io.Reader, onDelta func(string) error) (*Reply, error) {
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 64<<10), 8<<20)
	var event string
	var data strings.Builder
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		case line == "":
			payload := data.String()
			data.Reset()
			switch event {
			case "delta":
				var d struct {
					Text string `json:"text"`
				}
				if json.Unmarshal([]byte(payload), &d) == nil && onDelta != nil {
					if err := onDelta(d.Text); err != nil {
						return nil, err
					}
				}
			case "done":
				var reply Reply
				if err := json.Unmarshal([]byte(payload), &reply); err != nil {
					return nil, fmt.Errorf("openpersona: bad final event: %w", err)
				}
				return &reply, nil
			case "error":
				var e struct {
					Error string `json:"error"`
					Code  string `json:"code"`
				}
				json.Unmarshal([]byte(payload), &e)
				return nil, &APIError{Status: http.StatusBadGateway, Code: e.Code, Message: e.Error}
			}
			event = ""
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return nil, errors.New("openpersona: stream ended before the answer was complete")
}

// Close implements Persona; a hosted persona holds nothing.
func (r *Remote) Close() error { return nil }

// Job is an async answer.
type Job struct {
	ID         string          `json:"id"`
	Persona    string          `json:"persona"`
	Status     string          `json:"status"` // queued, running, succeeded, failed
	Error      string          `json:"error,omitempty"`
	Result     json.RawMessage `json:"result,omitempty"`
	CreatedAt  time.Time       `json:"created_at"`
	FinishedAt *time.Time      `json:"finished_at,omitempty"`
}

// Done reports whether the job has finished.
func (j *Job) Done() bool { return j.Status == "succeeded" || j.Status == "failed" }

// Reply decodes a finished job's answer.
func (j *Job) Reply() (*Reply, error) {
	switch j.Status {
	case "succeeded":
		var r Reply
		if err := json.Unmarshal(j.Result, &r); err != nil {
			return nil, fmt.Errorf("openpersona: bad job result: %w", err)
		}
		return &r, nil
	case "failed":
		return nil, fmt.Errorf("openpersona: job %s failed: %s", j.ID, j.Error)
	}
	return nil, fmt.Errorf("openpersona: job %s is %s", j.ID, j.Status)
}

// Job fetches a job's current state.
func (c *Client) Job(ctx context.Context, id string) (*Job, error) {
	var j Job
	err := c.do(ctx, http.MethodGet, "/v1/jobs/"+url.PathEscape(id), nil, &j)
	return &j, err
}

// Wait polls a job until it finishes or ctx ends.
func (c *Client) Wait(ctx context.Context, id string) (*Reply, error) {
	wait := c.opt.PollInterval
	for {
		j, err := c.Job(ctx, id)
		if err != nil {
			return nil, err
		}
		if j.Done() {
			return j.Reply()
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
		wait = min(wait*3/2, 5*time.Second)
	}
}

// DownloadChunk is the size of one ranged request of Download. 3 MiB keeps
// each response under AWS Lambda's 6 MB payload limit after base64
// encoding.
var DownloadChunk int64 = 3 << 20

// PackExt is the file extension of a persona pack.
const PackExt = ".persona"

// Download fetches the current version of a persona's pack into dir and
// returns its path (dir/<id>@<version>.persona). The API key needs the
// download scope. A file already there with the right SHA-256 is not
// downloaded again. The pack's hash and file header are verified before it
// is kept; open it with package local.
func (c *Client) Download(ctx context.Context, id, dir string) (string, error) {
	first, size, sum, version, err := c.packRange(ctx, id, 0)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, id+"@"+version+PackExt)
	if have, err := fileSHA(path); err == nil && have == sum {
		return path, nil
	}
	tmp := path + ".download"
	f, err := os.Create(tmp)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	w := io.MultiWriter(f, h)
	_, err = w.Write(first)
	for off := int64(len(first)); err == nil && off < size; {
		var chunk []byte
		var s string
		chunk, _, s, _, err = c.packRange(ctx, id, off)
		if err == nil && s != sum {
			err = fmt.Errorf("openpersona: %s changed during download; retry", id)
		}
		if err == nil {
			_, err = w.Write(chunk)
			off += int64(len(chunk))
		}
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && hex.EncodeToString(h.Sum(nil)) != sum {
		err = fmt.Errorf("openpersona: %s: downloaded bytes do not match sha256 %s", id, sum)
	}
	if err == nil {
		if err = checkPackHeader(tmp); err == nil {
			err = os.Rename(tmp, path)
		}
	}
	if err != nil {
		os.Remove(tmp)
		return "", err
	}
	return path, nil
}

func (c *Client) packRange(ctx context.Context, id string, off int64) (data []byte, size int64, sum, version string, err error) {
	req, err := c.newRequest(ctx, http.MethodGet, c.opt.BaseURL, "/v1/personas/"+url.PathEscape(id)+"/pack", nil)
	if err != nil {
		return nil, 0, "", "", err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", off, off+DownloadChunk-1))
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, "", "", err
	}
	defer resp.Body.Close()
	if err := checkStatus(resp); err != nil {
		return nil, 0, "", "", err
	}
	sum, version = resp.Header.Get("X-Pack-SHA256"), resp.Header.Get("X-Persona-Version")
	if sum == "" || version == "" {
		return nil, 0, "", "", errors.New("openpersona: the service did not identify the pack")
	}
	size = resp.ContentLength
	if cr := resp.Header.Get("Content-Range"); cr != "" { // bytes a-b/size
		if i := strings.LastIndex(cr, "/"); i >= 0 {
			size, _ = strconv.ParseInt(cr[i+1:], 10, 64)
		}
	}
	data, err = io.ReadAll(io.LimitReader(resp.Body, DownloadChunk+1))
	if err == nil && len(data) == 0 && size > 0 {
		err = errors.New("openpersona: empty pack chunk")
	}
	return data, size, sum, version, err
}

func fileSHA(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// A pack is an SQLite database whose header carries the pack application
// id (offset 68) and format version (offset 60), both big-endian.
const (
	packApplicationID = 0x4F504E50 // "OPNP"
	packFormat        = 1          // the newest format this library reads
)

// checkPackHeader verifies that path is a persona pack this library can
// read, without opening it as a database.
func checkPackHeader(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	var h [100]byte
	if _, err := io.ReadFull(f, h[:]); err != nil || string(h[:15]) != "SQLite format 3" || h[15] != 0 {
		return fmt.Errorf("openpersona: %s is not a persona pack", filepath.Base(path))
	}
	be := func(b []byte) uint32 { return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3]) }
	if app := be(h[68:72]); app != packApplicationID && app != 0 {
		return fmt.Errorf("openpersona: %s is not a persona pack (application id %#x)", filepath.Base(path), app)
	}
	if format := be(h[60:64]); format > packFormat {
		return fmt.Errorf("openpersona: %s has pack format %d; this library reads up to %d, upgrade it", filepath.Base(path), format, packFormat)
	}
	return nil
}
