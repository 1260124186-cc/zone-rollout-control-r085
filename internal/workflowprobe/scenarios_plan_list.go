package workflowprobe

import (
	"net/http"
	"net/url"
	"time"

	"solo-0014-zone-rollout-control/internal/domain"
)

type planListResponse struct {
	Items []domain.RolloutPlan `json:"items"`
	Meta  struct {
		Limit      int    `json:"limit"`
		NextCursor string `json:"next_cursor"`
		HasMore    bool   `json:"has_more"`
		Order      string `json:"order"`
		Filter     struct {
			State           string     `json:"state"`
			Zone            string     `json:"zone"`
			SerialMin       uint32     `json:"serial_min"`
			SerialMax       uint32     `json:"serial_max"`
			ActivatedAfter  *time.Time `json:"activated_after"`
			ActivatedBefore *time.Time `json:"activated_before"`
		} `json:"filter"`
	} `json:"meta"`
}

func listPlansRaw(h *harness, rawQuery string) (planListResponse, error) {
	var page planListResponse
	err := h.call(http.MethodGet, "/v1/plans"+rawQuery, nil, &page)
	return page, err
}

func createDraftPlan(h *harness, zone string) (domain.RolloutPlan, error) {
	var plan domain.RolloutPlan
	err := h.call(http.MethodPut, "/v1/plans", domain.PlanCreateInput{
		Zone:                zone,
		MinimumSuccessRatio: 0.8,
		MaximumLatencyMS:    100,
		ExpectedNodes:       1,
	}, &plan)
	return plan, err
}

func runPlanListBrowsing() error {
	h, err := newHarness()
	if err != nil {
		return err
	}
	defer h.close()

	// Five plans with distinct creation times:
	// A draft list-a @12:00, B draft list-b @12:01,
	// C active list-a serial 200 @12:02,
	// D active list-a serial 400 @12:03,
	// E draft list-a (no revision) @12:04.
	planA, err := createDraftPlan(h, "list-a.example")
	if err != nil {
		return err
	}
	h.clock.Advance(time.Minute)
	planB, err := createDraftPlan(h, "list-b.example")
	if err != nil {
		return err
	}
	h.clock.Advance(time.Minute)
	planC, _, err := createActivePlan(h, "list-a.example", 200, 1, 0.8, 100)
	if err != nil {
		return err
	}
	h.clock.Advance(time.Minute)
	planD, _, err := createActivePlan(h, "list-a.example", 400, 1, 0.8, 100)
	if err != nil {
		return err
	}
	h.clock.Advance(time.Minute)
	planE, err := createDraftPlan(h, "list-a.example")
	if err != nil {
		return err
	}

	// Legacy flow: no extended parameters keeps ID-ascending order and
	// paginates the whole set without duplicates or skips.
	legacyIDs, err := collectPlanIDs(h, "")
	if err != nil {
		return err
	}
	if err := assertSamePlans(legacyIDs, []string{planA.ID, planB.ID, planC.ID, planD.ID, planE.ID}, true); err != nil {
		return err
	}

	// Newest-created fixed order across pages.
	newestIDs, err := collectPlanIDs(h, "?order=newest_created&limit=2")
	if err != nil {
		return err
	}
	wantNewest := []string{planE.ID, planD.ID, planC.ID, planB.ID, planA.ID}
	if err := assertSamePlans(newestIDs, wantNewest, false); err != nil {
		return err
	}
	firstNewest, err := listPlansRaw(h, "?order=newest_created&limit=2")
	if err != nil {
		return err
	}
	if !firstNewest.Meta.HasMore || firstNewest.Meta.Order != string(domain.PlanOrderNewest) {
		return ensure(false, "newest page meta did not report has_more and order")
	}

	// Oldest-created fixed order.
	oldestIDs, err := collectPlanIDs(h, "?order=oldest_created&limit=3")
	if err != nil {
		return err
	}
	wantOldest := []string{planA.ID, planB.ID, planC.ID, planD.ID, planE.ID}
	if err := assertSamePlans(oldestIDs, wantOldest, false); err != nil {
		return err
	}

	// Combined filter: zone + active state + inclusive activation window +
	// latest-serial range isolates plan C.
	window := url.Values{}
	window.Set("zone", "LIST-A.EXAMPLE.")
	window.Set("state", "active")
	window.Set("activated_after", "2026-09-25T12:00:00Z")
	window.Set("activated_before", "2026-09-25T12:02:00Z")
	window.Set("serial_min", "100")
	window.Set("serial_max", "300")
	page, err := listPlansRaw(h, "?"+window.Encode())
	if err != nil {
		return err
	}
	if len(page.Items) != 1 || page.Items[0].ID != planC.ID {
		return ensure(false, "combined filter did not isolate the expected active plan")
	}
	if page.Meta.Filter.Zone != "list-a.example" || page.Meta.Filter.State != "active" ||
		page.Meta.Filter.SerialMin != 100 || page.Meta.Filter.SerialMax != 300 ||
		page.Meta.Filter.ActivatedAfter == nil || page.Meta.Filter.ActivatedBefore == nil {
		return ensure(false, "filter summary was not echoed back in page meta")
	}
	if page.Meta.HasMore {
		return ensure(false, "single-item final page reported more results")
	}

	// Serial range alone excludes draft plan E (no revision) and plan B.
	serialIDs, err := collectPlanIDs(h, "?zone=list-a.example&serial_min=100&serial_max=500&order=newest_created")
	if err != nil {
		return err
	}
	if err := assertSamePlans(serialIDs, []string{planD.ID, planC.ID}, false); err != nil {
		return err
	}

	// A v2 cursor is invalidated when the order or filter changes.
	cursor := firstNewest.Meta.NextCursor
	if _, err := listPlansRaw(h, "?order=oldest_created&limit=2&cursor="+url.QueryEscape(cursor)); err == nil {
		return ensure(false, "cursor continued after order changed")
	} else if err := expectFailure(err, http.StatusBadRequest, string(domain.CodeValidationFailed)); err != nil {
		return err
	}
	if _, err := listPlansRaw(h, "?order=newest_created&zone=list-b.example&cursor="+url.QueryEscape(cursor)); err == nil {
		return ensure(false, "cursor continued after filter changed")
	} else if err := expectFailure(err, http.StatusBadRequest, string(domain.CodeValidationFailed)); err != nil {
		return err
	}

	// A legacy cursor cannot seed the extended flow.
	legacyPage, err := listPlansRaw(h, "?limit=2")
	if err != nil {
		return err
	}
	_, err = listPlansRaw(h, "?zone=list-a.example&cursor="+url.QueryEscape(legacyPage.Meta.NextCursor))
	if err := expectFailure(err, http.StatusBadRequest, string(domain.CodeValidationFailed)); err != nil {
		return err
	}

	// Concurrent arrival: page over zone list-a, then a new plan arrives.
	// The old cursor continues deterministically (the new plan cannot appear
	// mid-walk), and a fresh restart surfaces it.
	arrivalQuery := "?zone=list-a.example&order=newest_created&limit=2"
	pageOne, err := listPlansRaw(h, arrivalQuery)
	if err != nil {
		return err
	}
	if err := assertSamePlans(idsOf(pageOne.Items), []string{planE.ID, planD.ID}, false); err != nil {
		return err
	}
	h.clock.Advance(time.Minute)
	planF, err := createDraftPlan(h, "list-a.example")
	if err != nil {
		return err
	}
	pageTwo, err := listPlansRaw(h, arrivalQuery+"&cursor="+url.QueryEscape(pageOne.Meta.NextCursor))
	if err != nil {
		return err
	}
	if err := assertSamePlans(idsOf(pageTwo.Items), []string{planC.ID, planA.ID}, false); err != nil {
		return err
	}
	restarted, err := listPlansRaw(h, arrivalQuery)
	if err != nil {
		return err
	}
	if len(restarted.Items) != 2 || restarted.Items[0].ID != planF.ID || restarted.Items[1].ID != planE.ID {
		return ensure(false, "fresh list did not surface the concurrently created plan first")
	}

	// Invalid query parameters are rejected with stable errors.
	for _, raw := range []string{
		"?order=created_at",
		"?activated_after=not-a-time",
		"?serial_min=300&serial_max=100",
		"?serial_min=0",
	} {
		if _, err := listPlansRaw(h, raw); err == nil {
			return ensure(false, "invalid list query was accepted: "+raw)
		} else if err := expectFailure(err, http.StatusBadRequest, string(domain.CodeValidationFailed)); err != nil {
			return err
		}
	}

	// Equal creation timestamps are broken by ID so paging still visits each
	// plan exactly once in newest order.
	tieOne, err := createDraftPlan(h, "tie.example")
	if err != nil {
		return err
	}
	tieTwo, err := createDraftPlan(h, "tie.example")
	if err != nil {
		return err
	}
	wantTie := []string{tieOne.ID, tieTwo.ID}
	if wantTie[0] < wantTie[1] {
		wantTie[0], wantTie[1] = wantTie[1], wantTie[0]
	}
	tieIDs, err := collectPlanIDs(h, "?zone=tie.example&order=newest_created&limit=1")
	if err != nil {
		return err
	}
	if err := assertSamePlans(tieIDs, wantTie, false); err != nil {
		return err
	}
	return nil
}

