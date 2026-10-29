// Package calib learns how gmwd's measures relate, from what it has seen:
// how many weekly points a 5h point costs, and how many tokens a 5h point
// buys with each model.
package calib

import (
	"context"
	"sort"
	"time"

	"gmwd/internal/pacer"
	"gmwd/internal/store"
)

const (
	ratioWindow  = 7 * 24 * time.Hour
	tokensWindow = 14 * 24 * time.Hour
	// minModelPoints of 5h movement before tokens per point is trusted.
	minModelPoints = 0.03
)

// ModelRate is how many tokens one 5h point (1%) bought with a model.
type ModelRate struct {
	Model          string  `json:"model"`
	TokensPerPoint float64 `json:"tokensPerPoint"` // input+output+cache-write tokens per 1% of the 5h window
	Points         float64 `json:"points"`         // 5h movement measured over (0..1)
	Runs           int     `json:"runs"`
}

// Estimates is the current calibration.
type Estimates struct {
	Ratio pacer.RatioEstimate `json:"ratio"`
	// Models is sorted by model name; only models with enough data.
	Models []ModelRate `json:"models,omitempty"`
	// LimitHits counts rate-limited readings in the last week.
	LimitHits int `json:"limitHits"`
}

// Rate returns the tokens per point for a model, if known.
func (e Estimates) Rate(model string) (ModelRate, bool) {
	for _, m := range e.Models {
		if m.Model == model {
			return m, true
		}
	}
	return ModelRate{}, false
}

// Estimate computes the calibration from stored samples and runs.
func Estimate(ctx context.Context, st *store.Store, now time.Time) (Estimates, error) {
	var e Estimates
	samples, err := st.ListUsage(ctx, now.Add(-ratioWindow))
	if err != nil {
		return e, err
	}
	e.Ratio = pacer.EstimateRatioDetail(samples)
	for _, s := range samples {
		if s.Status == "rejected" {
			e.LimitHits++
		}
	}

	runs, err := st.ListRuns(ctx, "", 5000)
	if err != nil {
		return e, err
	}
	type acc struct {
		tokens, points float64
		runs           int
	}
	by := map[string]*acc{}
	for _, r := range runs {
		if r.Model == "" || r.EndedAt.IsZero() || r.WindowSpent <= 0 || r.StartedAt.Before(now.Add(-tokensWindow)) {
			continue
		}
		a := by[r.Model]
		if a == nil {
			a = &acc{}
			by[r.Model] = a
		}
		a.tokens += float64(r.Tokens.Input + r.Tokens.Output + r.Tokens.CacheCreate)
		a.points += r.WindowSpent
		a.runs++
	}
	for model, a := range by {
		if a.points < minModelPoints {
			continue
		}
		e.Models = append(e.Models, ModelRate{Model: model, TokensPerPoint: a.tokens / (a.points * 100), Points: a.points, Runs: a.runs})
	}
	sort.Slice(e.Models, func(i, j int) bool { return e.Models[i].Model < e.Models[j].Model })
	return e, nil
}
