package materialise

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// HTTPProber polls a URL until it answers below 500 or the timeout expires. A
// connection refused early in a service's startup is expected, not fatal — the
// probe's job is to decide when the workspace is usable, so it retries until the
// deadline rather than failing on the first attempt.
type HTTPProber struct{}

func (HTTPProber) HTTP(url string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 5 * time.Second}
	var last error
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			cancel()
			return err
		}
		resp, err := client.Do(req)
		cancel()
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode < 500 {
				return nil
			}
			last = fmt.Errorf("%s answered %d", url, resp.StatusCode)
		} else {
			last = err
		}
		time.Sleep(250 * time.Millisecond)
	}
	if last == nil {
		last = fmt.Errorf("%s did not answer within %s", url, timeout)
	}
	return last
}
