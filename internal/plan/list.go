package plan

import (
	"context"
	"sort"
	"time"

	"solo-0014-zone-rollout-control/internal/domain"
)

// ListQuery is the combined plan list request.
//
// Filters are optional and combine with AND semantics:
//
//   - State matches the plan state exactly (the original single-state filter).
//   - Zone matches the canonical plan zone exactly.
//   - ActivatedAfter / ActivatedBefore bound plan.ActivatedAt inclusively;
//     only plans that have been activated can match an activation window.
//   - SerialMin / SerialMax bound the serial of the plan's latest revision;
//     plans without any revision never match a serial range.
//
// Order is fixed per request (newest_created or oldest_created) and is backed
// by the total order (CreatedAt, ID), so paging never repeats or skips an item
// inside one read snapshot.
type ListQuery struct {
	State           domain.PlanState
	Zone            string
	ActivatedAfter  *time.Time
	ActivatedBefore *time.Time
	SerialMin       uint32
	SerialMax       uint32
	Order           domain.PlanListOrder
	Limit           int
	Cursor          string
}

// extended reports whether the request uses the combined-filter list flow.
// Requests that only carry the original state filter (plus paging) stay on the
// legacy path so existing callers keep ID-ascending order and cursor format.
// A v2 cursor always selects the extended path, even if no other extended
// parameter is present.
func (q ListQuery) extended() bool {
	return q.Zone != "" ||
		q.ActivatedAfter != nil ||
		q.ActivatedBefore != nil ||
		q.SerialMin != 0 ||
		q.SerialMax != 0 ||
		q.Order != "" ||
		isCombinedCursor(q.Cursor)
}

func (q ListQuery) filter() domain.PlanListFilter {
	return domain.PlanListFilter{
		State:           q.State,
		Zone:            q.Zone,
		ActivatedAfter:  q.ActivatedAfter,
		ActivatedBefore: q.ActivatedBefore,
		SerialMin:       q.SerialMin,
		SerialMax:       q.SerialMax,
	}
}

func (s *Service) List(ctx context.Context, query ListQuery) (domain.PlanPage, error) {
	if query.Limit < 1 || query.Limit > 100 {
		return domain.PlanPage{}, domain.Validation("limit must be between 1 and 100", map[string]any{
			"limit": query.Limit,
		})
	}
	continuation, err := decodeCursor(query.Cursor)
	if err != nil {
		return domain.PlanPage{}, domain.Validation("cursor is invalid", nil)
	}

	if !query.extended() {
		return s.listLegacy(ctx, query, continuation)
	}
	return s.listCombined(ctx, query, continuation)
}

// listLegacy preserves the original list behavior: optional single-state
// filter, ascending plan ID order, and cursors that carry only a plan ID.
// New v2 cursors are rejected on this path with an explicit result.
func (s *Service) listLegacy(ctx context.Context, query ListQuery, continuation decodedCursor) (domain.PlanPage, error) {
	if continuation.combined != nil {
		return domain.PlanPage{}, errCursorDoesNotMatch
	}
	after := continuation.legacyID
	var page domain.PlanPage
	err := s.store.Read(ctx, func(state *domain.State) error {
		items := make([]domain.RolloutPlan, 0, len(state.Plans))
		for _, plan := range state.Plans {
			if query.State != "" && plan.State != query.State {
				continue
			}
			if after != "" && plan.ID <= after {
				continue
			}
			items = append(items, plan)
		}
		sort.Slice(items, func(i, j int) bool {
			return items[i].ID < items[j].ID
		})
		page.Meta.Limit = query.Limit
		page.Meta.Filter = domain.PlanListFilter{State: query.State}
		if len(items) > query.Limit {
			page.Items = append(page.Items, items[:query.Limit]...)
			page.Meta.NextCursor = encodeLegacyCursor(items[query.Limit-1].ID)
			page.Meta.HasMore = true
			return nil
		}
		page.Items = append(page.Items, items...)
		return nil
	})
	return page, err
}

