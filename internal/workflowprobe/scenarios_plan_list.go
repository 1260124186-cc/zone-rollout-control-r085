package workflowprobe

import (
	"net/http"
	"net/url"
	"time"

	"solo-0014-zone-rollout-control/internal/domain"
)

type planListPage struct {
	Items []domain.RolloutPlan `json:"items"`
	Meta  struct {
		Limit            int    `json:"limit"`
		NextCursor       string `json:"next_cursor"`
		HasMore          bool   `json:"has_more"`
		Order            string `json:"order"`
		SnapshotRevision uint64 `json:"snapshot_revision"`
		Filter           struct {
			State           string     `json:"state"`
			Zone            string     `json:"zone"`
			SerialMin       *uint32    `json:"serial_min"`
			SerialMax       *uint32    `json:"serial_max"`
			ActivatedAfter  *time.Time `json:"activated_after"`
			ActivatedBefore *time.Time `json:"activated_before"`
		} `json:"filter"`
	} `json:"meta"`
}

func listPlansPage(h *harness, query string) (planListPage, error) {
	var page planListPage
	err := h.call(http.MethodGet, "/v1/plans?"+query, nil, &page)
	return page, err
}

func createDraftPlan(h *harness, zone string) (domain.RolloutPlan, error) {
	var plan domain.RolloutPlan
	err := h.call(http.MethodPut, "/v1/plans", domain.PlanCreateInput{
		Zone:                zone,
		MinimumSuccessRatio: 0.5,
		MaximumLatencyMS:    1000,
		ExpectedNodes:       2,
	}, &plan)
	return plan, err
}

func addPlanRevision(h *harness, planID string, serial uint32) error {
	return h.call(http.MethodPatch, "/v1/plans/"+planID+"/revisions", domain.RevisionCreateInput{
		Serial:    serial,
		Authority: "ns1.example",
		Entries: []domain.ZoneEntry{
			{Owner: "example", Type: domain.EntryA, Value: "192.0.2.10"},
		},
	}, nil)
}

func activatePlan(h *harness, planID string, version uint64) (domain.RolloutPlan, error) {
	var plan domain.RolloutPlan
	err := h.call(http.MethodPatch, "/v1/plans/"+planID+"/activate", map[string]any{
		"expected_version": version,
	}, &plan)
	return plan, err
}

func completePlan(h *harness, planID string, version uint64) (domain.RolloutPlan, error) {
	var plan domain.RolloutPlan
	err := h.call(http.MethodPatch, "/v1/plans/"+planID+"/complete", map[string]any{
		"expected_version": version,
	}, &plan)
	return plan, err
}

