package remote

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"cubeherd/internal/api"
)

func query(from, to time.Time, points int) string {
	return "?from=" + strconv.FormatInt(from.Unix(), 10) + "&to=" + strconv.FormatInt(to.Unix(), 10) + "&points=" + strconv.Itoa(points)
}

func (c *Client) MetricsStatus(ctx context.Context) (api.MetricsStatus, error) {
	var s api.MetricsStatus
	return s, c.do(ctx, http.MethodGet, "/v1/metrics/status", nil, &s)
}

func (c *Client) NodeHistory(ctx context.Context, from, to time.Time, points int) (api.History, error) {
	var h api.History
	return h, c.do(ctx, http.MethodGet, "/v1/metrics/node"+query(from, to, points), nil, &h)
}

func (c *Client) ServerHistory(ctx context.Context, id string, from, to time.Time, points int) (api.History, error) {
	var h api.History
	return h, c.do(ctx, http.MethodGet, "/v1/metrics/servers/"+url.PathEscape(id)+query(from, to, points), nil, &h)
}

func (c *Client) MetricsLatest(ctx context.Context) (map[string]api.MetricPoint, error) {
	m := map[string]api.MetricPoint{}
	return m, c.do(ctx, http.MethodGet, "/v1/metrics/latest", nil, &m)
}

func (c *Client) PurgeMetrics(ctx context.Context, scope string) error {
	path := "/v1/metrics"
	if scope != "" {
		path += "/" + url.PathEscape(scope)
	}
	return c.do(ctx, http.MethodDelete, path, nil, nil)
}
