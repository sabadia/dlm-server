package client

import (
	"context"
	"net/http"
	"net/url"
	"time"
)

// SpeedOverride is a temporary replacement of the global speed limit.
type SpeedOverride struct {
	SpeedLimit int64     `json:"speedLimit"`
	Until      time.Time `json:"until"`
}

// QuotaView is the state of one quota.
type QuotaView struct {
	Bytes       int64      `json:"bytes"`
	Period      string     `json:"period"`
	Used        int64      `json:"used"`
	WindowStart *time.Time `json:"windowStart,omitempty"`
	ResetsAt    *time.Time `json:"resetsAt,omitempty"`
	Exhausted   bool       `json:"exhausted"`
}

// ActiveProfile names the speed profile in force.
type ActiveProfile struct {
	Index      int   `json:"index"`
	SpeedLimit int64 `json:"speedLimit"`
}

// ScopeLimits are the limits of the global scope or of one queue.
type ScopeLimits struct {
	QueueID              string         `json:"queueId,omitempty"`
	Name                 string         `json:"name,omitempty"`
	SpeedLimit           int64          `json:"speedLimit"`
	ConfiguredSpeedLimit int64          `json:"configuredSpeedLimit"`
	ActiveProfile        *ActiveProfile `json:"activeProfile,omitempty"`
	Quota                *QuotaView     `json:"quota,omitempty"`
}

// Limits is the answer of get-limits.
type Limits struct {
	Global   ScopeLimits    `json:"global"`
	Override *SpeedOverride `json:"override,omitempty"`
	Queues   []ScopeLimits  `json:"queues"`
}

// GetLimits calls get-limits (GET /v1/limits).
func (c *Client) GetLimits(ctx context.Context) (*Limits, error) {
	var out Limits
	if err := c.do(ctx, http.MethodGet, "/v1/limits", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SetLimitsOverride calls set-limits-override (POST /v1/limits/override):
// the global limit is speedLimit bytes per second (0: unlimited) for minutes.
func (c *Client) SetLimitsOverride(ctx context.Context, speedLimit int64, minutes int) (*Limits, error) {
	body := struct {
		SpeedLimit int64 `json:"speedLimit"`
		Minutes    int   `json:"minutes"`
	}{speedLimit, minutes}
	var out Limits
	if err := c.do(ctx, http.MethodPost, "/v1/limits/override", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ClearLimitsOverride calls clear-limits-override (DELETE /v1/limits/override).
func (c *Client) ClearLimitsOverride(ctx context.Context) (*Limits, error) {
	var out Limits
	if err := c.do(ctx, http.MethodDelete, "/v1/limits/override", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// TimelinePoint is the traffic of one bucket (and group).
type TimelinePoint struct {
	Time  time.Time `json:"time"`
	Group string    `json:"group,omitempty"`
	Down  int64     `json:"down"`
	Up    int64     `json:"up"`
}

// Timeline is the answer of get-stats-timeline.
type Timeline struct {
	Bucket  string          `json:"bucket"`
	GroupBy string          `json:"groupBy"`
	From    time.Time       `json:"from"`
	To      time.Time       `json:"to"`
	Points  []TimelinePoint `json:"points"`
}

// TimelineOptions are the parameters of StatsTimeline; empty values take the
// daemon's defaults.
type TimelineOptions struct {
	Bucket, GroupBy, From, To string
}

// StatsTimeline calls get-stats-timeline (GET /v1/stats/timeline).
func (c *Client) StatsTimeline(ctx context.Context, o TimelineOptions) (*Timeline, error) {
	q := url.Values{}
	for k, v := range map[string]string{"bucket": o.Bucket, "groupBy": o.GroupBy, "from": o.From, "to": o.To} {
		if v != "" {
			q.Set(k, v)
		}
	}
	path := "/v1/stats/timeline"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var out Timeline
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