func runPlanListQuery() error {
	h, err := newHarness()
	if err != nil {
		return err
	}
	defer h.close()

	// Five plans across two zones, three states, distinct revision serials and
	// creation times: A 12:00 active/100, B 12:01 draft/200, C 12:02 active/300,
	// D 12:03 completed/400, E 12:04 draft without revision.
	planA, err := createDraftPlan(h, "alpha.example")
	if err != nil {
		return err
	}
	h.clock.Advance(time.Minute)
	if _, err := createDraftPlan(h, "beta.example"); err != nil {
		return err
	}
	h.clock.Advance(time.Minute)
	planC, err := createDraftPlan(h, "alpha.example")
	if err != nil {
		return err
	}
	h.clock.Advance(time.Minute)
	planD, err := createDraftPlan(h, "alpha.example")
	if err != nil {
		return err
	}
	h.clock.Advance(time.Minute)
	if _, err := createDraftPlan(h, "alpha.example"); err != nil {
		return err
	}
	h.clock.Advance(time.Minute)

	if err := addPlanRevision(h, planA.ID, 100); err != nil {
		return err
	}
	if planA, err = activatePlan(h, planA.ID, 2); err != nil {
		return err
	}
	h.clock.Advance(time.Minute)
	if err := addPlanRevision(h, planC.ID, 300); err != nil {
		return err
	}
	h.clock.Advance(time.Minute)
	if planC, err = activatePlan(h, planC.ID, 2); err != nil {
		return err
	}
	h.clock.Advance(time.Minute)
	if err := addPlanRevision(h, planD.ID, 400); err != nil {
		return err
	}
	if _, err = activatePlan(h, planD.ID, 2); err != nil {
		return err
	}
	if planD, err = completePlan(h, planD.ID, 3); err != nil {
		return err
	}

	// Combined filters: zone + activation window + serial range + state.
	values := url.Values{}
	values.Set("zone", "alpha.example")
	values.Set("state", "completed")
	values.Set("activated_after", "2026-09-25T12:06:00Z")
	values.Set("activated_before", "2026-09-25T12:08:00Z")
	values.Set("serial_min", "350")
	values.Set("serial_max", "500")
	page, err := listPlansPage(h, values.Encode())
	if err != nil {
		return err
	}
	if len(page.Items) != 1 || page.Items[0].ID != planD.ID {
		return ensure(false, "combined filter did not return exactly the completed alpha plan")
	}
	if page.Meta.Order != string(domain.PlanOrderIDAsc) {
		return ensure(false, "default order was not echoed")
	}
	if page.Meta.HasMore || page.Meta.NextCursor != "" {
		return ensure(false, "single-item page claims more results")
	}
	if page.Meta.Filter.Zone != "alpha.example" || page.Meta.Filter.State != "completed" {
		return ensure(false, "zone/state filter summary was not echoed")
	}
	if page.Meta.Filter.SerialMin == nil || *page.Meta.Filter.SerialMin != 350 ||
		page.Meta.Filter.SerialMax == nil || *page.Meta.Filter.SerialMax != 500 {
		return ensure(false, "serial filter summary was not echoed")
	}
	if page.Meta.Filter.ActivatedAfter == nil ||
		!page.Meta.Filter.ActivatedAfter.Equal(time.Date(2026, time.September, 25, 12, 6, 0, 0, time.UTC)) {
		return ensure(false, "activated_after filter summary was not echoed")
	}
	if page.Meta.SnapshotRevision == 0 {
		return ensure(false, "snapshot revision was not echoed")
	}
	snapshot := page.Meta.SnapshotRevision

	// Fixed created_desc order with cursor pagination: no repeats or skips in
	// the snapshot, and has_more flips to false on the final page.
	values = url.Values{}
	values.Set("zone", "alpha.example")
	values.Set("order", "created_desc")
	values.Set("limit", "2")
	page1, err := listPlansPage(h, values.Encode())
	if err != nil {
		return err
	}
	if len(page1.Items) != 2 || !page1.Meta.HasMore || page1.Meta.NextCursor == "" {
		return ensure(false, "first created_desc page was not paginated")
	}
	if page1.Items[0].CreatedAt.Before(page1.Items[1].CreatedAt) {
		return ensure(false, "created_desc order is not descending")
	}
	if page1.Meta.Order != "created_desc" || page1.Meta.SnapshotRevision != snapshot {
		return ensure(false, "first page metadata does not match query")
	}
	values.Set("cursor", page1.Meta.NextCursor)
	page2, err := listPlansPage(h, values.Encode())
	if err != nil {
		return err
	}
	if len(page2.Items) != 2 || page2.Meta.HasMore || page2.Meta.NextCursor != "" {
		return ensure(false, "second created_desc page was not the final page")
	}
	seen := map[string]bool{}
	for _, item := range append(page1.Items, page2.Items...) {
		if seen[item.ID] {
			return ensure(false, "cursor pagination repeated a plan")
		}
		seen[item.ID] = true
	}
	if len(seen) != 4 {
		return ensure(false, "cursor pagination skipped or duplicated plans")
	}
	if page2.Meta.SnapshotRevision != snapshot {
		return ensure(false, "cursor changed the read snapshot")
	}

	// A concurrent write invalidates the old cursor explicitly.
	staleCursor := page1.Meta.NextCursor
	h.clock.Advance(time.Minute)
	if _, err := createDraftPlan(h, "alpha.example"); err != nil {
		return err
	}
	values.Set("cursor", staleCursor)
	if _, err := listPlansPage(h, values.Encode()); err == nil {
		return ensure(false, "stale cursor was accepted after a concurrent write")
	} else if err := expectFailure(err, http.StatusConflict, string(domain.CodeCursorStale)); err != nil {
		return err
	}

	// Cursor cannot resume a different filter or order.
	fresh := url.Values{}
	fresh.Set("zone", "alpha.example")
	fresh.Set("order", "created_desc")
	fresh.Set("limit", "2")
	freshPage, err := listPlansPage(h, fresh.Encode())
	if err != nil {
		return err
	}
	mismatch := url.Values{}
	mismatch.Set("zone", "beta.example")
	mismatch.Set("order", "created_desc")
	mismatch.Set("limit", "2")
	mismatch.Set("cursor", freshPage.Meta.NextCursor)
	if _, err := listPlansPage(h, mismatch.Encode()); err != nil {
		if err := expectFailure(err, http.StatusBadRequest, string(domain.CodeCursorInvalid)); err != nil {
			return err
		}
	} else {
		return ensure(false, "cursor was accepted with a changed filter")
	}
	mismatch = url.Values{}
	mismatch.Set("zone", "alpha.example")
	mismatch.Set("order", "created_asc")
	mismatch.Set("limit", "2")
	mismatch.Set("cursor", freshPage.Meta.NextCursor)
	if _, err := listPlansPage(h, mismatch.Encode()); err != nil {
		if err := expectFailure(err, http.StatusBadRequest, string(domain.CodeCursorInvalid)); err != nil {
			return err
		}
	} else {
		return ensure(false, "cursor was accepted with a changed order")
	}

	// Legacy surface: default order, bare cursors, stable plan objects.
	legacyIDs := map[string]bool{}
	legacyQuery := "limit=2"
	for {
		legacy, err := listPlansPage(h, legacyQuery)
		if err != nil {
			return err
		}
		if legacy.Meta.Order != string(domain.PlanOrderIDAsc) {
			return ensure(false, "default order is not identifier ascending")
		}
		for index := 1; index < len(legacy.Items); index++ {
			if legacy.Items[index-1].ID >= legacy.Items[index].ID {
				return ensure(false, "legacy identifier order is not ascending")
			}
		}
		for _, item := range legacy.Items {
			if legacyIDs[item.ID] {
				return ensure(false, "legacy pagination repeated a plan")
			}
			legacyIDs[item.ID] = true
		}
		if !legacy.Meta.HasMore {
			if legacy.Meta.NextCursor != "" {
				return ensure(false, "final page still carries a cursor")
			}
			break
		}
		legacyQuery = "limit=2&cursor=" + url.QueryEscape(legacy.Meta.NextCursor)
	}
	if len(legacyIDs) != 6 {
		return ensure(false, "legacy listing did not cover every plan exactly once")
	}
	activePage, err := listPlansPage(h, "state=active")
	if err != nil {
		return err
	}
	activeIDs := map[string]bool{}
	for _, item := range activePage.Items {
		activeIDs[item.ID] = true
		if item.State != domain.PlanActive || item.LatestRevisionID == "" {
			return ensure(false, "legacy plan objects lost compatibility fields")
		}
	}
	if len(activeIDs) != 2 || !activeIDs[planA.ID] || !activeIDs[planC.ID] {
		return ensure(false, "legacy state filter did not return both active plans")
	}

	// Invalid parameters are rejected without touching state.
	for _, raw := range []string{
		"order=bogus",
		"state=bogus",
		"serial_min=abc",
		"serial_min=0",
		"serial_min=500&serial_max=100",
		"activated_after=not-a-time",
		"activated_after=2026-09-25T12:10:00Z&activated_before=2026-09-25T12:09:00Z",
		"limit=0",
		"limit=101",
		"zone=bad..example",
		"cursor=!!!not-a-cursor!!!",
	} {
		if _, err := listPlansPage(h, raw); err == nil {
			return ensure(false, "invalid list parameter was accepted: "+raw)
		} else if err := expectFailure(err, http.StatusBadRequest, string(domain.CodeValidationFailed)); err != nil {
			return err
		}
	}
	return nil
}
