package channel

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// mediaHTTPClient follows only same-origin redirects. In particular, bearer
// tokens and bot tokens embedded in request paths must never reach another host.
func mediaHTTPClient(client *http.Client, origin *url.URL) *http.Client {
	if client == nil {
		client = http.DefaultClient
	}
	clone := *client
	prior := client.CheckRedirect
	clone.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if req.URL.Scheme != origin.Scheme || !strings.EqualFold(req.URL.Host, origin.Host) || req.URL.User != nil {
			return http.ErrUseLastResponse
		}
		if prior != nil {
			return prior(req, via)
		}
		if len(via) >= 10 {
			return fmt.Errorf("too many redirects")
		}
		return nil
	}
	return &clone
}

func checkMediaStatus(resp *http.Response) error {
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		resp.Body.Close()
		return fmt.Errorf("media request failed (HTTP %d)", resp.StatusCode)
	}
	return nil
}
