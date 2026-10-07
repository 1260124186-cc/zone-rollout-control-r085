package plan

import (
	"encoding/json"
	"time"

	"solo-0014-zone-rollout-control/internal/domain"
	"solo-0014-zone-rollout-control/internal/platform"
)

// errCursorInvalid marks a malformed or unparseable continuation token.
var errCursorInvalid = domain.Validation("cursor is invalid", nil)

// errCursorDoesNotMatch marks a cursor that is well-formed but cannot continue
// the current list, for example because the filter or order changed. The old
// cursor is therefore explicitly invalidated rather than silently drifting.
var errCursorDoesNotMatch = domain.Validation("cursor does not match the current filter or order", map[string]any{
	"cursor": "restart without a cursor",
})

// encodeLegacyCursor keeps the original plain-ID cursor format for the
// backward-compatible single-state list.
func encodeLegacyCursor(id string) string {
	return platform.EncodeCursor(id)
}

// isCombinedCursor reports whether raw is a v2 combined-filter cursor without
// treating malformed tokens as an error.
func isCombinedCursor(raw string) bool {
	if raw == "" {
		return false
	}
	decoded, err := platform.DecodeCursor(raw)
	if err != nil {
		return false
	}
	var combined listCursor
	return json.Unmarshal([]byte(decoded), &combined) == nil && combined.Version == listCursorVersion
}

// listCursorVersion marks cursors produced by the combined-filter list flow.
// Legacy cursors are plain base64-encoded plan IDs and are recognized by the
// absence of this prefix.
const listCursorVersion = "plan-list-v2"

// listCursor is the opaque continuation of a combined-filter plan list. It
// pins the evaluated filter and order together with the last returned plan's
// total-order position. A cursor is only valid when replayed against the exact
// same normalized filter and order; otherwise the caller receives an explicit
// validation error instead of silently skipping or duplicating items.
type listCursor struct {
	Version string                `json:"v"`
	Order   domain.PlanListOrder  `json:"order"`
	Filter  domain.PlanListFilter `json:"filter"`
	// LastCreatedAt / LastID identify the last plan handed out. CreatedAt is
	// stored as an RFC3339 string so the cursor serialization stays exact.
	LastCreatedAt string `json:"last_created_at"`
	LastID        string `json:"last_id"`
}

func encodeListCursor(order domain.PlanListOrder, filter domain.PlanListFilter, last domain.RolloutPlan) string {
	cursor := listCursor{
		Version:       listCursorVersion,
		Order:         order,
		Filter:        filter,
		LastCreatedAt: last.CreatedAt.UTC().Format(time.RFC3339Nano),
		LastID:        last.ID,
	}
	payload, _ := json.Marshal(cursor)
	return platform.EncodeCursor(string(payload))
}

// decodedCursor is the parsed continuation of either the new combined-filter
// list flow or the legacy single-state list.
type decodedCursor struct {
	combined *listCursor
	// legacyID holds the last plan ID for the original ID-ascending list.
	legacyID string
}

func decodeCursor(raw string) (decodedCursor, error) {
	decoded, err := platform.DecodeCursor(raw)
	if err != nil {
		return decodedCursor{}, errCursorInvalid
	}
	if decoded == "" {
		return decodedCursor{}, nil
	}
	var combined listCursor
	if json.Unmarshal([]byte(decoded), &combined) == nil && combined.Version == listCursorVersion {
		if _, err := time.Parse(time.RFC3339Nano, combined.LastCreatedAt); err != nil {
			return decodedCursor{}, errCursorInvalid
		}
		if combined.LastID == "" {
			return decodedCursor{}, errCursorInvalid
		}
		return decodedCursor{combined: &combined}, nil
	}
	// Legacy cursors carry only a plan ID.
	return decodedCursor{legacyID: decoded}, nil
}
