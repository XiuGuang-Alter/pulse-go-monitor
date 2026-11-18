package main

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

func historyParameter(query url.Values, key string, fallback int64) (int64, error) {
	values, exists := query[key]
	if !exists {
		return fallback, nil
	}
	if len(values) != 1 {
		return 0, fmt.Errorf("%w：%s只能填写一次", ErrValidation, key)
	}
	n, err := strconv.ParseInt(values[0], 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%w：%s须为非负整数", ErrValidation, key)
	}
	return n, nil
}

type historyQuery struct {
	targetID int64
	beforeID int64
	limit    int
}

func parseHistoryQuery(query url.Values) (historyQuery, error) {
	target, err := historyParameter(query, "target_id", 0)
	if err != nil {
		return historyQuery{}, err
	}
	before, err := historyParameter(query, "before_id", 0)
	if err != nil {
		return historyQuery{}, err
	}
	limit, err := historyParameter(query, "limit", 50)
	if err != nil {
		return historyQuery{}, err
	}
	if limit < 1 || limit > 200 {
		return historyQuery{}, fmt.Errorf("%w：limit为1–200", ErrValidation)
	}
	return historyQuery{targetID: target, beforeID: before, limit: int(limit)}, nil
}

func (a *App) alertEvents(w http.ResponseWriter, r *http.Request) {
	page, err := parseHistoryQuery(r.URL.Query())
	if err != nil {
		apiError(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), storeOperationTimeout)
	defer cancel()
	events, err := a.store.AlertEvents(ctx, page.targetID, page.limit, page.beforeID)
	if err != nil {
		apiError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, events)
}

func (a *App) incidentHistory(w http.ResponseWriter, r *http.Request) {
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || len(query["status"]) > 1 {
		apiError(w, ErrValidation)
		return
	}
	page, err := parseHistoryQuery(query)
	if err != nil {
		apiError(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), storeOperationTimeout)
	defer cancel()
	now := time.Now()
	items, err := a.store.Incidents(ctx, page.targetID, query.Get("status"), page.limit, page.beforeID, now)
	if err != nil {
		apiError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, struct {
		Items      []Incident `json:"items"`
		ObservedAt int64      `json:"observed_at"`
	}{items, now.UnixMilli()})
}
