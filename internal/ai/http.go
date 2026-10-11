package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"time"
)

// no overall timeout: a long agent turn can stream for minutes. The
// context cancels it.
var client = &http.Client{}

// post sends a JSON body and returns the response once the status is
// 200, retrying rate limits, overload and server errors a few times
// before anything has streamed.
func post(ctx context.Context, url string, headers map[string]string, body any) (*http.Response, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	wait := time.Second
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(b))
		if err != nil {
			return nil, err
		}
		req.Header.Set("content-type", "application/json")
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusOK {
			return resp, nil
		}
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		resp.Body.Close()
		apiErr := &APIError{Status: resp.StatusCode, Msg: errorMessage(msg)}
		retry := resp.StatusCode == 429 || resp.StatusCode == 529 || resp.StatusCode >= 500
		if !retry || attempt >= 3 {
			return nil, apiErr
		}
		d := wait
		if s, err := strconv.Atoi(resp.Header.Get("retry-after")); err == nil && s > 0 {
			d = time.Duration(s) * time.Second
		}
		select {
		case <-time.After(d):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		wait *= 2
	}
}

// errorMessage digs the message out of {"error": {"message": ...}},
// which both APIs use, and falls back to the raw body.
func errorMessage(body []byte) string {
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &e) == nil && e.Error.Message != "" {
		return e.Error.Message
	}
	s := string(bytes.TrimSpace(body))
	if len(s) > 300 {
		s = s[:300] + "..."
	}
	return s
}
