package plan

import (
	"context"
	"sort"
	"strings"
	"time"

	"solo-0014-zone-rollout-control/internal/domain"
	"solo-0014-zone-rollout-control/internal/platform"
)

// listCursorVersion tags structured continuation cursors. Plain legacy cursors
// decode to a bare plan identifier and never carry this envelope.
const listCursorVersion = 2

// listCursor is the structured continuation token for the extended list flow.
// It pins the read snapshot and the exact ordered query, so a page can only
// resume against the same snapshot with the same filters and order.
type listCursor struct {
	Version         int              `json:"version"`
	Snapshot        uint64           `json:"snapshot"`
	Order           domain.PlanOrder `json:"order"`
	State           domain.PlanState `json:"state,omitempty"`
	Zone            string           `json:"zone,omitempty"`
	ActivatedAfter  *time.Time       `json:"activated_after,omitempty"`
	ActivatedBefore *time.Time       `json:"activated_before,omitempty"`
	SerialMin       *uint32          `json:"serial_min,omitempty"`
	SerialMax       *uint32          `json:"serial_max,omitempty"`
	// LastID is the tiebreaker anchor. LastCreatedAt additionally anchors the
	// created_asc and created_desc orders.
	LastID        string     `json:"last_id"`
	LastCreatedAt *time.Time `json:"last_created_at,omitempty"`
}

// List preserves the original list entry point: a single optional state filter
// on the default identifier order. It delegates to the extended query so both
// surfaces share the same snapshot, filtering and pagination code.
func (s *Service) List(
	ctx context.Context,
	stateFilter domain.PlanState,
	limit int,
	cursor string,
) (domain.PlanPage, error) {
	return s.ListByQuery(ctx, domain.PlanListQuery{
		State:  stateFilter,
		Order:  domain.PlanOrderIDAsc,
		Limit:  limit,
		Cursor: cursor,
	})
}

// ListByQuery returns a deterministic page of plans matching the query.
//
// The extended query (any filter beyond state, or an explicit created order)
// uses snapshot-pinned cursors: every page of one traversal reads the same
// store revision, so items are neither repeated nor skipped, and a cursor
// produced before concurrent writes fails explicitly as cursor_stale instead
// of silently resuming against the changed snapshot. The original state-only
// query on the default order keeps its legacy bare-ID cursor behavior.
func (s *Service) ListByQuery(
	ctx context.Context,
	query domain.PlanListQuery,
) (domain.PlanPage, error) {
	if query.Limit < 1 || query.Limit > 100 {
		return domain.PlanPage{}, domain.Validation("limit must be between 1 and 100", map[string]any{
			"limit": query.Limit,
		})
	}
	normalized, err := domain.NormalizePlanListQuery(query)
	if err != nil {
		return domain.PlanPage{}, err
	}
	legacy := isLegacyListQuery(normalized)

	var v1After string
	var anchor *listCursor
	if normalized.Cursor != "" {
		if legacy {
			after, decodeErr := platform.DecodeCursor(normalized.Cursor)
			if decodeErr != nil || strings.HasPrefix(after, "{") {
				return domain.PlanPage{}, domain.Validation("cursor is invalid", nil)
			}
			v1After = after
		} else {
			cursor, cursorErr := decodeListCursor(normalized.Cursor)
			if cursorErr != nil {
				return domain.PlanPage{}, cursorErr
			}
			if err := validateListCursor(cursor, normalized); err != nil {
				return domain.PlanPage{}, err
			}
			anchor = cursor
		}
	}

	var page domain.PlanPage
	err = s.store.Read(ctx, func(state *domain.State) error {
		if anchor != nil && state.Revision != anchor.Snapshot {
			return staleCursor(state.Revision, anchor.Snapshot)
		}
		items := make([]domain.RolloutPlan, 0, len(state.Plans))
		for _, plan := range state.Plans {
			if !matchesListFilter(plan, normalized, state) {
				continue
			}
			if v1After != "" && plan.ID <= v1After {
				continue
			}
			if anchor != nil && !afterAnchor(plan, normalized.Order, anchor) {
				continue
			}
			items = append(items, plan)
		}
		sortPlans(items, normalized.Order)

		page.Meta.Limit = normalized.Limit
		page.Meta.Order = normalized.Order
		page.Meta.Filter = normalized.FilterSummary()
		page.Meta.SnapshotRevision = state.Revision
		if len(items) > normalized.Limit {
			page.Items = append(page.Items, items[:normalized.Limit]...)
			page.Meta.HasMore = true
			last := items[normalized.Limit-1]
			cursor := buildListCursor(normalized, state.Revision, last)
			if legacy {
				page.Meta.NextCursor = platform.EncodeCursor(last.ID)
			} else {
				encoded, encodeErr := platform.EncodeJSONCursor(cursor)
				if encodeErr != nil {
					return domain.NewError(domain.CodeInternal, "could not encode cursor", nil)
				}
				page.Meta.NextCursor = encoded
			}
			return nil
		}
		page.Items = append(page.Items, items...)
		return nil
	})
	return page, err
}

// isLegacyListQuery reports whether the query uses only the original list
// surface: a single optional state filter, the default identifier order.
func isLegacyListQuery(query domain.PlanListQuery) bool {
	return query.Zone == "" &&
		query.ActivatedAfter == nil &&
		query.ActivatedBefore == nil &&
		query.SerialMin == nil &&
		query.SerialMax == nil &&
		query.Order == domain.PlanOrderIDAsc
}

