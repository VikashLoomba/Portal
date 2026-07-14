package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"iter"
	"net/http"
	"time"

	"github.com/VikashLoomba/Portal/pkg/api"
)

// ReadyPollInterval controls how often WaitReady probes the daemon after a
// failed availability check. It is exposed so tests can shrink the interval.
var ReadyPollInterval = 50 * time.Millisecond

// Setup starts POST /v1/setup and returns its ndjson event stream. Setup uses
// the caller's raw context because the operation is long-running. Callers must
// consume the iterator or cancel ctx so the response body can be closed.
func (c *Client) Setup(ctx context.Context, req api.SetupRequest) (iter.Seq2[api.SetupEvent, error], error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://unix/v1/setup", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := c.do(httpReq)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return nil, apiError(resp)
	}

	return func(yield func(api.SetupEvent, error) bool) {
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64*1024), eventsBufCap)
		for sc.Scan() {
			var ev api.SetupEvent
			if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
				yield(api.SetupEvent{}, err)
				return
			}
			if !yield(ev, nil) {
				return
			}
		}
		if err := sc.Err(); err != nil {
			yield(api.SetupEvent{}, err)
		}
	}, nil
}

// WaitReady polls GET /v1/version until the daemon answers or the timeout (or
// parent context) expires.
func (c *Client) WaitReady(ctx context.Context, timeout time.Duration) error {
	wctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(ReadyPollInterval)
	defer ticker.Stop()
	for {
		if c.Available(wctx) {
			return nil
		}
		select {
		case <-wctx.Done():
			return wctx.Err()
		case <-ticker.C:
		}
	}
}
