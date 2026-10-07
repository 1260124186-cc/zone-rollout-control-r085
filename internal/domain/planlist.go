package domain

import (
	"strings"
	"time"
)

// PlanOrder selects the fixed traversal order used by plan list queries.
type PlanOrder string

const (
	// PlanOrderIDAsc preserves the original list order: plan identifier ascending.
	PlanOrderIDAsc PlanOrder = "id_asc"
	// PlanOrderCreatedAsc walks plans from the earliest created to the most recent.
	PlanOrderCreatedAsc PlanOrder = "created_asc"
	// PlanOrderCreatedDesc walks plans from the most recently created to the earliest.
	PlanOrderCreatedDesc PlanOrder = "created_desc"
)

// PlanListQuery is the full set of filters accepted by the plan list flow.
// Zero-valued pointers and empty strings mean the corresponding filter is absent.
type PlanListQuery struct {
	State           PlanState
	Zone            string
	ActivatedAfter  *time.Time
	ActivatedBefore *time.Time
	SerialMin       *uint32
	SerialMax       *uint32
	Order           PlanOrder
	Limit           int
	Cursor          string
}

// PlanFilterSummary echoes the effective, normalized filters applied to a page.
// Absent filters remain nil so callers can distinguish "unset" from zero values.
type PlanFilterSummary struct {
	State           PlanState  `json:"state,omitempty"`
	Zone            string     `json:"zone,omitempty"`
	ActivatedAfter  *time.Time `json:"activated_after,omitempty"`
	ActivatedBefore *time.Time `json:"activated_before,omitempty"`
	SerialMin       *uint32    `json:"serial_min,omitempty"`
	SerialMax       *uint32    `json:"serial_max,omitempty"`
}

// FilterSummary returns the effective filter summary for the normalized query.
func (q PlanListQuery) FilterSummary() PlanFilterSummary {
	return PlanFilterSummary{
		State:           q.State,
		Zone:            q.Zone,
		ActivatedAfter:  q.ActivatedAfter,
		ActivatedBefore: q.ActivatedBefore,
		SerialMin:       q.SerialMin,
		SerialMax:       q.SerialMax,
	}
}

// NormalizePlanListQuery canonicalizes filters and validates their values and
// combinations. The returned query uses only canonical, comparable values that
// are also embedded into continuation cursors.
func NormalizePlanListQuery(query PlanListQuery) (PlanListQuery, error) {
	details := make(map[string]any)
	switch query.State {
	case "", PlanDraft, PlanActive, PlanCompleted:
	default:
		details["state"] = "must be draft, active, or completed"
	}
	if query.Zone != "" {
		query.Zone = canonicalName(query.Zone)
		if err := validateCanonicalName(query.Zone); err != nil {
			details["zone"] = err.Error()
		}
	}
	if query.ActivatedAfter != nil {
		normalized := query.ActivatedAfter.UTC()
		query.ActivatedAfter = &normalized
	}
	if query.ActivatedBefore != nil {
		normalized := query.ActivatedBefore.UTC()
		query.ActivatedBefore = &normalized
	}
	if query.ActivatedAfter != nil && query.ActivatedBefore != nil &&
		query.ActivatedAfter.After(*query.ActivatedBefore) {
		details["activated_after"] = "must not be later than activated_before"
	}
	if query.SerialMin != nil && *query.SerialMin == 0 {
		details["serial_min"] = "must be greater than zero"
	}
	if query.SerialMax != nil && *query.SerialMax == 0 {
		details["serial_max"] = "must be greater than zero"
	}
	if query.SerialMin != nil && query.SerialMax != nil && *query.SerialMin > *query.SerialMax {
		details["serial_min"] = "must not be greater than serial_max"
	}
	switch query.Order {
	case "":
		query.Order = PlanOrderIDAsc
	case PlanOrderIDAsc, PlanOrderCreatedAsc, PlanOrderCreatedDesc:
	default:
		details["order"] = "must be id_asc, created_asc, or created_desc"
	}
	query.Zone = strings.TrimSpace(query.Zone)
	query.Cursor = strings.TrimSpace(query.Cursor)
	if len(details) > 0 {
		return PlanListQuery{}, Validation("plan list query is invalid", details)
	}
	return query, nil
}
