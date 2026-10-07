package httpapi

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"solo-0014-zone-rollout-control/internal/domain"
	"solo-0014-zone-rollout-control/internal/plan"
)

type versionInput struct {
	ExpectedVersion uint64 `json:"expected_version"`
}

func (s *Server) createPlan(w http.ResponseWriter, r *http.Request) {
	var input domain.PlanCreateInput
	if err := decode(w, r, &input); err != nil {
		sendError(w, r, err)
		return
	}
	created, err := s.plans.Create(r.Context(), input)
	if err != nil {
		sendError(w, r, err)
		return
	}
	sendData(w, r, http.StatusCreated, created)
}

func (s *Server) listPlans(w http.ResponseWriter, r *http.Request) {
	query := plan.ListQuery{Cursor: r.URL.Query().Get("cursor")}
	query.Limit = 50
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			sendError(w, r, domain.Validation("limit must be an integer", map[string]any{"limit": raw}))
			return
		}
		query.Limit = parsed
	}

	state := domain.PlanState(strings.TrimSpace(r.URL.Query().Get("state")))
	if state != "" && state != domain.PlanDraft && state != domain.PlanActive && state != domain.PlanCompleted {
		sendError(w, r, domain.Validation("plan state is not supported", map[string]any{"state": state}))
		return
	}
	query.State = state

	if raw := strings.TrimSpace(r.URL.Query().Get("zone")); raw != "" {
		zone, err := domain.NormalizeDNSName(raw)
		if err != nil {
			sendError(w, r, err)
			return
		}
		query.Zone = zone
	}

	var invalidParams map[string]any
	addInvalid := func(name string, message string) {
		if invalidParams == nil {
			invalidParams = make(map[string]any)
		}
		invalidParams[name] = message
	}

	if raw := strings.TrimSpace(r.URL.Query().Get("activated_after")); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			addInvalid("activated_after", "must be an RFC3339 timestamp")
		} else {
			query.ActivatedAfter = &parsed
		}
	}
	if raw := strings.TrimSpace(r.URL.Query().Get("activated_before")); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			addInvalid("activated_before", "must be an RFC3339 timestamp")
		} else {
			query.ActivatedBefore = &parsed
		}
	}

	parseSerial := func(name string) uint32 {
		raw := strings.TrimSpace(r.URL.Query().Get(name))
		if raw == "" {
			return 0
		}
		parsed, err := strconv.ParseUint(raw, 10, 32)
		if err != nil || parsed == 0 {
			addInvalid(name, "must be a serial between 1 and 4294967295")
			return 0
		}
		return uint32(parsed)
	}
	query.SerialMin = parseSerial("serial_min")
	query.SerialMax = parseSerial("serial_max")

	if raw := strings.TrimSpace(r.URL.Query().Get("order")); raw != "" {
		switch domain.PlanListOrder(raw) {
		case domain.PlanOrderNewest, domain.PlanOrderOldest:
			query.Order = domain.PlanListOrder(raw)
		default:
			addInvalid("order", "must be newest_created or oldest_created")
		}
	}

	if invalidParams != nil {
		sendError(w, r, domain.Validation("plan list query is invalid", invalidParams))
		return
	}
	if query.ActivatedAfter != nil && query.ActivatedBefore != nil &&
		query.ActivatedAfter.After(*query.ActivatedBefore) {
		sendError(w, r, domain.Validation("plan list query is invalid", map[string]any{
			"activated_after": "must not be later than activated_before",
		}))
		return
	}
	if query.SerialMin != 0 && query.SerialMax != 0 && query.SerialMin > query.SerialMax {
		sendError(w, r, domain.Validation("plan list query is invalid", map[string]any{
			"serial_min": "must not be greater than serial_max",
		}))
		return
	}

	page, err := s.plans.List(r.Context(), query)
	if err != nil {
		sendError(w, r, err)
		return
	}
	sendData(w, r, http.StatusOK, page)
}

func (s *Server) getPlan(w http.ResponseWriter, r *http.Request) {
	result, err := s.plans.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		sendError(w, r, err)
		return
	}
	sendData(w, r, http.StatusOK, result)
}

func (s *Server) addRevision(w http.ResponseWriter, r *http.Request) {
	var input domain.RevisionCreateInput
	if err := decode(w, r, &input); err != nil {
		sendError(w, r, err)
		return
	}
	result, err := s.plans.AddRevision(r.Context(), r.PathValue("id"), input)
	if err != nil {
		sendError(w, r, err)
		return
	}
	sendData(w, r, http.StatusCreated, result)
}

func (s *Server) activatePlan(w http.ResponseWriter, r *http.Request) {
	var input versionInput
	if err := decode(w, r, &input); err != nil {
		sendError(w, r, err)
		return
	}
	if input.ExpectedVersion == 0 {
		sendError(w, r, domain.Validation("expected_version must be greater than zero", nil))
		return
	}
	result, err := s.plans.Activate(r.Context(), r.PathValue("id"), input.ExpectedVersion)
	if err != nil {
		sendError(w, r, err)
		return
	}
	sendData(w, r, http.StatusOK, result)
}

func (s *Server) completePlan(w http.ResponseWriter, r *http.Request) {
	var input versionInput
	if err := decode(w, r, &input); err != nil {
		sendError(w, r, err)
		return
	}
	if input.ExpectedVersion == 0 {
		sendError(w, r, domain.Validation("expected_version must be greater than zero", nil))
		return
	}
	result, err := s.plans.Complete(r.Context(), r.PathValue("id"), input.ExpectedVersion)
	if err != nil {
		sendError(w, r, err)
		return
	}
	sendData(w, r, http.StatusOK, result)
}
