package main

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
)

func (a *App) alertEvents(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	read := func(key string, fallback int64) (int64, error) {
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
	target, err := read("target_id", 0)
	if err != nil {
		apiError(w, err)
		return
	}
	before, err := read("before_id", 0)
	if err != nil {
		apiError(w, err)
		return
	}
	limit, err := read("limit", 50)
	if err != nil {
		apiError(w, err)
		return
	}
	if limit < 1 || limit > 200 {
		apiError(w, ErrValidation)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), storeOperationTimeout)
	defer cancel()
	events, err := a.store.AlertEvents(ctx, target, int(limit), before)
	if err != nil {
		apiError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, events)
}