func collectPlanIDs(h *harness, rawQuery string) ([]string, error) {
	seen := make([]string, 0)
	separator := "?"
	if len(rawQuery) > 0 {
		separator = "&"
	}
	cursor := ""
	for {
		query := rawQuery
		if cursor != "" {
			query += separator + "cursor=" + url.QueryEscape(cursor)
		}
		page, err := listPlansRaw(h, query)
		if err != nil {
			return nil, err
		}
		for _, item := range page.Items {
			seen = append(seen, item.ID)
		}
		if !page.Meta.HasMore {
			if page.Meta.NextCursor != "" {
				return nil, ensure(false, "has_more=false with a next cursor")
			}
			return seen, nil
		}
		if page.Meta.NextCursor == "" {
			return nil, ensure(false, "has_more=true without a next cursor")
		}
		cursor = page.Meta.NextCursor
	}
}

func idsOf(plans []domain.RolloutPlan) []string {
	ids := make([]string, 0, len(plans))
	for _, plan := range plans {
		ids = append(ids, plan.ID)
	}
	return ids
}

// assertSamePlans compares two ordered ID sequences. When byID is true the
// expected slice is treated as an unordered set sorted by ID (legacy order).
func assertSamePlans(got []string, want []string, byID bool) error {
	if byID {
		want = append([]string(nil), want...)
		for i := 1; i < len(want); i++ {
			for j := i; j > 0 && want[j-1] > want[j]; j-- {
				want[j-1], want[j] = want[j], want[j-1]
			}
		}
	}
	if len(got) != len(want) {
		return ensure(false, "plan sequence length mismatch")
	}
	for i := range got {
		if got[i] != want[i] {
			return ensure(false, "plan sequence order or membership mismatch")
		}
	}
	return nil
}