func matchesListFilter(plan domain.RolloutPlan, query domain.PlanListQuery, state *domain.State) bool {
	if query.State != "" && plan.State != query.State {
		return false
	}
	if query.Zone != "" && plan.Zone != query.Zone {
		return false
	}
	if query.ActivatedAfter != nil || query.ActivatedBefore != nil {
		if plan.ActivatedAt == nil {
			return false
		}
		if query.ActivatedAfter != nil && plan.ActivatedAt.Before(*query.ActivatedAfter) {
			return false
		}
		if query.ActivatedBefore != nil && plan.ActivatedAt.After(*query.ActivatedBefore) {
			return false
		}
	}
	if query.SerialMin != nil || query.SerialMax != nil {
		if plan.LatestRevisionID == "" {
			return false
		}
		revision, ok := state.Revisions[plan.LatestRevisionID]
		if !ok {
			return false
		}
		if query.SerialMin != nil && revision.Serial < *query.SerialMin {
			return false
		}
		if query.SerialMax != nil && revision.Serial > *query.SerialMax {
			return false
		}
	}
	return true
}

// sortPlans applies the fixed order of each query. CreatedAt ties always fall
// back to plan ID so the order is total and stable across map iteration.
func sortPlans(items []domain.RolloutPlan, order domain.PlanOrder) {
	switch order {
	case domain.PlanOrderCreatedAsc:
		sort.Slice(items, func(i, j int) bool {
			if !items[i].CreatedAt.Equal(items[j].CreatedAt) {
				return items[i].CreatedAt.Before(items[j].CreatedAt)
			}
			return items[i].ID < items[j].ID
		})
	case domain.PlanOrderCreatedDesc:
		sort.Slice(items, func(i, j int) bool {
			if !items[i].CreatedAt.Equal(items[j].CreatedAt) {
				return items[i].CreatedAt.After(items[j].CreatedAt)
			}
			return items[i].ID > items[j].ID
		})
	default:
		sort.Slice(items, func(i, j int) bool {
			return items[i].ID < items[j].ID
		})
	}
}

// afterAnchor reports whether plan sits strictly after the cursor anchor in
// the query order. Matching the anchor exactly is excluded so replaying a
// cursor never repeats the last item of the previous page.
func afterAnchor(plan domain.RolloutPlan, order domain.PlanOrder, cursor *listCursor) bool {
	switch order {
	case domain.PlanOrderCreatedAsc:
		if plan.CreatedAt.Before(*cursor.LastCreatedAt) {
			return false
		}
		if plan.CreatedAt.After(*cursor.LastCreatedAt) {
			return true
		}
		return plan.ID > cursor.LastID
	case domain.PlanOrderCreatedDesc:
		if plan.CreatedAt.After(*cursor.LastCreatedAt) {
			return false
		}
		if plan.CreatedAt.Before(*cursor.LastCreatedAt) {
			return true
		}
		return plan.ID < cursor.LastID
	default:
		return plan.ID > cursor.LastID
	}
}

func buildListCursor(
	query domain.PlanListQuery,
	snapshot uint64,
	last domain.RolloutPlan,
) listCursor {
	cursor := listCursor{
		Version:         listCursorVersion,
		Snapshot:        snapshot,
		Order:           query.Order,
		State:           query.State,
		Zone:            query.Zone,
		ActivatedAfter:  query.ActivatedAfter,
		ActivatedBefore: query.ActivatedBefore,
		SerialMin:       query.SerialMin,
		SerialMax:       query.SerialMax,
		LastID:          last.ID,
	}
	if query.Order == domain.PlanOrderCreatedAsc || query.Order == domain.PlanOrderCreatedDesc {
		createdAt := last.CreatedAt.UTC()
		cursor.LastCreatedAt = &createdAt
	}
	return cursor
}

func decodeListCursor(raw string) (*listCursor, error) {
	var cursor listCursor
	if err := platform.DecodeJSONCursor(raw, &cursor); err != nil {
		return nil, invalidCursor("cursor is not a valid continuation token")
	}
	if cursor.Version != listCursorVersion || cursor.LastID == "" {
		return nil, invalidCursor("cursor is not a valid continuation token")
	}
	return &cursor, nil
}

// validateListCursor rejects cursors that cannot resume the supplied query:
// structured cursors embed their order and filter fingerprint, and only the
// exact same ordered query may continue. Snapshot matching is checked while
// holding the store read so it reflects the current revision.
func validateListCursor(cursor *listCursor, query domain.PlanListQuery) error {
	if cursor.Order != query.Order ||
		cursor.State != query.State ||
		cursor.Zone != query.Zone ||
		!timePointerEqual(cursor.ActivatedAfter, query.ActivatedAfter) ||
		!timePointerEqual(cursor.ActivatedBefore, query.ActivatedBefore) ||
		!uint32PointerEqual(cursor.SerialMin, query.SerialMin) ||
		!uint32PointerEqual(cursor.SerialMax, query.SerialMax) {
		return invalidCursor("cursor does not match the current filter and order")
	}
	return nil
}

func timePointerEqual(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}

func uint32PointerEqual(a, b *uint32) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func invalidCursor(message string) *domain.DomainError {
	return domain.NewError(domain.CodeCursorInvalid, message, nil)
}

func staleCursor(current uint64, expected uint64) *domain.DomainError {
	return domain.NewError(
		domain.CodeCursorStale,
		"cursor was created against an older snapshot; restart the listing without a cursor",
		map[string]any{
			"current_snapshot":  current,
			"expected_snapshot": expected,
		},
	)
}
