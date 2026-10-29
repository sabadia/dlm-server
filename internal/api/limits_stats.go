package api

import (
	"context"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/sabadia/dlm-server/internal/service"
	"github.com/sabadia/dlm-server/internal/storage"
)

// OverrideLimitsBody is the request of set-limits-override.
type OverrideLimitsBody struct {
	SpeedLimit int64 `json:"speedLimit" minimum:"0" doc:"Global speed limit in bytes per second while the override lasts; 0 is unlimited (turbo)."`
	Minutes    int   `json:"minutes" minimum:"1" maximum:"1440" doc:"How long the override lasts, in minutes (1 to 1440); the configured limit applies again afterwards."`
}

// TimelineInput is the request of get-stats-timeline.
type TimelineInput struct {
	Bucket  string    `query:"bucket" enum:"hour,day" default:"hour" doc:"Size of a bucket (UTC)."`
	GroupBy string    `query:"groupBy" enum:"none,engine,queue" default:"none" doc:"Split every bucket by engine or by queue."`
	From    time.Time `query:"from" doc:"Start of the range, inclusive (RFC 3339). Default: 24 hours (bucket=hour) or 30 days (bucket=day) before the end."`
	To      time.Time `query:"to" doc:"End of the range, exclusive (RFC 3339). Default: the end of the current bucket."`
}

// TimelinePoint is the traffic of one bucket (and group).
type TimelinePoint struct {
	Time  time.Time `json:"time" doc:"Start of the bucket (UTC)."`
	Group string    `json:"group,omitempty" doc:"The engine or queue ID when groupBy is set."`
	Down  int64     `json:"down" doc:"Bytes downloaded."`
	Up    int64     `json:"up" doc:"Bytes uploaded (torrents)."`
}

// Timeline is the response of get-stats-timeline.
type Timeline struct {
	Bucket  string          `json:"bucket"`
	GroupBy string          `json:"groupBy"`
	From    time.Time       `json:"from"`
	To      time.Time       `json:"to"`
	Points  []TimelinePoint `json:"points" doc:"Oldest first; buckets without traffic are absent."`
}

func (s *Server) registerLimitsAndTimeline() {
	Register(s, huma.Operation{
		OperationID: "get-limits",
		Method:      http.MethodGet,
		Path:        "/v1/limits",
		Summary:     "Speed limits and quotas in force",
		Description: "The global speed limit now in force (and the configured one when a temporary override replaces it), the active speed profile, " +
			"the quota usage of the global scope, and the same per queue.",
		Tags:   []string{tagSettings},
		Errors: []int{http.StatusServiceUnavailable},
	}, s.getLimits)

	Register(s, huma.Operation{
		OperationID: "set-limits-override",
		Method:      http.MethodPost,
		Path:        "/v1/limits/override",
		Summary:     "Temporarily replace the global speed limit",
		Description: "Replaces the global speed limit by speedLimit (0: unlimited, \"turbo\") for 1 to 1440 minutes; the configured limit and profiles apply again afterwards. " +
			"A new override replaces the previous one. Clients are told with a limits.changed event, also when the override ends.",
		Tags:   []string{tagSettings},
		Errors: []int{http.StatusUnprocessableEntity, http.StatusServiceUnavailable},
	}, s.setLimitsOverride)

	Register(s, huma.Operation{
		OperationID: "clear-limits-override",
		Method:      http.MethodDelete,
		Path:        "/v1/limits/override",
		Summary:     "End the temporary speed limit",
		Description: "Ends a temporary global speed override now; without one it does nothing. Answers like get-limits.",
		Tags:        []string{tagSettings},
		Errors:      []int{http.StatusServiceUnavailable},
	}, s.clearLimitsOverride)

	Register(s, huma.Operation{
		OperationID: "get-stats-timeline",
		Method:      http.MethodGet,
		Path:        "/v1/stats/timeline",
		Summary:     "Traffic over time",
		Description: "Bytes downloaded and uploaded per hour or day (UTC), optionally split by engine or queue, for charts. " +
			"Recorded from when the daemon first measured it; at most 1000 buckets per request (422 otherwise).",
		Tags:   []string{tagTasks},
		Errors: []int{http.StatusUnprocessableEntity, http.StatusServiceUnavailable},
	}, s.getStatsTimeline)
}

func (s *Server) getLimits(ctx context.Context, _ *struct{}) (*struct{ Body *service.LimitsView }, error) {
	if s.limits == nil {
		return nil, errServicesUnavailable()
	}
	v, err := s.limits.Limits(ctx)
	if err != nil {
		return nil, s.fail(ctx, "get-limits", err, locBody)
	}
	return &struct{ Body *service.LimitsView }{Body: v}, nil
}

func (s *Server) setLimitsOverride(ctx context.Context, in *struct{ Body OverrideLimitsBody }) (*struct{ Body *service.LimitsView }, error) {
	if s.limits == nil {
		return nil, errServicesUnavailable()
	}
	v, err := s.limits.SetOverride(ctx, in.Body.SpeedLimit, time.Duration(in.Body.Minutes)*time.Minute)
	if err != nil {
		return nil, s.fail(ctx, "set-limits-override", err, locBody)
	}
	return &struct{ Body *service.LimitsView }{Body: v}, nil
}

func (s *Server) clearLimitsOverride(ctx context.Context, _ *struct{}) (*struct{ Body *service.LimitsView }, error) {
	if s.limits == nil {
		return nil, errServicesUnavailable()
	}
	v, err := s.limits.ClearOverride(ctx)
	if err != nil {
		return nil, s.fail(ctx, "clear-limits-override", err, locBody)
	}
	return &struct{ Body *service.LimitsView }{Body: v}, nil
}

func (s *Server) getStatsTimeline(ctx context.Context, in *TimelineInput) (*struct{ Body Timeline }, error) {
	if s.tasks == nil {
		return nil, errServicesUnavailable()
	}
	bucket := time.Hour
	if in.Bucket == "day" {
		bucket = 24 * time.Hour
	}
	to := in.To
	if to.IsZero() {
		to = time.Now().UTC().Truncate(bucket).Add(bucket)
	}
	from := in.From
	if from.IsZero() {
		if bucket == time.Hour {
			from = to.Add(-24 * time.Hour)
		} else {
			from = to.Add(-30 * 24 * time.Hour)
		}
	}
	group := storage.TransferGroup("")
	if in.GroupBy != "none" {
		group = storage.TransferGroup(in.GroupBy)
	}
	pts, err := s.tasks.Timeline(ctx, from, to, bucket, group)
	if err != nil {
		return nil, s.fail(ctx, "get-stats-timeline", err, "query")
	}
	out := Timeline{Bucket: in.Bucket, GroupBy: in.GroupBy, From: from.UTC(), To: to.UTC(), Points: make([]TimelinePoint, len(pts))}
	for i, p := range pts {
		out.Points[i] = TimelinePoint{Time: p.Bucket, Group: p.Group, Down: p.Down, Up: p.Up}
	}
	return &struct{ Body Timeline }{Body: out}, nil
}
