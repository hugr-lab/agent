package search

import (
	"sort"
	"time"
)

// applyRecencyRerank computes the combined relevance × recency score
// for every hit and reorders the slice in place.
//
//   combined = (1 − distance) × 1/(1 + age_hours / half_life_hours)
//
// distance is from the GraphQL semantic similarity (cosine, so
// 0..2). When distance is nil (recency-only or keyword path),
// (1 − distance) is treated as 1.0 so the recency boost is the
// sole sort key.
//
// age_hours is computed from the hit's created_at against `now`.
// half_life is converted to hours; zero half_life is treated as
// 1 hour (the mission default) to avoid divide-by-zero.
//
// Returns the truncated slice (caller's `limit`).
func applyRecencyRerank(hits []SearchHit, halfLife time.Duration, now time.Time, limit int) []SearchHit {
	hl := halfLife.Hours()
	if hl <= 0 {
		hl = 1
	}

	for i := range hits {
		h := &hits[i]
		ageHours := now.Sub(h.CreatedAt).Hours()
		if ageHours < 0 {
			ageHours = 0
		}
		recency := 1.0 / (1.0 + ageHours/hl)
		h.RecencyBoost = recency

		relevance := 1.0
		if h.Distance != nil {
			d := *h.Distance
			if d < 0 {
				d = 0
			}
			if d > 2 {
				d = 2 // cosine distance is bounded; clamp defensively
			}
			relevance = 1.0 - d
			if relevance < 0 {
				relevance = 0
			}
		}
		h.Combined = relevance * recency
	}

	sort.SliceStable(hits, func(i, j int) bool {
		return hits[i].Combined > hits[j].Combined
	})

	if limit > 0 && len(hits) > limit {
		hits = hits[:limit]
	}
	return hits
}

// applyRecencyOnly is the recency-only ordering (no semantic
// distance available, e.g. keyword fallback or no-query path).
// Identical math to applyRecencyRerank with relevance == 1.
func applyRecencyOnly(hits []SearchHit, halfLife time.Duration, now time.Time, limit int) []SearchHit {
	hl := halfLife.Hours()
	if hl <= 0 {
		hl = 1
	}
	for i := range hits {
		h := &hits[i]
		ageHours := now.Sub(h.CreatedAt).Hours()
		if ageHours < 0 {
			ageHours = 0
		}
		boost := 1.0 / (1.0 + ageHours/hl)
		h.RecencyBoost = boost
		h.Combined = boost
	}
	sort.SliceStable(hits, func(i, j int) bool {
		return hits[i].Combined > hits[j].Combined
	})
	if limit > 0 && len(hits) > limit {
		hits = hits[:limit]
	}
	return hits
}