// listCombined evaluates the full filter set under a fixed total order and
// resumes from a validated v2 cursor. Filtering, sorting and page slicing all
// happen inside one store.Read snapshot.
func (s *Service) listCombined(ctx context.Context, query ListQuery, continuation decodedCursor) (domain.PlanPage, error) {
	order := query.Order
	if order == "" {
		order = domain.PlanOrderNewest
	}

	var afterTime time.Time
	var afterID string
	switch {
	case continuation.combined != nil:
		if continuation.combined.Order != order ||
			!sameFilter(continuation.combined.Filter, query.filter()) {
			// The cursor belongs to a different list; continuing would mix
			// orders or filters. Fail explicitly instead of drifting.
			return domain.PlanPage{}, errCursorDoesNotMatch
		}
		afterTime, _ = time.Parse(time.RFC3339Nano, continuation.combined.LastCreatedAt)
		afterID = continuation.combined.LastID
	case continuation.legacyID != "":
		// A legacy cursor can only be replayed on the legacy flow, where its
		// ID-ascending order and single-state filter are defined.
		return domain.PlanPage{}, errCursorDoesNotMatch
	}

	filter := query.filter()
	var page domain.PlanPage
	err := s.store.Read(ctx, func(state *domain.State) error {
		items := make([]domain.RolloutPlan, 0, len(state.Plans))
		for _, plan := range state.Plans {
			if !matchesFilter(state, plan, filter) {
				continue
			}
			if afterID != "" && !afterPosition(plan, order, afterTime, afterID) {
				continue
			}
			items = append(items, plan)
		}
		sort.Slice(items, func(i, j int) bool {
			if !items[i].CreatedAt.Equal(items[j].CreatedAt) {
				if order == domain.PlanOrderNewest {
					return items[i].CreatedAt.After(items[j].CreatedAt)
				}
				return items[i].CreatedAt.Before(items[j].CreatedAt)
			}
			if order == domain.PlanOrderNewest {
				return items[i].ID > items[j].ID
			}
			return items[i].ID < items[j].ID
		})
		page.Meta.Limit = query.Limit
		page.Meta.Order = order
		page.Meta.Filter = filter
		if len(items) > query.Limit {
			page.Items = append(page.Items, items[:query.Limit]...)
			last := items[query.Limit-1]
			page.Meta.NextCursor = encodeListCursor(order, filter, last)
			page.Meta.HasMore = true
			return nil
		}
		page.Items = append(page.Items, items...)
		return nil
	})
	return page, err
}

// matchesFilter applies the AND-combined plan list filter against a plan in
// the given snapshot.
func matchesFilter(state *domain.State, plan domain.RolloutPlan, filter domain.PlanListFilter) bool {
	if filter.State != "" && plan.State != filter.State {
		return false
	}
	if filter.Zone != "" && plan.Zone != filter.Zone {
		return false
	}
	if filter.ActivatedAfter != nil || filter.ActivatedBefore != nil {
		if plan.ActivatedAt == nil {
			return false
		}
		activated := plan.ActivatedAt.UTC()
		if filter.ActivatedAfter != nil && activated.Before(filter.ActivatedAfter.UTC()) {
			return false
		}
		if filter.ActivatedBefore != nil && activated.After(filter.ActivatedBefore.UTC()) {
			return false
		}
	}
	if filter.SerialMin != 0 || filter.SerialMax != 0 {
		if plan.LatestRevisionID == "" {
			return false
		}
		revision, ok := state.Revisions[plan.LatestRevisionID]
		if !ok {
			return false
		}
		if filter.SerialMin != 0 && revision.Serial < filter.SerialMin {
			return false
		}
		if filter.SerialMax != 0 && revision.Serial > filter.SerialMax {
			return false
		}
	}
	return true
}

// afterPosition reports whether plan still belongs after the cursor position
// (afterTime, afterID) in the chosen total order. Plans strictly before or at
// the cursor position are reported as false so they are never repeated.
func afterPosition(plan domain.RolloutPlan, order domain.PlanListOrder, afterTime time.Time, afterID string) bool {
	if !plan.CreatedAt.Equal(afterTime) {
		if order == domain.PlanOrderNewest {
			return plan.CreatedAt.Before(afterTime)
		}
		return plan.CreatedAt.After(afterTime)
	}
	if order == domain.PlanOrderNewest {
		return plan.ID < afterID
	}
	return plan.ID > afterID
}

// sameFilter compares two normalized list filters for cursor validation.
func sameFilter(a, b domain.PlanListFilter) bool {
	if a.State != b.State || a.Zone != b.Zone ||
		a.SerialMin != b.SerialMin || a.SerialMax != b.SerialMax {
		return false
	}
	if !sameTime(a.ActivatedAfter, b.ActivatedAfter) || !sameTime(a.ActivatedBefore, b.ActivatedBefore) {
		return false
	}
	return true
}

func sameTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.UTC().Equal(b.UTC())
}
